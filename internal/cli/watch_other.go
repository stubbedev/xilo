//go:build !linux && !darwin

package cli

import (
	"errors"

	"github.com/spf13/cobra"
)

// watchCmd needs inotify (Linux) or kqueue (macOS). Elsewhere it errors with guidance.
func watchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "watch <account>/<cache>",
		Short: "Watch the Nix store and auto-push newly-built paths (Linux, macOS)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return errors.New("watch is only supported on Linux and macOS; use a Nix post-build-hook with `xilo push` instead")
		},
	}
}
