package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/fyltr/angee/api"
)

func postProvision(path string, req any) error {
	var result api.ProvisionResponse
	if _, err := apiPost(path, req, &result); err != nil {
		return err
	}
	return printProvisionResult(&result)
}

func printProvisionResult(result *api.ProvisionResponse) error {
	if outputJSON {
		return nil
	}
	if result.Message != "" {
		printSuccess(result.Message)
	} else if result.Status != "" {
		printSuccess(result.Status)
	}
	if result.Root != "" {
		printInfo("ANGEE_ROOT: " + result.Root)
	}
	if result.Manifest != "" {
		printInfo("Manifest: " + result.Manifest)
	}
	return nil
}

func rootFromRequest(req any) string {
	switch r := req.(type) {
	case api.StackInitRequest:
		return r.Root
	case api.StackUpdateRequest:
		return r.Root
	case api.WorkspaceInitRequest:
		return r.Root
	case api.WorkspaceUpdateRequest:
		return r.Root
	case api.WorkspaceListRequest:
		return r.Root
	case api.WorkspaceDevRequest:
		return r.Root
	case api.AgentInitRequest:
		return r.Root
	case api.AgentUpdateRequest:
		return r.Root
	case api.AgentActionRequest:
		return r.Root
	case api.AgentChatRequest:
		return r.Root
	case api.AgentAskRequest:
		return r.Root
	case api.ReconcileRequest:
		return r.Root
	default:
		return resolveRoot()
	}
}

func parseKeyValueFlags(flags []string, flagName string) (map[string]string, error) {
	out := make(map[string]string, len(flags))
	for _, flag := range flags {
		key, value, ok := strings.Cut(flag, "=")
		if !ok || strings.TrimSpace(key) == "" {
			return nil, fmt.Errorf("%s expects NAME=VALUE, got %q", flagName, flag)
		}
		out[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func parsePortFlags(flags []string) (map[string]int, error) {
	out := make(map[string]int, len(flags))
	for _, flag := range flags {
		key, value, ok := strings.Cut(flag, "=")
		if !ok || strings.TrimSpace(key) == "" {
			return nil, fmt.Errorf("--port expects NAME=PORT, got %q", flag)
		}
		port, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || port <= 0 || port > 65535 {
			return nil, fmt.Errorf("--port %s has invalid port %q", strings.TrimSpace(key), value)
		}
		out[strings.TrimSpace(key)] = port
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func resolveInitRoot(path string) string {
	if angeeRoot != "" {
		return expandPath(angeeRoot)
	}
	if v := os.Getenv("ANGEE_ROOT"); v != "" {
		return expandPath(v)
	}
	base := path
	if base == "" {
		base = "."
	}
	return filepath.Join(expandPath(base), ".angee")
}

func expandPath(path string) string {
	if path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
	}
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}
