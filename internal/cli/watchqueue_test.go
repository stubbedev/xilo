package cli

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPathQueueDropsNothingWhilePushIsSlow(t *testing.T) {
	const total = 20000
	q := newPathQueue()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	release := make(chan struct{})
	var mu sync.Mutex
	pushed := map[string]bool{}
	done := make(chan error, 1)
	go func() {
		done <- q.run(ctx, func(_ context.Context, batch []string) error {
			<-release
			mu.Lock()
			for _, p := range batch {
				pushed[p] = true
			}
			n := len(pushed)
			mu.Unlock()
			if n == total {
				cancel()
			}
			return nil
		})
	}()

	want := make([]string, total)
	for i := range want {
		want[i] = fmt.Sprintf("/nix/store/%032d-p", i)
		q.add(want[i])
		q.add(want[i])
	}
	close(release)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("queue did not drain")
	}
	got := slices.Sorted(maps.Keys(pushed))
	if !slices.Equal(got, want) {
		t.Fatalf("pushed %d distinct paths, want all %d added", len(got), total)
	}
}

func TestPathQueueFailedPushIsAnErrorNamingThePath(t *testing.T) {
	q := newPathQueue()
	boom := errors.New("server down")
	q.add("/nix/store/aaaa-broken")
	err := q.run(context.Background(), func(context.Context, []string) error { return boom })
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "/nix/store/aaaa-broken") {
		t.Fatalf("err = %v, want the push failure naming the path", err)
	}
}

func TestPathQueueStoppedWithPendingPathsIsAnError(t *testing.T) {
	q := newPathQueue()
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- q.run(ctx, func(ctx context.Context, _ []string) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		})
	}()
	q.add("/nix/store/aaaa-inflight")
	<-started
	cancel()
	err := <-done
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "/nix/store/aaaa-inflight") {
		t.Fatalf("err = %v, want an error naming the path that was not pushed", err)
	}
}

func TestPathQueueStoppedIdleIsClean(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := newPathQueue().run(ctx, func(context.Context, []string) error { return nil }); err != nil {
		t.Fatalf("idle stop: %v", err)
	}
}
