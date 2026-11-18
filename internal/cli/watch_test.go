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

func TestStorePath(t *testing.T) {
	cases := []struct {
		base string
		want string
		ok   bool
	}{
		{storeName("hello"), "/nix/store/" + storeName("hello"), true},
		{storeName("hello.drv"), "", false},
		{storeName("hello-source"), "", false},
		{"abc123-hello", "", false},
		{"tmp-1916-188238956", "", false},
		{"", "", false},
		{".links", "", false},
	}
	for _, tc := range cases {
		got, ok := storePath("/nix/store", tc.base)
		if ok != tc.ok || got != tc.want {
			t.Errorf("storePath(%q) = %q, %v; want %q, %v", tc.base, got, ok, tc.want, tc.ok)
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
		done <- watchStore(ctx, dir, func(p string) { got <- p }, func() {})
	}()

	// Realise paths as Nix does (lock, path, unlock) until the watcher
	// (established asynchronously) reports one. Fresh names each round: a path
	// already present when the watcher starts is not news. A .drv must be
	// filtered out.
	deadline := time.After(5 * time.Second)
	var path string
	for i := 0; path == ""; i++ {
		realise(t, dir, fmt.Sprintf("tmp-%d-188238956", i))
		realise(t, dir, storeName(fmt.Sprintf("skip-me-%d.drv", i)))
		realise(t, dir, storeName(fmt.Sprintf("abc-pkg-%d", i)))
		select {
		case p := <-got:
			path = p
		case <-deadline:
			t.Fatal("no watch event within 5s")
		case <-time.After(20 * time.Millisecond):
		}
	}
	if !strings.HasPrefix(path, filepath.Join(dir, storeName("abc-pkg-"))) {
		t.Fatalf("path = %q, want %s/%s (drv locks and temporary directories must be filtered)", path, dir, storeName("abc-pkg-N"))
	}

	// A build that holds its lock across several rescans is reported once,
	// when the lock goes, and not while the path is still being written.
	slow := filepath.Join(dir, storeName("slow-pkg"))
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

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("watchStore returned %v after cancel, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watchStore did not return after cancel without a further store event")
	}
}

func storeName(label string) string { return strings.Repeat("0", 32) + "-" + label }

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

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		os.Stdout = old
		if err != nil {
			t.Fatalf("watch returned %v after cancel, want nil", err)
		}
	case <-time.After(5 * time.Second):
		os.Stdout = old
		t.Fatal("watch did not stop after cancel without a further store event")
	}
}

func TestWatchStoreReportsThePathRealisedRightAfterReady(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan string, 4)
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- watchStore(ctx, dir, func(p string) { got <- p }, func() { close(ready) })
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("watchStore returned before it was ready: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("watchStore never reported ready")
	}
	realise(t, dir, storeName("first"))
	select {
	case p := <-got:
		if p != filepath.Join(dir, storeName("first")) {
			t.Fatalf("reported %s, want %s", p, filepath.Join(dir, storeName("first")))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first path realised after ready was never reported")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
