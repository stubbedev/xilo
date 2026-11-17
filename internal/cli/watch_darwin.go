package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"
)

// watchSettle is how long a burst of store changes is left to finish before
// the store is rescanned: one scan per burst, not one per lock file.
const watchSettle = 100 * time.Millisecond

// watchStore on macOS. kqueue says only that the directory changed, never what
// changed, so we put one NOTE_WRITE watch on the directory itself,
// and on each burst a rescan diffed against the last one: a path is valid
// once it is present and its lock is gone.
func watchStore(ctx context.Context, storeDir string, onValid func(path string)) error {
	kq, err := syscall.Kqueue()
	if err != nil {
		return fmt.Errorf("kqueue: %w", err)
	}
	defer syscall.Close(kq)
	dir, err := syscall.Open(storeDir, syscall.O_EVTONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", storeDir, err)
	}
	defer syscall.Close(dir)

	var change syscall.Kevent_t
	syscall.SetKevent(&change, dir, syscall.EVFILT_VNODE, syscall.EV_ADD|syscall.EV_CLEAR)
	change.Fflags = syscall.NOTE_WRITE
	if _, err := syscall.Kevent(kq, []syscall.Kevent_t{change}, nil, nil); err != nil {
		return fmt.Errorf("kevent %s: %w", storeDir, err)
	}

	names, err := readStore(storeDir)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	pending := map[string]bool{}
	for _, n := range names {
		if base, ok := strings.CutSuffix(n, ".lock"); ok {
			pending[base] = true
		} else {
			seen[n] = true
		}
	}

	events := make([]syscall.Kevent_t, 1)
	// A timeout rather than closing the queue from another goroutine, which
	// does not reliably wake a blocked kevent.
	timeout := syscall.NsecToTimespec(int64(500 * time.Millisecond))
	for {
		n, err := syscall.Kevent(kq, nil, events, &timeout)
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return fmt.Errorf("kevent: %w", err)
		}
		if n == 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(watchSettle):
		}

		names, err := readStore(storeDir)
		if err != nil {
			return err
		}
		locks := map[string]bool{}
		for _, n := range names {
			if base, ok := strings.CutSuffix(n, ".lock"); ok {
				locks[base] = true
				pending[base] = true
			}
		}
		next := make(map[string]bool, len(names))
		for _, n := range names {
			if strings.HasSuffix(n, ".lock") {
				continue
			}
			next[n] = true
			if (seen[n] && !pending[n]) || locks[n] {
				continue
			}
			delete(pending, n)
			if path, ok := lockToStorePath(storeDir, n+".lock"); ok {
				onValid(path)
			}
		}
		// A lock that went without its path appearing was a failed or
		// cancelled build: nothing to wait for.
		for base := range pending {
			if !locks[base] && !next[base] {
				delete(pending, base)
			}
		}
		seen = next
	}
}

// readStore lists the store's entry names, unsorted and without a stat each:
// a real store holds tens of thousands of them.
func readStore(storeDir string) ([]string, error) {
	f, err := os.Open(storeDir)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Readdirnames(-1)
}
