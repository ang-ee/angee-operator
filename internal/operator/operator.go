package operator

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/ang-ee/angee-operator/api"
	opgql "github.com/ang-ee/angee-operator/internal/operator/gql"
	"github.com/ang-ee/angee-operator/internal/query"
	"github.com/ang-ee/angee-operator/internal/queryfields"
	"github.com/ang-ee/angee-operator/internal/service"
	"github.com/ang-ee/angee-operator/internal/stackroot"
	"github.com/spf13/cobra"
)

var Version = "dev"

type Config struct {
	Root           string
	Bind           string
	Port           int
	Token          string
	JWTSecret      string
	AllowedOrigins []string
}

type Server struct {
	config           Config
	platform         service.API
	eventHub         *opgql.EventHub
	tokens           *tokenMinter
	logStreamer      LogStreamer
	graphqlHandler   http.Handler
	graphqlWSHandler http.Handler
	server           *http.Server
}

func Execute(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	config := Config{Root: ".", Bind: "127.0.0.1", Port: 9000}
	cmd := &cobra.Command{
		Use:           "operator",
		Short:         "Run the Angee operator",
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			server, err := NewServer(config)
			if err != nil {
				return err
			}
			addr := net.JoinHostPort(config.Bind, strconv.Itoa(config.Port))
			fmt.Fprintf(stdout, "operator listening on http://%s\n", addr)
			return server.ListenAndServe(ctx)
		},
	}
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	cmd.SetArgs(args)
	cmd.Flags().StringVar(&config.Root, "root", config.Root, "ANGEE_ROOT containing angee.yaml")
	cmd.Flags().StringVar(&config.Bind, "bind", config.Bind, "listen address")
	cmd.Flags().IntVar(&config.Port, "port", config.Port, "listen port")
	cmd.Flags().StringVar(&config.Token, "token", config.Token, "bearer token for protected endpoints")
	cmd.Flags().StringVar(&config.JWTSecret, "jwt-secret", config.JWTSecret, "explicit HS256 signing key for mintConnectionToken (default: env ANGEE_OPERATOR_JWT_SECRET, then HKDF-from-bearer, then per-process random)")
	cmd.Flags().StringArrayVar(&config.AllowedOrigins, "allowed-origin", config.AllowedOrigins, "additional allowed WebSocket Origin (repeatable); loopback origins are always allowed")
	return cmd.ExecuteContext(ctx)
}

func NewServer(config Config) (*Server, error) {
	if config.Bind == "" {
		config.Bind = "127.0.0.1"
	}
	if config.Port == 0 {
		config.Port = 9000
	}
	if !isLoopback(config.Bind) && config.Token == "" {
		return nil, errors.New("non-loopback operator binds require --token")
	}
	root, err := stackroot.Resolve(config.Root)
	if err != nil {
		return nil, err
	}
	config.Root = root
	platform, err := service.New(config.Root)
	if err != nil {
		return nil, err
	}
	jwtSecret := config.JWTSecret
	if jwtSecret == "" {
		jwtSecret = os.Getenv("ANGEE_OPERATOR_JWT_SECRET")
	}
	minter, err := newTokenMinter(jwtSecret, config.Token)
	if err != nil {
		return nil, err
	}
	eventHub := opgql.NewEventHub(platform)
	// Dev default: ephemeral live-proxy. A configured production log backend
	// would replace this with a prodStreamer.
	s := &Server{config: config, platform: platform, eventHub: eventHub, tokens: minter, logStreamer: ephemeralStreamer{platform: platform}}
	fmt.Fprintf(os.Stderr, "operator: jwt signing key fingerprint=%s\n", minter.Fingerprint())
	graphqlHandler, graphqlWSHandler, err := newGraphQLHandler(s)
	if err != nil {
		return nil, err
	}
	s.graphqlHandler = graphqlHandler
	s.graphqlWSHandler = graphqlWSHandler
	cop := http.NewCrossOriginProtection()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /edge/verify", s.edgeVerify)
	// gqlgen 0.17 dispatches subscriptions over SSE as POST with
	// Accept: text/event-stream — same route, same wrapper.
	mux.Handle("POST /graphql", s.auth(cop.Handler(s.graphqlHandler)))
	// The WebSocket upgrade is a GET with no Authorization header, so it is
	// not wrapped in s.auth (auth runs in the transport InitFunc) nor in
	// CrossOriginProtection (which treats GET as safe); the upgrader's origin
	// allowlist is the cross-site guard.
	mux.Handle("GET /graphql", s.graphqlWSHandler)
	mux.Handle("GET /stack/status", s.auth(http.HandlerFunc(s.stackStatus)))
	mux.Handle("POST /stack/init", s.auth(http.HandlerFunc(s.stackInit)))
	mux.Handle("POST /stack/update", s.auth(http.HandlerFunc(s.stackUpdate)))
	mux.Handle("POST /stack/prepare", s.auth(http.HandlerFunc(s.stackPrepare)))
	mux.Handle("POST /stack/build", s.auth(http.HandlerFunc(s.stackBuild)))
	mux.Handle("POST /stack/up", s.auth(http.HandlerFunc(s.stackUp)))
	mux.Handle("POST /stack/dev", s.auth(http.HandlerFunc(s.stackDev)))
	mux.Handle("POST /stack/down", s.auth(http.HandlerFunc(s.stackDown)))
	mux.Handle("POST /stack/destroy", s.auth(http.HandlerFunc(s.stackDestroy)))
	mux.Handle("GET /stack/logs", s.auth(http.HandlerFunc(s.stackLogs)))
	// Foreground up/dev: chunked text/plain streams that bring services up and
	// attach to their combined output, mirroring the local CLI's foreground
	// behavior so `--operator` clients see the same stream. These are GET
	// (not POST) to match the existing streaming routes and the remote
	// client's GET-only stream reader; the bring-up side effect is guarded by
	// auth() and is acceptable given these are not browser-reachable.
	mux.Handle("GET /stack/up/stream", s.auth(http.HandlerFunc(s.stackUpStream)))
	mux.Handle("GET /stack/dev/stream", s.auth(http.HandlerFunc(s.stackDevStream)))
	mux.Handle("GET /ingress/status", s.auth(http.HandlerFunc(s.ingressStatus)))
	mux.Handle("GET /jobs", s.auth(http.HandlerFunc(s.jobList)))
	mux.Handle("POST /jobs/{name}/run", s.auth(http.HandlerFunc(s.jobRun)))
	mux.Handle("GET /jobs/{name}/logs", s.auth(http.HandlerFunc(s.jobLogs)))
	mux.Handle("GET /services", s.auth(http.HandlerFunc(s.serviceList)))
	mux.Handle("POST /services", s.auth(http.HandlerFunc(s.serviceInit)))
	mux.Handle("POST /services/create", s.auth(http.HandlerFunc(s.serviceCreate)))
	mux.Handle("PATCH /services/{name}", s.auth(http.HandlerFunc(s.serviceUpdate)))
	mux.Handle("POST /services/{name}/up", s.auth(http.HandlerFunc(s.serviceUp)))
	mux.Handle("POST /services/{name}/start", s.auth(http.HandlerFunc(s.serviceStart)))
	mux.Handle("POST /services/{name}/stop", s.auth(http.HandlerFunc(s.serviceStop)))
	mux.Handle("POST /services/{name}/restart", s.auth(http.HandlerFunc(s.serviceRestart)))
	mux.Handle("POST /services/{name}/destroy", s.auth(http.HandlerFunc(s.serviceDestroy)))
	mux.Handle("GET /services/{name}/logs", s.auth(http.HandlerFunc(s.serviceLogs)))
	// The per-service log socket is a GET WebSocket upgrade: like GET /graphql
	// it carries no Authorization header, so auth runs in-handler and the
	// upgrader's origin allowlist is the cross-site guard. Not wrapped in s.auth.
	mux.Handle("GET /services/{name}/logs/stream", http.HandlerFunc(s.serviceLogsStream))
	mux.Handle("GET /services/{name}/endpoint", s.auth(http.HandlerFunc(s.serviceEndpoint)))
	mux.Handle("GET /sources", s.auth(http.HandlerFunc(s.sourceList)))
	mux.Handle("GET /sources/{name}/status", s.auth(http.HandlerFunc(s.sourceStatus)))
	mux.Handle("POST /sources/{name}/fetch", s.auth(http.HandlerFunc(s.sourceFetch)))
	mux.Handle("POST /sources/{name}/pull", s.auth(http.HandlerFunc(s.sourcePull)))
	mux.Handle("POST /sources/{name}/push", s.auth(http.HandlerFunc(s.sourcePush)))
	mux.Handle("GET /workspaces", s.auth(http.HandlerFunc(s.workspaceList)))
	mux.Handle("POST /workspaces", s.auth(http.HandlerFunc(s.workspaceCreate)))
	mux.Handle("GET /workspaces/{name}", s.auth(http.HandlerFunc(s.workspaceGet)))
	mux.Handle("PATCH /workspaces/{name}", s.auth(http.HandlerFunc(s.workspaceUpdate)))
	mux.Handle("GET /workspaces/{name}/status", s.auth(http.HandlerFunc(s.workspaceStatus)))
	mux.Handle("GET /workspaces/{name}/logs", s.auth(http.HandlerFunc(s.workspaceLogs)))
	mux.Handle("POST /workspaces/{name}/destroy", s.auth(http.HandlerFunc(s.workspaceDestroy)))
	mux.Handle("GET /workspaces/{name}/git", s.auth(http.HandlerFunc(s.workspaceGit)))
	mux.Handle("POST /workspaces/{name}/push", s.auth(http.HandlerFunc(s.workspacePush)))
	mux.Handle("POST /workspaces/{name}/sync-base", s.auth(http.HandlerFunc(s.workspaceSyncBase)))
	// REST parity for GraphQL-only operations. Every route here is auth()-wrapped
	// just like the rest of the operator surface.
	mux.Handle("GET /gitops/topology", s.auth(http.HandlerFunc(s.gitOpsTopology)))
	mux.Handle("GET /sources/{name}/diff", s.auth(http.HandlerFunc(s.sourceDiff)))
	mux.Handle("POST /workspaces/preflight", s.auth(http.HandlerFunc(s.workspaceCreatePreflight)))
	mux.Handle("POST /workspaces/{name}/sources/{slot}/fetch", s.auth(http.HandlerFunc(s.workspaceSourceFetch)))
	mux.Handle("POST /workspaces/{name}/sources/{slot}/pull", s.auth(http.HandlerFunc(s.workspaceSourcePull)))
	mux.Handle("POST /workspaces/{name}/sources/{slot}/push", s.auth(http.HandlerFunc(s.workspaceSourcePush)))
	mux.Handle("GET /workspaces/{name}/sources/{slot}/diff", s.auth(http.HandlerFunc(s.workspaceSourceDiff)))
	mux.Handle("POST /workspaces/{name}/sources/{slot}/merge", s.auth(http.HandlerFunc(s.workspaceSourceMerge)))
	mux.Handle("POST /workspaces/{name}/sources/{slot}/rebase", s.auth(http.HandlerFunc(s.workspaceSourceRebase)))
	mux.Handle("POST /workspaces/{name}/sources/{slot}/merge-abort", s.auth(http.HandlerFunc(s.workspaceSourceMergeAbort)))
	mux.Handle("POST /workspaces/{name}/sources/{slot}/rebase-abort", s.auth(http.HandlerFunc(s.workspaceSourceRebaseAbort)))
	mux.Handle("POST /workspaces/{name}/sources/{slot}/rebase-continue", s.auth(http.HandlerFunc(s.workspaceSourceRebaseContinue)))
	mux.Handle("POST /workspaces/{name}/sources/{slot}/publish", s.auth(http.HandlerFunc(s.workspaceSourcePublish)))
	mux.Handle("GET /templates", s.auth(http.HandlerFunc(s.templates)))
	mux.Handle("GET /templates/{ref...}", s.auth(http.HandlerFunc(s.template)))
	mux.Handle("POST /tokens/mint", s.auth(http.HandlerFunc(s.mintConnectionToken)))
	mux.Handle("POST /tokens/route", s.auth(http.HandlerFunc(s.mintRouteToken)))
	mux.Handle("GET /secrets", s.auth(http.HandlerFunc(s.secretsList)))
	mux.Handle("GET /secrets/{name}", s.auth(http.HandlerFunc(s.secretGet)))
	mux.Handle("GET /secrets/{name}/value", s.auth(http.HandlerFunc(s.secretValue)))
	mux.Handle("POST /secrets/{name}", s.auth(http.HandlerFunc(s.secretSet)))
	mux.Handle("DELETE /secrets/{name}", s.auth(http.HandlerFunc(s.secretDelete)))
	mux.Handle("GET /mcp", s.auth(http.HandlerFunc(s.mcp)))
	s.server = &http.Server{
		Addr:              net.JoinHostPort(config.Bind, strconv.Itoa(config.Port)),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s, nil
}

// Close releases server-owned resources. It is automatically invoked by
// ListenAndServe on shutdown; tests that construct a Server without serving
// can call Close directly to tear down background goroutines.
func (s *Server) Close() {
	if s.eventHub != nil {
		s.eventHub.Stop()
	}
}

func (s *Server) ListenAndServe(ctx context.Context) error {
	// Register SIGINT before starting the listener so a Ctrl-C arriving in
	// the brief startup window isn't delivered with its default disposition
	// (process termination).
	sigint := make(chan os.Signal, 1)
	signal.Notify(sigint, os.Interrupt)
	defer signal.Stop(sigint)

	s.eventHub.Start()
	defer s.eventHub.Stop()

	errCh := make(chan error, 1)
	go func() {
		err := s.server.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	var tearDown bool
	select {
	case <-ctx.Done():
	case <-sigint:
		tearDown = true
	case err := <-errCh:
		return err
	}

	// If the parent ctx (e.g. `angee operator` via cli/root.go) also cancels
	// on SIGINT, the select above may have woken on ctx.Done() while SIGINT
	// was simultaneously delivered to our own channel. Drain to make the
	// teardown decision deterministic.
	if !tearDown {
		select {
		case <-sigint:
			tearDown = true
		default:
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.server.Shutdown(shutdownCtx); err != nil {
		<-errCh
		return err
	}
	if tearDown {
		s.tearDownStack()
	}
	return <-errCh
}

// tearDownStack brings the local stack down when the operator receives
// SIGINT. Errors are logged but do not fail the operator's exit — by the
// time we get here the HTTP server is already closed and we want shutdown
// to make best-effort progress. The fresh background context is intentional:
// we want teardown to have its own deadline rather than inheriting one that
// may already be cancelled or near-expired.
func (s *Server) tearDownStack() {
	if s.platform == nil {
		return
	}
	fmt.Fprintln(os.Stderr, "operator: tearing down stack on SIGINT")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := s.platform.StackDown(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "operator:", err)
	}
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) stackStatus(w http.ResponseWriter, r *http.Request) {
	status, err := s.platform.StackStatus(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) stackPrepare(w http.ResponseWriter, r *http.Request) {
	compiled, err := s.platform.StackPrepare(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, compiled)
}

func (s *Server) stackInit(w http.ResponseWriter, r *http.Request) {
	req, err := decode[api.StackInitRequest](r)
	if err != nil {
		writeBadRequest(w, err)
		return
	}
	result, err := s.platform.StackInit(r.Context(), req.Template, req.Path, req.Inputs, req.Force)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"status": "initialized", "template": result.Template, "root": result.Root})
}

func (s *Server) stackUpdate(w http.ResponseWriter, r *http.Request) {
	if err := s.platform.StackUpdate(r.Context()); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func (s *Server) stackBuild(w http.ResponseWriter, r *http.Request) {
	req, err := decode[api.StackRuntimeRequest](r)
	if err != nil {
		writeBadRequest(w, err)
		return
	}
	if err := s.platform.StackBuild(r.Context(), req.Services); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "built"})
}

func (s *Server) stackUp(w http.ResponseWriter, r *http.Request) {
	req, err := decode[api.StackRuntimeRequest](r)
	if err != nil {
		writeBadRequest(w, err)
		return
	}
	if err := s.platform.StackUp(r.Context(), req.Services, req.Build); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "started"})
}

func (s *Server) stackDev(w http.ResponseWriter, r *http.Request) {
	req, err := decode[api.StackRuntimeRequest](r)
	if err != nil {
		writeBadRequest(w, err)
		return
	}
	if err := s.platform.StackDev(r.Context(), req.Build); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "started"})
}

func (s *Server) stackDown(w http.ResponseWriter, r *http.Request) {
	if err := s.platform.StackDown(r.Context()); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
}

func (s *Server) stackDestroy(w http.ResponseWriter, r *http.Request) {
	purge := r.URL.Query().Get("purge") == "true"
	if err := s.platform.StackDestroy(r.Context(), purge); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "destroyed"})
}

func (s *Server) stackLogs(w http.ResponseWriter, r *http.Request) {
	logs, err := s.platform.StackLogs(r.Context(), r.URL.Query()["service"], false)
	if err != nil {
		writeError(w, err)
		return
	}
	writeLogStream(w, logs)
}

func (s *Server) stackUpStream(w http.ResponseWriter, r *http.Request) {
	build := r.URL.Query().Get("build") == "true"
	fw := startStream(w)
	// The stream has already committed HTTP 200, so a foreground failure
	// (including setup errors) can only be reported in-band.
	if err := s.platform.StackUpForeground(r.Context(), r.URL.Query()["service"], build, fw, fw); err != nil {
		fmt.Fprintf(fw, "angee: %v\n", err)
	}
}

func (s *Server) stackDevStream(w http.ResponseWriter, r *http.Request) {
	build := r.URL.Query().Get("build") == "true"
	fw := startStream(w)
	if err := s.platform.StackDevForeground(r.Context(), build, fw, fw); err != nil {
		fmt.Fprintf(fw, "angee: %v\n", err)
	}
}

func (s *Server) serviceEndpoint(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	endpoint, err := s.platform.ServiceEndpoint(r.Context(), name)
	if err != nil {
		writeError(w, err)
		return
	}
	// Advertise the live log socket + credential alongside the service endpoint
	// so a consumer can open the stream without a second round trip.
	endpoint.LogStream = s.logStreamDescriptor(r, name)
	writeJSON(w, http.StatusOK, endpoint)
}

func (s *Server) ingressStatus(w http.ResponseWriter, r *http.Request) {
	status, err := s.platform.IngressStatus(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) serviceList(w http.ResponseWriter, r *http.Request) {
	q, err := parseListQuery(r)
	if err != nil {
		writeError(w, err)
		return
	}
	nodes, total, err := s.platform.ServiceList(r.Context(), q)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.ServiceListResponse{Nodes: nodes, TotalCount: total})
}

func (s *Server) jobList(w http.ResponseWriter, r *http.Request) {
	q, err := parseListQuery(r)
	if err != nil {
		writeError(w, err)
		return
	}
	nodes, total, err := s.platform.JobList(r.Context(), q)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.JobListResponse{Nodes: nodes, TotalCount: total})
}

func (s *Server) jobRun(w http.ResponseWriter, r *http.Request) {
	req, err := decode[api.JobRunRequest](r)
	if err != nil {
		writeBadRequest(w, err)
		return
	}
	out, err := s.platform.JobRun(r.Context(), r.PathValue("name"), req.Inputs)
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

func (s *Server) jobLogs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotImplemented, api.ErrorResponse{Error: "job logs are returned by job run"})
}

func (s *Server) serviceInit(w http.ResponseWriter, r *http.Request) {
	req, err := decode[api.ServiceInitRequest](r)
	if err != nil {
		writeBadRequest(w, err)
		return
	}
	if err := s.platform.ServiceInit(r.Context(), req); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"status": "created", "name": req.Name})
}

func (s *Server) serviceCreate(w http.ResponseWriter, r *http.Request) {
	req, err := decode[api.ServiceCreateRequest](r)
	if err != nil {
		writeBadRequest(w, err)
		return
	}
	state, err := s.platform.ServiceCreate(r.Context(), req)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, state)
}

func (s *Server) serviceUpdate(w http.ResponseWriter, r *http.Request) {
	req, err := decode[api.ServiceInitRequest](r)
	if err != nil {
		writeBadRequest(w, err)
		return
	}
	req.Name = r.PathValue("name")
	if err := s.platform.ServiceUpdate(r.Context(), req); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated", "name": req.Name})
}

func (s *Server) serviceUp(w http.ResponseWriter, r *http.Request) {
	s.serviceAction(w, r, "up")
}

func (s *Server) serviceStart(w http.ResponseWriter, r *http.Request) {
	s.serviceAction(w, r, "start")
}

func (s *Server) serviceStop(w http.ResponseWriter, r *http.Request) {
	s.serviceAction(w, r, "stop")
}

func (s *Server) serviceRestart(w http.ResponseWriter, r *http.Request) {
	s.serviceAction(w, r, "restart")
}

func (s *Server) serviceAction(w http.ResponseWriter, r *http.Request, action string) {
	name := r.PathValue("name")
	var err error
	switch action {
	case "up":
		err = s.platform.ServiceUp(r.Context(), []string{name})
	case "start":
		err = s.platform.ServiceStart(r.Context(), []string{name})
	case "stop":
		err = s.platform.ServiceStop(r.Context(), []string{name})
	case "restart":
		err = s.platform.ServiceRestart(r.Context(), []string{name})
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": action})
}

func (s *Server) serviceDestroy(w http.ResponseWriter, r *http.Request) {
	if err := s.platform.ServiceDestroy(r.Context(), r.PathValue("name"), true); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "destroyed"})
}

func (s *Server) serviceLogs(w http.ResponseWriter, r *http.Request) {
	logs, err := s.platform.StackLogs(r.Context(), []string{r.PathValue("name")}, false)
	if err != nil {
		writeError(w, err)
		return
	}
	writeLogStream(w, logs)
}

func (s *Server) sourceList(w http.ResponseWriter, r *http.Request) {
	q, err := parseListQuery(r)
	if err != nil {
		writeError(w, err)
		return
	}
	nodes, total, err := s.platform.SourceList(r.Context(), q)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.SourceListResponse{Nodes: nodes, TotalCount: total})
}

func (s *Server) sourceStatus(w http.ResponseWriter, r *http.Request) {
	state, err := s.platform.SourceStatus(r.Context(), r.PathValue("name"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (s *Server) sourceFetch(w http.ResponseWriter, r *http.Request) {
	state, err := s.platform.SourceFetch(r.Context(), r.PathValue("name"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (s *Server) sourcePull(w http.ResponseWriter, r *http.Request) {
	state, err := s.platform.SourcePull(r.Context(), r.PathValue("name"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (s *Server) sourcePush(w http.ResponseWriter, r *http.Request) {
	req, err := decode[api.SourceOperationRequest](r)
	if err != nil {
		writeBadRequest(w, err)
		return
	}
	state, err := s.platform.SourcePush(r.Context(), r.PathValue("name"), req.Ref)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (s *Server) workspaceList(w http.ResponseWriter, r *http.Request) {
	q, err := parseListQuery(r)
	if err != nil {
		writeError(w, err)
		return
	}
	nodes, total, err := s.platform.WorkspaceList(r.Context(), q)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.WorkspaceListResponse{Nodes: nodes, TotalCount: total})
}

func (s *Server) workspaceCreate(w http.ResponseWriter, r *http.Request) {
	req, err := decode[api.WorkspaceCreateRequest](r)
	if err != nil {
		writeBadRequest(w, err)
		return
	}
	ref, err := s.platform.WorkspaceCreate(r.Context(), req)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, ref)
}

func (s *Server) workspaceGet(w http.ResponseWriter, r *http.Request) {
	ref, err := s.platform.WorkspaceGet(r.Context(), r.PathValue("name"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ref)
}

func (s *Server) workspaceStatus(w http.ResponseWriter, r *http.Request) {
	status, err := s.platform.WorkspaceStatus(r.Context(), r.PathValue("name"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) workspaceUpdate(w http.ResponseWriter, r *http.Request) {
	req, err := decode[api.WorkspaceUpdateRequest](r)
	if err != nil {
		writeBadRequest(w, err)
		return
	}
	ref, err := s.platform.WorkspaceUpdate(r.Context(), r.PathValue("name"), req.Inputs, req.TTL)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ref)
}

func (s *Server) workspaceLogs(w http.ResponseWriter, r *http.Request) {
	logs, err := s.platform.WorkspaceLogs(r.Context(), r.PathValue("name"), false)
	if err != nil {
		writeError(w, err)
		return
	}
	writeLogStream(w, logs)
}

func (s *Server) workspaceDestroy(w http.ResponseWriter, r *http.Request) {
	purge := r.URL.Query().Get("purge") == "true"
	if err := s.platform.WorkspaceDestroy(r.Context(), r.PathValue("name"), purge); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "destroyed"})
}

func (s *Server) workspaceGit(w http.ResponseWriter, r *http.Request) {
	states, err := s.platform.WorkspaceGitStatus(r.Context(), r.PathValue("name"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, states)
}

func (s *Server) workspacePush(w http.ResponseWriter, r *http.Request) {
	req, err := decode[api.SourceOperationRequest](r)
	if err != nil {
		writeBadRequest(w, err)
		return
	}
	states, err := s.platform.WorkspacePush(r.Context(), r.PathValue("name"), req.Ref)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, states)
}

func (s *Server) workspaceSyncBase(w http.ResponseWriter, r *http.Request) {
	req, err := decode[api.WorkspaceSyncBaseRequest](r)
	if err != nil {
		writeBadRequest(w, err)
		return
	}
	states, err := s.platform.WorkspaceSyncBase(r.Context(), r.PathValue("name"), req.Method)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, states)
}

func (s *Server) mcp(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, mcpDescriptor())
}

// auth gates protected routes with a two-tier check. An empty configured token
// leaves the operator open (loopback dev). Otherwise a request authenticates
// either with the admin bearer (full, unscoped, server-to-server access) or
// with a minted aud="operator" token, whose actor and capability scope are
// attached to the request context for downstream enforcement.
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.config.Token == "" {
			next.ServeHTTP(w, r)
			return
		}
		token, ok := parseBearer(r.Header.Get("Authorization"))
		if !ok {
			writeJSON(w, http.StatusUnauthorized, api.ErrorResponse{Error: "unauthorized"})
			return
		}
		claims, authed := s.authenticateBearer(token)
		if !authed {
			writeJSON(w, http.StatusUnauthorized, api.ErrorResponse{Error: "unauthorized"})
			return
		}
		if claims != nil {
			r = r.WithContext(withActorScope(r.Context(), *claims))
		}
		next.ServeHTTP(w, r)
	})
}

// parseBearer extracts the credential from an Authorization-style value,
// requiring the "Bearer " scheme prefix. ok is false when the prefix is
// absent. Shared by the HTTP auth middleware and the WebSocket InitFunc so the
// two authentication entrypoints normalize the token identically.
func parseBearer(value string) (token string, ok bool) {
	token, ok = strings.CutPrefix(strings.TrimSpace(value), "Bearer ")
	return strings.TrimSpace(token), ok
}

// authenticateBearer applies the two-tier check to a raw bearer value (already
// stripped of the "Bearer " prefix). It returns (nil, true) for the admin
// bearer (full, unscoped access), (claims, true) for a valid minted
// aud=operator token, and (nil, false) otherwise. Callers must only invoke it
// when a token is configured; with no configured token, access is open and
// this is never reached.
func (s *Server) authenticateBearer(raw string) (*Claims, bool) {
	if constantTimeEqual(raw, s.config.Token) {
		return nil, true
	}
	claims, err := s.tokens.Verify(raw, audienceOperator)
	if err != nil {
		return nil, false
	}
	return &claims, true
}

// constantTimeEqual reports whether got equals want without leaking length or
// content through timing. Inputs are hashed first so the comparison is over
// fixed-width digests regardless of token length.
func constantTimeEqual(got, want string) bool {
	g := sha256.Sum256([]byte(got))
	w := sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(g[:], w[:]) == 1
}

func writeError(w http.ResponseWriter, err error) {
	writeServiceError(w, err)
}

func writeBadRequest(w http.ResponseWriter, err error) {
	writeJSON(w, http.StatusBadRequest, api.ErrorResponse{Error: err.Error()})
}

// parseListQuery reads the optional `?query=<url-encoded JSON>` filter/sort/paging
// spec a list endpoint accepts. An absent parameter yields the match-all Args;
// malformed JSON is an invalid-input error (400). Unknown filter/sort fields are
// rejected later by the platform's query.Validate.
func parseListQuery(r *http.Request) (query.Args, error) {
	raw := r.URL.Query().Get("query")
	if raw == "" {
		return query.Args{}, nil
	}
	var lq api.ListQuery
	if err := json.Unmarshal([]byte(raw), &lq); err != nil {
		return query.Args{}, &service.InvalidInputError{Field: "query", Reason: "invalid list query JSON"}
	}
	return queryfields.ToArgs(lq), nil
}

// maxRESTBodyBytes caps the size of a REST request body to keep a hostile
// or buggy client from OOM'ing the operator with a multi-gigabyte JSON
// payload. Matches the graphql handler's body cap.
const maxRESTBodyBytes = 1 << 20

func decode[T any](r *http.Request) (T, error) {
	var value T
	if r.Body == nil {
		return value, nil
	}
	// Passing a nil ResponseWriter is supported by MaxBytesReader (it only
	// uses w to set Connection: close on overflow). Callers receive a
	// *http.MaxBytesError that writeBadRequest renders as 400 — clients
	// don't get to oversize a POST simply because decode is generic.
	r.Body = http.MaxBytesReader(nil, r.Body, maxRESTBodyBytes)
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(&value); err != nil && !errors.Is(err, io.EOF) {
		return value, err
	}
	return value, nil
}

func writeLogStream(w http.ResponseWriter, logs <-chan string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	for line := range logs {
		_, _ = io.WriteString(w, line)
	}
}

// startStream commits a chunked text/plain response and returns a writer that
// flushes after every write so foreground output reaches the client live
// rather than buffering until the handler returns. Long-running foreground
// up/dev streams need this; the finite writeLogStream path does not.
func startStream(w http.ResponseWriter) io.Writer {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	return &flushWriter{w: w, flusher: asFlusher(w)}
}

func asFlusher(w http.ResponseWriter) http.Flusher {
	if f, ok := w.(http.Flusher); ok {
		return f
	}
	return nil
}

type flushWriter struct {
	w       io.Writer
	flusher http.Flusher
}

func (fw *flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	if fw.flusher != nil {
		fw.flusher.Flush()
	}
	return n, err
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func isLoopback(bind string) bool {
	ip := net.ParseIP(bind)
	if ip == nil {
		return bind == "localhost"
	}
	return ip.IsLoopback()
}

// checkWebSocketOrigin is the upgrader's CheckOrigin: the cross-site-WebSocket-
// hijacking guard for the GET /graphql upgrade (CrossOriginProtection does not
// gate it). It delegates to originAllowed against the configured allowlist.
func (s *Server) checkWebSocketOrigin(r *http.Request) bool {
	return originAllowed(r.Header.Get("Origin"), s.config.AllowedOrigins)
}

// originAllowed reports whether a WebSocket upgrade from origin is permitted.
// A request with no Origin header (a non-browser client that cannot forge one)
// and any loopback origin are always allowed; otherwise the origin must match
// a configured allowlist entry exactly (case-insensitive). This is fail-closed:
// an unparseable or non-loopback, non-allowlisted origin is rejected.
func originAllowed(origin string, allowed []string) bool {
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	// Hostnames are case-insensitive; lowercase before the loopback check so
	// e.g. "http://LOCALHOST:5173" is recognized as loopback.
	if isLoopback(strings.ToLower(u.Hostname())) {
		return true
	}
	for _, a := range allowed {
		if strings.EqualFold(strings.TrimSpace(a), origin) {
			return true
		}
	}
	return false
}
