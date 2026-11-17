//go:build linux || darwin

package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/stubbedev/xilo/internal/push"
)

// watchCmd auto-pushes newly-built store paths. Mirrors `attic watch-store`:
// Nix drops a `.lock` file in /nix/store while realising a path and removes it
// when the path becomes valid. watchStore is per platform, on the stdlib
// syscalls directly — no external dependency: inotify reports each lock-file
// deletion (watch_linux.go), kqueue only that the directory changed, so the
// store is rescanned (watch_darwin.go).
func watchCmd() *cobra.Command {
	var url, token, storeDir string
	c := &cobra.Command{
		Use:   "watch <account>/<cache>",
		Short: "Watch the Nix store and auto-push newly-built paths (Linux, macOS)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			url, token = resolveServer(url, token)
			if url == "" {
				return errNoServer
			}
			cache, err := resolveRef(url, token, args[0])
			if err != nil {
				return err
			}
			cl := push.NewClient(url, cache, token, 0)
			cl.Quiet = true
			fmt.Printf("watching %s → pushing to %s\n", storeDir, cache)

			ctx, cancel := context.WithCancel(cmd.Context())
			defer cancel()
			queue := newPathQueue()
			pushed := make(chan error, 1)
			go func() {
				err := queue.run(ctx, cl.Push)
				cancel()
				pushed <- err
			}()
			watchErr := watchStore(ctx, storeDir, queue.add)
			cancel()
			return errors.Join(watchErr, <-pushed)
		},
	}
	c.Flags().StringVar(&url, "url", "", "server base URL (env XILO_URL / saved login)")
	c.Flags().StringVar(&token, "token", "", "push token (env XILO_TOKEN / saved login)")
	c.Flags().StringVar(&storeDir, "store", "/nix/store", "Nix store directory to watch")
	return c
}

// lockToStorePath turns "<hash>-<name>.drv.lock" style lock file names into the
// store path they guard, filtering out .drv and -source paths (as attic does).
func lockToStorePath(storeDir, name string) (string, bool) {
	base, ok := strings.CutSuffix(name, ".lock")
	if !ok {
		return "", false
	}
	if strings.HasSuffix(base, ".drv") || strings.HasSuffix(base, "-source") {
		return "", false
	}
	if base == "" || strings.HasPrefix(base, ".") {
		return "", false
	}
	return storeDir + "/" + base, true
}
