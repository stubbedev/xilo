//go:build linux

package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLockToStorePath(t *testing.T) {
	cases := []struct {
		name string
		want string
		ok   bool
	}{
		{"abc123-hello.lock", "/nix/store/abc123-hello", true},
		{"abc123-hello.drv.lock", "", false},
		{"abc123-hello-source.lock", "", false},
		{"notalock", "", false},
		{".lock", "", false},
		{".hidden.lock", "", false},
	}
	for _, tc := range cases {
		got, ok := lockToStorePath("/nix/store", tc.name)
		if ok != tc.ok || got != tc.want {
			t.Errorf("lockToStorePath(%q) = %q, %v; want %q, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

func TestWatchStore(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	got := make(chan string, 16)
	done := make(chan error, 1)
	go func() {
		done <- watchStore(ctx, dir, func(p string) { got <- p })
	}()

	// Create + remove lock files until the watcher (established asynchronously)
	// reports the deletion. A .drv.lock deletion must be filtered out.
	deadline := time.After(5 * time.Second)
	var path string
	for path == "" {
		for _, name := range []string{"skip-me.drv.lock", "abc-pkg.lock"} {
			f := filepath.Join(dir, name)
			if err := os.WriteFile(f, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(f); err != nil {
				t.Fatal(err)
			}
		}
		select {
		case p := <-got:
			path = p
		case <-deadline:
			t.Fatal("no watch event within 5s")
		case <-time.After(20 * time.Millisecond):
		}
	}
	if path != filepath.Join(dir, "abc-pkg") {
		t.Fatalf("path = %q, want %s/abc-pkg (drv locks must be filtered)", path, dir)
	}

	// Cancellation closes the fd, but a read already blocked on it only wakes
	// on the next event — generate events until watchStore returns.
	cancel()
	deadline = time.After(5 * time.Second)
	for {
		f := filepath.Join(dir, "wake.lock")
		os.WriteFile(f, nil, 0o644)
		os.Remove(f)
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("watchStore returned %v after cancel, want nil", err)
			}
			return
		case <-deadline:
			t.Fatal("watchStore did not return after cancel")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func TestWatchCmdRunsAndStops(t *testing.T) {
	isolateEnv(t)
	storeDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// silence the "watching ..." banner
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	old := os.Stdout
	os.Stdout = devnull

	root := Root()
	root.SetArgs([]string{"watch", "c", "--store", storeDir, "--url", "http://127.0.0.1:1"})
	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx) }()

	time.Sleep(100 * time.Millisecond) // let the inotify watch establish
	cancel()

	// wake the blocked read with a harmless (non-.lock) deletion
	deadline := time.After(5 * time.Second)
	for {
		f := filepath.Join(storeDir, "wake")
		os.WriteFile(f, nil, 0o644)
		os.Remove(f)
		select {
		case err := <-done:
			os.Stdout = old
			if err != nil {
				t.Fatalf("watch returned %v after cancel, want nil", err)
			}
			return
		case <-deadline:
			os.Stdout = old
			t.Fatal("watch did not stop after cancel")
		case <-time.After(20 * time.Millisecond):
		}
	}
}
