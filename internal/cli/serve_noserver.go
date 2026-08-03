//go:build noserver

package cli

import (
	"errors"

	"github.com/spf13/cobra"
)

// Client-only build: the `noserver` tag drops internal/server from the import
// graph, so the binary compiles without the templ/Tailwind generated assets.
// That is what makes xilo installable on platforms where the frontend
// toolchain is missing (e.g. riscv64-linux, where nixpkgs has no
// tailwindcss_4). See flake.nix `packages.xilo-cli`.
func serveCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "serve",
		Short:  "Run the cache server (unavailable in this build)",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return errors.New("this is a client-only xilo build; install the full package to run the server")
		},
	}
}
