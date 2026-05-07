package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/fyltr/angee/api"
	"github.com/fyltr/angee/internal/runtime"
	"github.com/fyltr/angee/internal/service"
)

func explicitOperatorConfigured() bool {
	return operatorURL != "" || os.Getenv("ANGEE_OPERATOR_URL") != ""
}

func localPlatform(rootPath string) (*service.Platform, error) {
	if rootPath == "" {
		rootPath = resolveRoot()
	}
	return service.NewPlatform(expandPath(rootPath), nil)
}

// apiPost sends a POST with JSON body and decodes the response. By default it
// dispatches directly to an in-process operator runtime. HTTP is used only when
// --operator or ANGEE_OPERATOR_URL explicitly selects a remote operator.
func apiPost(path string, reqBody, result any) ([]byte, error) {
	if explicitOperatorConfigured() {
		return httpPost(path, reqBody, result)
	}
	response, err := localPost(context.Background(), rootFromRequest(reqBody), path, reqBody)
	return finishAPIResponse(response, result, err)
}

func apiGet(path string, result any) ([]byte, error) {
	if explicitOperatorConfigured() {
		return httpGet(path, result)
	}
	response, err := localGet(context.Background(), resolveRoot(), path)
	return finishAPIResponse(response, result, err)
}

func streamAPIGet(path string, params url.Values, out io.Writer) error {
	if explicitOperatorConfigured() {
		requestURL := fmt.Sprintf("%s%s?%s", resolveOperator(), path, params.Encode())
		resp, err := doRequest("GET", requestURL, nil)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			body, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("%s", body)
		}
		_, err = io.Copy(out, resp.Body)
		return err
	}
	return localStreamGet(context.Background(), resolveRoot(), path, params, out)
}

func httpPost(path string, reqBody, result any) ([]byte, error) {
	var bodyReader io.Reader
	if reqBody != nil {
		data, err := json.Marshal(reqBody)
		if err != nil {
			return nil, err
		}
		bodyReader = bytes.NewReader(data)
	}

	resp, err := doRequest("POST", resolveOperator()+path, bodyReader)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("%s", body)
	}

	if outputJSON {
		fmt.Println(string(body))
		return body, nil
	}

	if result != nil {
		if err := json.Unmarshal(body, result); err != nil {
			return body, fmt.Errorf("parsing response: %w", err)
		}
	}
	return body, nil
}

func httpGet(path string, result any) ([]byte, error) {
	resp, err := doRequest("GET", resolveOperator()+path, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("%s", body)
	}
	if outputJSON {
		fmt.Println(string(body))
		return body, nil
	}
	if result != nil {
		if err := json.Unmarshal(body, result); err != nil {
			return body, fmt.Errorf("parsing response: %w", err)
		}
	}
	return body, nil
}

func finishAPIResponse(response, result any, err error) ([]byte, error) {
	if err != nil {
		body, marshalErr := json.Marshal(api.ErrorResponse{Error: err.Error()})
		if marshalErr != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%s", body)
	}
	body, err := json.Marshal(response)
	if err != nil {
		return nil, err
	}
	if outputJSON {
		fmt.Println(string(body))
		return body, nil
	}
	if result != nil {
		if err := json.Unmarshal(body, result); err != nil {
			return body, fmt.Errorf("parsing response: %w", err)
		}
	}
	return body, nil
}

func localPost(ctx context.Context, rootPath, path string, reqBody any) (any, error) {
	platform, err := localPlatform(rootPath)
	if err != nil {
		return nil, err
	}
	switch {
	case path == "/stacks/init":
		return platform.StackInit(ctx, reqBody.(api.StackInitRequest))
	case path == "/stacks/update":
		return platform.StackUpdate(ctx, reqBody.(api.StackUpdateRequest))
	case path == "/workspaces/init":
		return platform.WorkspaceInit(ctx, reqBody.(api.WorkspaceInitRequest))
	case path == "/workspaces/list":
		return platform.WorkspaceList(ctx, reqBody.(api.WorkspaceListRequest))
	case strings.HasPrefix(path, "/workspaces/") && strings.HasSuffix(path, "/update"):
		req := reqBody.(api.WorkspaceUpdateRequest)
		if req.Name == "" {
			req.Name = pathSegment(path, 2)
		}
		return platform.WorkspaceUpdate(ctx, req)
	case strings.HasPrefix(path, "/workspaces/") && strings.HasSuffix(path, "/dev"):
		req := reqBody.(api.WorkspaceDevRequest)
		if req.Name == "" {
			req.Name = pathSegment(path, 2)
		}
		return platform.WorkspaceDev(ctx, req)
	case path == "/agents/init":
		return platform.AgentInit(ctx, reqBody.(api.AgentInitRequest))
	case strings.HasPrefix(path, "/agents/") && strings.HasSuffix(path, "/update"):
		req := reqBody.(api.AgentUpdateRequest)
		if req.Name == "" {
			req.Name = pathSegment(path, 2)
		}
		return platform.AgentUpdate(ctx, req)
	case strings.HasPrefix(path, "/agents/") && strings.HasSuffix(path, "/restart"):
		req := reqBody.(api.AgentActionRequest)
		if req.Name == "" {
			req.Name = pathSegment(path, 2)
		}
		return platform.AgentRestart(ctx, req)
	case strings.HasPrefix(path, "/agents/") && strings.HasSuffix(path, "/destroy"):
		req := reqBody.(api.AgentActionRequest)
		if req.Name == "" {
			req.Name = pathSegment(path, 2)
		}
		return platform.AgentDestroy(ctx, req)
	case strings.HasPrefix(path, "/agents/") && strings.HasSuffix(path, "/chat"):
		req := reqBody.(api.AgentChatRequest)
		if req.Name == "" {
			req.Name = pathSegment(path, 2)
		}
		return platform.AgentChat(ctx, req)
	case strings.HasPrefix(path, "/agents/") && strings.HasSuffix(path, "/ask"):
		req := reqBody.(api.AgentAskRequest)
		if req.Name == "" {
			req.Name = pathSegment(path, 2)
		}
		return platform.AgentAsk(ctx, req)
	case strings.HasPrefix(path, "/agents/") && strings.HasSuffix(path, "/start"):
		return platform.AgentStart(ctx, pathSegment(path, 2))
	case strings.HasPrefix(path, "/agents/") && strings.HasSuffix(path, "/stop"):
		name := pathSegment(path, 2)
		if err := platform.AgentStop(ctx, name); err != nil {
			return nil, err
		}
		return api.AgentActionResponse{Status: "stopped", Agent: name}, nil
	case path == "/reconcile":
		return platform.Reconcile(ctx, reqBody.(api.ReconcileRequest))
	case path == "/deploy":
		req, _ := reqBody.(api.DeployRequest)
		return platform.Deploy(ctx, req.Message)
	case path == "/rollback":
		return platform.Rollback(ctx, reqBody.(api.RollbackRequest).SHA)
	case path == "/pull":
		return platform.Pull(ctx)
	case path == "/down":
		if err := platform.Down(ctx); err != nil {
			return nil, err
		}
		return api.DownResponse{Status: "down"}, nil
	default:
		return nil, fmt.Errorf("unsupported local operator API path %s", path)
	}
}

func localGet(ctx context.Context, rootPath, path string) (any, error) {
	platform, err := localPlatform(rootPath)
	if err != nil {
		return nil, err
	}
	switch path {
	case "/plan":
		return platform.Plan(ctx)
	case "/status":
		return platform.Status(ctx)
	case "/agents":
		return platform.AgentList(ctx)
	default:
		return nil, fmt.Errorf("unsupported local operator API path %s", path)
	}
}

func localStreamGet(ctx context.Context, rootPath, path string, params url.Values, out io.Writer) error {
	platform, err := localPlatform(rootPath)
	if err != nil {
		return err
	}
	serviceName := ""
	switch {
	case path == "/logs":
	case strings.HasPrefix(path, "/logs/"):
		serviceName = strings.TrimPrefix(path, "/logs/")
	case strings.HasPrefix(path, "/agents/") && strings.HasSuffix(path, "/logs"):
		serviceName = "agent-" + pathSegment(path, 2)
	default:
		return fmt.Errorf("unsupported local operator API path %s", path)
	}
	rc, err := platform.ServiceLogs(ctx, serviceName, logOptionsFromValues(params))
	if err != nil {
		return err
	}
	defer rc.Close()
	_, err = io.Copy(out, rc)
	return err
}

func logOptionsFromValues(values url.Values) runtime.LogOptions {
	lines := 100
	if raw := values.Get("lines"); raw != "" {
		if parsed, err := parsePositiveInt(raw); err == nil {
			lines = parsed
		}
	}
	return runtime.LogOptions{
		Follow: values.Get("follow") == "true" || values.Get("follow") == "1",
		Lines:  lines,
		Since:  values.Get("since"),
	}
}

func pathSegment(path string, index int) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if index-1 < 0 || index-1 >= len(parts) {
		return ""
	}
	return parts[index-1]
}

func parsePositiveInt(raw string) (int, error) {
	var value int
	if _, err := fmt.Sscanf(raw, "%d", &value); err != nil {
		return 0, err
	}
	if value <= 0 {
		return 0, fmt.Errorf("not positive")
	}
	return value, nil
}
