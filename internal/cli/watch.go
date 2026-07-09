//go:build linux

package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"syscall"
	"unsafe"

	"github.com/spf13/cobra"

	"github.com/stubbedev/xilo/internal/push"
)

// watchCmd auto-pushes newly-built store paths. Mirrors `attic watch-store`:
// Nix drops a `.lock` file in /nix/store while realising a path and removes it
// when the path becomes valid, so we watch for lock-file deletions. Uses the
// stdlib inotify syscalls directly — no external dependency.
func watchCmd() *cobra.Command {
	var url, token, storeDir string
	c := &cobra.Command{
		Use:   "watch <cache>",
		Short: "Watch the Nix store and auto-push newly-built paths (Linux)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			url, token = resolveServer(url, token)
			cl := push.NewClient(url, args[0], token, 0)
			cl.Quiet = true
			fmt.Printf("watching %s → pushing to %s\n", storeDir, args[0])

			// Decouple pushing from the inotify read loop: a buffered queue +
			// worker keeps draining the fd so a build burst can't overflow the
			// kernel event queue and drop store paths.
			queue := make(chan string, 1024)
			go func() {
				for path := range queue {
					if err := cl.Push(cmd.Context(), []string{path}); err != nil {
						fmt.Fprintf(os.Stderr, "push %s: %v\n", path, err)
					}
				}
			}()

			return watchStore(cmd.Context(), storeDir, func(path string) {
				select {
				case queue <- path:
				default:
					fmt.Fprintf(os.Stderr, "watch: queue full, dropping %s\n", path)
				}
			})
		},
	}
	c.Flags().StringVar(&url, "url", "", "server base URL (env XILO_URL / saved login)")
	c.Flags().StringVar(&token, "token", "", "push token (env XILO_TOKEN / saved login)")
	c.Flags().StringVar(&storeDir, "store", "/nix/store", "Nix store directory to watch")
	return c
}

func watchStore(ctx context.Context, storeDir string, onValid func(path string)) error {
	fd, err := syscall.InotifyInit1(syscall.IN_CLOEXEC)
	if err != nil {
		return fmt.Errorf("inotify_init: %w", err)
	}
	defer syscall.Close(fd)
	// Watch for the removal of lock files (a store path becoming valid).
	if _, err := syscall.InotifyAddWatch(fd, storeDir, syscall.IN_DELETE); err != nil {
		return fmt.Errorf("inotify_add_watch %s: %w", storeDir, err)
	}

	go func() { <-ctx.Done(); syscall.Close(fd) }()

	buf := make([]byte, 64*1024)
	for {
		n, err := syscall.Read(fd, buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		for off := 0; off+syscall.SizeofInotifyEvent <= n; {
			ev := (*syscall.InotifyEvent)(unsafe.Pointer(&buf[off]))
			nameLen := int(ev.Len)
			nameBytes := buf[off+syscall.SizeofInotifyEvent : off+syscall.SizeofInotifyEvent+nameLen]
			name := strings.TrimRight(string(nameBytes), "\x00")
			if path, ok := lockToStorePath(storeDir, name); ok {
				onValid(path)
			}
			off += syscall.SizeofInotifyEvent + nameLen
		}
	}
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
