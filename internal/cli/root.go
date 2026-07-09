// Package cli builds xilo's cobra command tree.
package cli

import (
	"os"

	"github.com/spf13/cobra"
)

var configPath string

// Root returns the top-level `xilo` command.
func Root() *cobra.Command {
	root := &cobra.Command{
		Use:           "xilo",
		Short:         "Self-hosted Nix binary cache",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&configPath, "config", defaultConfig(),
		"path to config YAML (env XILO_CONFIG)")
	root.AddCommand(
		serveCmd(), pushCmd(), watchCmd(),
		loginCmd(), useCmd(),
		cacheCmd(), tokenCmd(), gcCmd(), schemaCmd(),
	)
	return root
}

func defaultConfig() string {
	if v := os.Getenv("XILO_CONFIG"); v != "" {
		return v
	}
	return "xilo.yaml"
}
