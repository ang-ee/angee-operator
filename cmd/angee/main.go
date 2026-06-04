package main

import (
	"fmt"
	"os"

	"github.com/ang-ee/angee-operator/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
