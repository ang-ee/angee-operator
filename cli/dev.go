package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/fyltr/angee/api"
	"github.com/fyltr/angee/internal/dev"
	"github.com/spf13/cobra"
)

var (
	devOnly   []string
	devExcept []string
	devUI     string
)

var devCmd = &cobra.Command{
	Use:   "dev",
	Short: "Run the current stack through a local operator",
	Long: `Run the current stack's declared dev services, jobs, and workflows.

The CLI runs the local operator runtime in-process, streams prefixed process
output, and owns the dev services until Ctrl+C. It does not dispatch to
framework-specific tools or inspect project runtime files.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		req := api.ReconcileRequest{
			Root:   resolveRoot(),
			Mode:   "dev",
			Only:   devOnly,
			Except: devExcept,
		}
		return runDev(req)
	},
}

func runDev(req api.ReconcileRequest) error {
	if explicitOperatorConfigured() {
		return postProvision("/reconcile", req)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	platform, err := localPlatform(req.Root)
	if err != nil {
		return err
	}
	if outputJSON {
		result, err := platform.Reconcile(ctx, req)
		if err != nil {
			_ = platform.StopStackLocalServices()
			return err
		}
		if _, err := finishAPIResponse(result, nil, nil); err != nil {
			_ = platform.StopStackLocalServices()
			return err
		}
		<-ctx.Done()
		return platform.StopStackLocalServices()
	}
	sink, err := pickDevSink(devUI)
	if err != nil {
		return err
	}
	if pane, ok := sink.(*dev.PaneSink); ok {
		go func() {
			<-pane.Done()
			if proc, err := os.FindProcess(os.Getpid()); err == nil {
				_ = proc.Signal(os.Interrupt)
			}
		}()
		defer func() {
			pane.Quit()
			pane.Wait()
		}()
	}
	result, err := platform.ReconcileWithOutput(ctx, req, sink)
	if err != nil {
		_ = platform.StopStackLocalServices()
		return err
	}
	sink.SystemLine("%s", result.Message)
	sink.SystemLine("ANGEE_ROOT: %s", result.Root)
	sink.SystemLine("Manifest: %s", result.Manifest)
	sink.SystemLine("dev services are running; press Ctrl+C to stop")
	<-ctx.Done()
	if !strings.EqualFold(devUI, "panes") {
		fmt.Println()
	}
	sink.SystemLine("stopping dev services")
	if err := platform.StopStackLocalServices(); err != nil {
		return err
	}
	sink.SystemLine("dev services stopped")
	return nil
}

func pickDevSink(mode string) (dev.Sink, error) {
	switch strings.ToLower(mode) {
	case "", "lines":
		return dev.NewLineSink(os.Stdout, false), nil
	case "panes":
		if !dev.IsStdoutTTY() {
			return nil, fmt.Errorf("--ui=panes requires an interactive terminal")
		}
		return dev.NewPaneSink(), nil
	default:
		return nil, fmt.Errorf("--ui=%q: must be 'lines' or 'panes'", mode)
	}
}

func init() {
	devCmd.Flags().StringSliceVar(&devOnly, "only", nil, "Run only these declared services/jobs")
	devCmd.Flags().StringSliceVar(&devExcept, "except", nil, "Run all declared services/jobs except these")
	devCmd.Flags().StringVar(&devUI, "ui", "lines", "Output mode: lines or panes")
}
