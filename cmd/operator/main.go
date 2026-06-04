package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/ang-ee/angee-operator/internal/operator"
)

func main() {
	os.Exit(run())
}

func run() int {
	// SIGINT is handled inside the operator package so it can run a stack
	// teardown before exiting. SIGTERM stays here as a graceful cancel.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()

	if err := operator.Execute(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
