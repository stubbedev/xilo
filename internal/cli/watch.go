//go:build linux || darwin

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/stubbedev/xilo/internal/push"
)

// watchCmd auto-pushes newly-built store paths. Mirrors `attic watch-store`:
// Nix drops a `.lock` file in /nix/store while realising a path and removes it
// when the path becomes valid. watchStore is per platform, on the stdlib
// syscalls directly — no external dependency: inotify reports each lock-file
// deletion (watch_linux.go), kqueue only that the directory changed, so the
// store is rescanned (watch_darwin.go).
func abortOnSecondSignal(stopCtx context.Context) (context.Context, context.CancelFunc) {
	abortCtx, abort := context.WithCancelCause(context.WithoutCancel(stopCtx))
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	seen := 0
	if stopCtx.Err() != nil {
		seen = 1
	}
	go func() {
		defer signal.Stop(sigs)
		for {
			select {
			case <-sigs:
				seen++
				if seen == 1 {
					fmt.Fprintln(os.Stderr, "xilo watch: stopping; pushing what is queued (signal again to abort)")
					continue
				}
				abort(errors.New("aborted by a second signal"))
				return
			case <-abortCtx.Done():
				return
			}
		}
	}()
	return abortCtx, func() { abort(context.Canceled) }
}

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

			pushCtx, abort := abortOnSecondSignal(cmd.Context())
			defer abort()
			return watchAndPush(cmd.Context(), pushCtx, storeDir, watchStore, cl.Push)
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
