package main

import (
	"fmt"
	"os"

	"github.com/stubbedev/xilo/internal/cli"
)

// version is set via -ldflags "-X main.version=…".
var version = "dev"

func main() {
	root := cli.Root()
	root.Version = version
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
