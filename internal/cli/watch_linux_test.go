package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWatchStoreRescansWhatAnInotifyOverflowDropped(t *testing.T) {
	raw, err := os.ReadFile("/proc/sys/fs/inotify/max_queued_events")
	if err != nil {
		t.Fatal(err)
	}
	queueLimit, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("parse max_queued_events %q: %v", raw, err)
	}

	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	reported := map[string]bool{}
	first := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	onValid := func(p string) {
		once.Do(func() {
			close(first)
			<-release
		})
		mu.Lock()
		reported[filepath.Base(p)] = true
		mu.Unlock()
	}
	done := make(chan error, 1)
	go func() { done <- watchStore(ctx, dir, onValid, func() {}) }()

	deadline := time.After(5 * time.Second)
	for i := 0; ; i++ {
		realise(t, dir, storeName(fmt.Sprintf("warm-%d", i)))
		select {
		case <-first:
		case <-deadline:
			t.Fatal("no watch event within 5s")
		case <-time.After(20 * time.Millisecond):
			continue
		}
		break
	}

	burst := queueLimit + 64
	for i := range burst {
		realise(t, dir, storeName(fmt.Sprintf("burst-%d", i)))
	}
	close(release)

	missing := func() []string {
		mu.Lock()
		defer mu.Unlock()
		var gone []string
		for i := range burst {
			if name := storeName(fmt.Sprintf("burst-%d", i)); !reported[name] {
				gone = append(gone, name)
			}
		}
		return gone
	}
	for wait := time.After(30 * time.Second); len(missing()) > 0; {
		select {
		case <-wait:
			gone := missing()
			t.Fatalf("%d of %d paths never reported after the queue overflowed, e.g. %s", len(gone), burst, gone[0])
		case <-time.After(50 * time.Millisecond):
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("watchStore returned %v after cancel, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watchStore did not return after cancel")
	}
}
