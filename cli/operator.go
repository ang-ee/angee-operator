package cli

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/fyltr/angee/internal/operator"
	"github.com/spf13/cobra"
)

var (
	operatorBind string
	operatorPort int
)

var operatorCmd = &cobra.Command{
	Use:    "operator",
	Short:  "Run the embedded operator",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE:   runEmbeddedOperator,
}

func init() {
	operatorCmd.Flags().StringVar(&operatorBind, "bind", "127.0.0.1", "Bind address")
	operatorCmd.Flags().IntVar(&operatorPort, "port", 9000, "Operator port")
}

func runEmbeddedOperator(cmd *cobra.Command, args []string) error {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	srv, err := operator.New(resolveRoot(), logger)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	bind := operatorBind
	if bind == "" {
		bind = srv.Platform.Cfg.BindAddress
	}
	if bind == "" {
		bind = "127.0.0.1"
	}
	port := operatorPort
	if port == 0 {
		port = srv.Platform.Cfg.Port
	}
	if port == 0 {
		port = 9000
	}
	return srv.Start(ctx, bind+":"+strconv.Itoa(port))
}
