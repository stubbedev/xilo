//go:build linux || darwin

package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

	// Realise paths as Nix does (lock, path, unlock) until the watcher
	// (established asynchronously) reports one. Fresh names each round: a path
	// already present when the watcher starts is not news. A .drv must be
	// filtered out.
	deadline := time.After(5 * time.Second)
	var path string
	for i := 0; path == ""; i++ {
		realise(t, dir, fmt.Sprintf("skip-me-%d.drv", i))
		realise(t, dir, fmt.Sprintf("abc-pkg-%d", i))
		select {
		case p := <-got:
			path = p
		case <-deadline:
			t.Fatal("no watch event within 5s")
		case <-time.After(20 * time.Millisecond):
		}
	}
	if !strings.HasPrefix(path, filepath.Join(dir, "abc-pkg-")) {
		t.Fatalf("path = %q, want %s/abc-pkg-N (drv locks must be filtered)", path, dir)
	}

	// A build that holds its lock across several rescans is reported once,
	// when the lock goes, and not while the path is still being written.
	slow := filepath.Join(dir, "slow-pkg")
	lock := slow + ".lock"
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if err := os.Mkdir(slow, 0o755); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	for len(got) > 0 { // stragglers from the warm-up loop
		if p := <-got; p == slow {
			t.Fatal("slow-pkg reported while its lock was held")
		}
	}
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-got:
		if p != slow {
			t.Fatalf("path = %q, want %s", p, slow)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("slow-pkg not reported after its lock went")
	}

	// Cancellation closes the fd, but on Linux a read already blocked on it
	// only wakes on the next event — generate events until watchStore returns.
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

// realise does to dir what Nix does to the store when it builds name: take
// the lock, write the path, register it, drop the lock.
func realise(t *testing.T, dir, name string) {
	t.Helper()
	lock := filepath.Join(dir, name+".lock")
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
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
	root.SetArgs([]string{"watch", "default/c", "--store", storeDir, "--url", "http://127.0.0.1:1"})
	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx) }()

	time.Sleep(100 * time.Millisecond) // let the watch establish
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
