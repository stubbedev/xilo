//go:build !linux

package cli

import (
	"errors"

	"github.com/spf13/cobra"
)

// watchCmd is Linux-only (uses inotify). Elsewhere it errors with guidance.
func watchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "watch <cache>",
		Short: "Watch the Nix store and auto-push newly-built paths (Linux only)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return errors.New("watch is only supported on Linux; use a Nix post-build-hook with `xilo push` instead")
		},
	}
}
