// Package cli implements the angee command-line interface.
package cli

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
)

// Version is set at build time via -ldflags "-X github.com/fyltr/angee/cli.Version=v0.0.7".
// Defaults to "dev" for local builds.
var Version = "dev"

// httpClient is the package-level HTTP client used for CLI → operator calls.
// 60-second timeout protects the CLI from a deadlocked operator. Streaming
// endpoints (logs) bypass this and manage their own deadlines.
var httpClient = &http.Client{Timeout: 60 * time.Second}

var (
	// rootFlags
	angeeRoot   string
	operatorURL string
	outputJSON  bool
	apiKey      string
)

// rootCmd is the base command.
var rootCmd = &cobra.Command{
	Use:     "angee",
	Short:   "Agentic infrastructure as code",
	Version: Version,
	Long: `angee — self-managed agent containerisation and orchestration engine.

Get started:
  angee init                       Initialize the default dev stack
  angee stack init dev             Initialize a named stack
  angee dev                        Reconcile local dev services
  angee workspace init <name>      Provision a workspace
  angee agent init <name>          Provision an agent-backed workspace`,
}

// Execute runs the root command.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func init() {
	rootCmd.PersistentFlags().StringVar(&angeeRoot, "root", "", "ANGEE_ROOT path (default: discovered .angee)")
	rootCmd.PersistentFlags().StringVar(&operatorURL, "operator", "", "Remote operator URL (explicit only)")
	rootCmd.PersistentFlags().BoolVar(&outputJSON, "json", false, "Output in JSON format")
	rootCmd.PersistentFlags().StringVar(&apiKey, "api-key", "", "API key for operator authentication")

	// Register all subcommands
	rootCmd.AddCommand(
		operatorCmd,
		initCmd,
		stackCmd,
		workspaceCmd,
		agentCmd,
		devCmd,
		upCmd,
		downCmd,
		destroyCmd,
		lsCmd,
		deployCmd,
		rollbackCmd,
		logsCmd,
		planCmd,
		statusCmd,
		pullCmd,
		restartCmd,
	)
}

// resolveRoot returns the ANGEE_ROOT path from flag, env, or by walking parent
// directories for the target manifest. Detection order:
//  1. --root flag
//  2. ANGEE_ROOT env var
//  3. angee.yaml in cwd or a parent → that directory is ANGEE_ROOT
//  4. .angee/angee.yaml in cwd or a parent → that .angee is ANGEE_ROOT
//  5. fallback: ./.angee
func resolveRoot() string {
	if angeeRoot != "" {
		return expandPath(angeeRoot)
	}
	if v := os.Getenv("ANGEE_ROOT"); v != "" {
		return expandPath(v)
	}

	wd, err := os.Getwd()
	if err != nil {
		return ".angee"
	}
	for dir := wd; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "angee.yaml")); err == nil {
			return dir
		}
		candidate := filepath.Join(dir, ".angee")
		if _, err := os.Stat(filepath.Join(candidate, "angee.yaml")); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
	}
	return filepath.Join(wd, ".angee")
}

// resolveOperator returns the operator base URL.
func resolveOperator() string {
	if operatorURL != "" {
		return operatorURL
	}
	if v := os.Getenv("ANGEE_OPERATOR_URL"); v != "" {
		return v
	}
	return "http://localhost:9000"
}

// resolveAPIKey returns the API key from flag, env, or empty.
func resolveAPIKey() string {
	if apiKey != "" {
		return apiKey
	}
	if v := os.Getenv("ANGEE_API_KEY"); v != "" {
		return v
	}
	return ""
}

// newRequest creates an http.Request with auth and content-type headers set.
func newRequest(method, url string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	if key := resolveAPIKey(); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if body != nil && (method == "POST" || method == "PUT" || method == "PATCH") {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// doRequest creates and executes an HTTP request with auth headers.
func doRequest(method, url string, body io.Reader) (*http.Response, error) {
	req, err := newRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	return httpClient.Do(req)
}

func printSuccess(msg string) {
	fmt.Printf("  \033[32m✔\033[0m %s\n", msg)
}

func printInfo(msg string) {
	fmt.Printf("  \033[36m→\033[0m %s\n", msg)
}
