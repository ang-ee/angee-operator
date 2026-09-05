package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/ang-ee/angee-operator/internal/cli"
	"github.com/ang-ee/angee-operator/internal/cli/inputform"
)

func main() {
	if err := cli.Execute(); err != nil {
		if errors.Is(err, inputform.ErrAborted) {
			fmt.Fprintln(os.Stderr, inputform.ErrAborted)
		} else {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(cli.ExitCode(err))
	}
}
