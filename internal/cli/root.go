// Package cli builds xilo's cobra command tree.
package cli

import (
	"os"
	"path/filepath"

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
		cacheCmd(), tokenCmd(), gcCmd(), fsckCmd(), schemaCmd(),
	)
	return root
}

func defaultConfig() string {
	if v := os.Getenv("XILO_CONFIG"); v != "" {
		return v
	}
	if _, err := os.Stat("xilo.yaml"); err == nil {
		return "xilo.yaml"
	}
	// XDG user config (used by the home-manager module).
	if dir, err := os.UserConfigDir(); err == nil {
		p := filepath.Join(dir, "xilo", "xilo.yaml")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	// System-wide fallback (used by the NixOS module and other service installs).
	if _, err := os.Stat("/etc/xilo/xilo.yaml"); err == nil {
		return "/etc/xilo/xilo.yaml"
	}
	return "xilo.yaml"
}
