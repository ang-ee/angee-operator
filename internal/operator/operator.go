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
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/fyltr/angee/api"
	"github.com/fyltr/angee/internal/service"
	"github.com/spf13/cobra"
)

type Config struct {
	Root  string
	Bind  string
	Port  int
	Token string
}

type Server struct {
	config         Config
	platform       *service.Platform
	graphqlHandler http.Handler
	server         *http.Server
}

func Execute(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	config := Config{Root: ".", Bind: "127.0.0.1", Port: 9000}
	cmd := &cobra.Command{
		Use:           "operator",
		Short:         "Run the Angee operator",
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
	root, err := resolveRoot(config.Root)
	if err != nil {
		return nil, err
	}
	config.Root = root
	platform, err := service.New(config.Root)
	if err != nil {
		return nil, err
	}
	s := &Server{config: config, platform: platform}
	graphqlHandler, err := newGraphQLHandler(s)
	if err != nil {
		return nil, err
	}
	s.graphqlHandler = graphqlHandler
	cop := http.NewCrossOriginProtection()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.Handle("POST /graphql", s.auth(cop.Handler(s.graphqlHandler)))
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
	mux.Handle("GET /jobs", s.auth(http.HandlerFunc(s.jobList)))
	mux.Handle("POST /jobs/{name}/run", s.auth(http.HandlerFunc(s.jobRun)))
	mux.Handle("GET /jobs/{name}/logs", s.auth(http.HandlerFunc(s.jobLogs)))
	mux.Handle("GET /services", s.auth(http.HandlerFunc(s.serviceList)))
	mux.Handle("POST /services", s.auth(http.HandlerFunc(s.serviceInit)))
	mux.Handle("PATCH /services/{name}", s.auth(http.HandlerFunc(s.serviceUpdate)))
	mux.Handle("POST /services/{name}/start", s.auth(http.HandlerFunc(s.serviceStart)))
	mux.Handle("POST /services/{name}/stop", s.auth(http.HandlerFunc(s.serviceStop)))
	mux.Handle("POST /services/{name}/restart", s.auth(http.HandlerFunc(s.serviceRestart)))
	mux.Handle("POST /services/{name}/destroy", s.auth(http.HandlerFunc(s.serviceDestroy)))
	mux.Handle("GET /services/{name}/logs", s.auth(http.HandlerFunc(s.serviceLogs)))
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
	mux.Handle("POST /workspaces/{name}/start", s.auth(http.HandlerFunc(s.workspaceStart)))
	mux.Handle("POST /workspaces/{name}/stop", s.auth(http.HandlerFunc(s.workspaceStop)))
	mux.Handle("POST /workspaces/{name}/restart", s.auth(http.HandlerFunc(s.workspaceRestart)))
	mux.Handle("POST /workspaces/{name}/destroy", s.auth(http.HandlerFunc(s.workspaceDestroy)))
	mux.Handle("GET /workspaces/{name}/git", s.auth(http.HandlerFunc(s.workspaceGit)))
	mux.Handle("POST /workspaces/{name}/push", s.auth(http.HandlerFunc(s.workspacePush)))
	mux.Handle("GET /events", s.auth(http.HandlerFunc(s.events)))
	mux.Handle("GET /mcp", s.auth(http.HandlerFunc(s.mcp)))
	s.server = &http.Server{
		Addr:              net.JoinHostPort(config.Bind, strconv.Itoa(config.Port)),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s, nil
}

func (s *Server) ListenAndServe(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		err := s.server.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.server.Shutdown(shutdownCtx); err != nil {
			return err
		}
		return <-errCh
	case err := <-errCh:
		return err
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

func (s *Server) serviceList(w http.ResponseWriter, r *http.Request) {
	services, err := s.platform.ServiceList(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, services)
}

func (s *Server) jobList(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.platform.JobList(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, jobs)
}

func (s *Server) jobRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Inputs map[string]string `json:"inputs"`
	}
	if decoded, err := decode[struct {
		Inputs map[string]string `json:"inputs"`
	}](r); err == nil {
		req = decoded
	} else {
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
	writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "job logs are returned by job run"})
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
	sources, err := s.platform.SourceList(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sources)
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
	refs, err := s.platform.WorkspaceList(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, refs)
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
	var req struct {
		Inputs map[string]string `json:"inputs"`
		TTL    string            `json:"ttl"`
	}
	decoded, err := decode[struct {
		Inputs map[string]string `json:"inputs"`
		TTL    string            `json:"ttl"`
	}](r)
	if err != nil {
		writeBadRequest(w, err)
		return
	}
	req = decoded
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

func (s *Server) workspaceStart(w http.ResponseWriter, r *http.Request) {
	if err := s.platform.WorkspaceStart(r.Context(), r.PathValue("name")); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "started"})
}

func (s *Server) workspaceStop(w http.ResponseWriter, r *http.Request) {
	if err := s.platform.WorkspaceStop(r.Context(), r.PathValue("name")); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
}

func (s *Server) workspaceRestart(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.platform.WorkspaceStop(r.Context(), name); err != nil {
		writeError(w, err)
		return
	}
	if err := s.platform.WorkspaceStart(r.Context(), name); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "restarted"})
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

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = fmt.Fprint(w, "event: ready\ndata: {}\n\n")
}

func (s *Server) mcp(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, mcpDescriptor())
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.config.Token == "" {
			next.ServeHTTP(w, r)
			return
		}
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		got := sha256.Sum256([]byte(token))
		want := sha256.Sum256([]byte(s.config.Token))
		if !ok || subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeError(w http.ResponseWriter, err error) {
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
}

func writeBadRequest(w http.ResponseWriter, err error) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
}

func decode[T any](r *http.Request) (T, error) {
	var value T
	if r.Body == nil {
		return value, nil
	}
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

func resolveRoot(root string) (string, error) {
	if root == "" {
		root = "."
	}
	if _, err := os.Stat(filepath.Join(root, "angee.yaml")); err == nil {
		return root, nil
	}
	control := filepath.Join(root, ".angee")
	if _, err := os.Stat(filepath.Join(control, "angee.yaml")); err == nil {
		return control, nil
	}
	return root, nil
}
