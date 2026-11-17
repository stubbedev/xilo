package cli

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type fakeWatcher struct {
	ready chan func(string)
}

func newFakeWatcher() *fakeWatcher { return &fakeWatcher{ready: make(chan func(string), 1)} }

func (w *fakeWatcher) watch(ctx context.Context, _ string, onValid func(string)) error {
	w.ready <- onValid
	<-ctx.Done()
	return nil
}

type recordingPusher struct {
	mu      sync.Mutex
	pushed  map[string]bool
	started chan []string
	release chan struct{}
	fail    error
}

func newRecordingPusher() *recordingPusher {
	return &recordingPusher{pushed: map[string]bool{}, started: make(chan []string, 64), release: make(chan struct{})}
}

func (p *recordingPusher) push(ctx context.Context, batch []string) error {
	p.started <- batch
	select {
	case <-p.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	if p.fail != nil {
		return p.fail
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, b := range batch {
		p.pushed[b] = true
	}
	return nil
}

func (p *recordingPusher) got() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Sorted(maps.Keys(p.pushed))
}

func storePaths(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("/nix/store/%032d-p", i)
	}
	return out
}

func TestWatchDropsNothingWhilePushIsSlow(t *testing.T) {
	const total = 20000
	stop, cancel := context.WithCancel(context.Background())
	defer cancel()
	w, p := newFakeWatcher(), newRecordingPusher()
	done := make(chan error, 1)
	go func() { done <- watchAndPush(stop, context.Background(), "", w.watch, p.push) }()

	add := <-w.ready
	want := storePaths(total)
	for _, path := range want {
		add(path)
		add(path)
	}
	cancel()
	close(p.release)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("watch: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("watch did not drain")
	}
	if got := p.got(); !slices.Equal(got, want) {
		t.Fatalf("pushed %d distinct paths, want all %d added", len(got), total)
	}
}

func TestWatchStopFinishesTheInFlightPushAndDrainsTheQueue(t *testing.T) {
	stop, cancel := context.WithCancel(context.Background())
	w, p := newFakeWatcher(), newRecordingPusher()
	done := make(chan error, 1)
	go func() { done <- watchAndPush(stop, context.Background(), "", w.watch, p.push) }()

	add := <-w.ready
	paths := storePaths(3)
	add(paths[0])
	<-p.started
	add(paths[1])
	add(paths[2])
	cancel()
	time.Sleep(50 * time.Millisecond)
	close(p.release)

	if err := <-done; err != nil {
		t.Fatalf("watch after stop: %v", err)
	}
	if got := p.got(); !slices.Equal(got, paths) {
		t.Fatalf("pushed %v after the stop, want the in-flight and queued paths %v", got, paths)
	}
}

func TestWatchFailedDrainNamesEveryUnpushedPath(t *testing.T) {
	stop, cancel := context.WithCancel(context.Background())
	w, p := newFakeWatcher(), newRecordingPusher()
	p.fail = errors.New("server down")
	done := make(chan error, 1)
	go func() { done <- watchAndPush(stop, context.Background(), "", w.watch, p.push) }()

	add := <-w.ready
	paths := storePaths(3)
	add(paths[0])
	<-p.started
	add(paths[1])
	add(paths[2])
	cancel()
	close(p.release)

	err := <-done
	if !errors.Is(err, p.fail) {
		t.Fatalf("err = %v, want the push failure", err)
	}
	for _, path := range paths {
		if !strings.Contains(err.Error(), path) {
			t.Errorf("error does not name unpushed %s: %v", path, err)
		}
	}
}

func TestWatchAbortDuringDrainNamesEveryUnpushedPath(t *testing.T) {
	stop, cancelStop := context.WithCancel(context.Background())
	pushCtx, abort := context.WithCancelCause(context.Background())
	w, p := newFakeWatcher(), newRecordingPusher()
	done := make(chan error, 1)
	go func() { done <- watchAndPush(stop, pushCtx, "", w.watch, p.push) }()

	add := <-w.ready
	paths := storePaths(2)
	add(paths[0])
	<-p.started
	add(paths[1])
	cancelStop()
	abort(errors.New("aborted by a second signal"))

	err := <-done
	if err == nil {
		t.Fatal("aborted drain exited cleanly")
	}
	for _, path := range paths {
		if !strings.Contains(err.Error(), path) {
			t.Errorf("error does not name unpushed %s: %v", path, err)
		}
	}
}

func TestWatchStoppedIdleIsClean(t *testing.T) {
	stop, cancel := context.WithCancel(context.Background())
	w, p := newFakeWatcher(), newRecordingPusher()
	done := make(chan error, 1)
	go func() { done <- watchAndPush(stop, context.Background(), "", w.watch, p.push) }()
	<-w.ready
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("idle stop: %v", err)
	}
}

func TestWatcherErrorStillDrainsTheQueue(t *testing.T) {
	broken := errors.New("inotify queue overflowed")
	p := newRecordingPusher()
	close(p.release)
	paths := storePaths(2)
	err := watchAndPush(context.Background(), context.Background(), "", func(_ context.Context, _ string, onValid func(string)) error {
		for _, path := range paths {
			onValid(path)
		}
		return broken
	}, p.push)
	if !errors.Is(err, broken) {
		t.Fatalf("err = %v, want the watcher's failure", err)
	}
	if got := p.got(); !slices.Equal(got, paths) {
		t.Fatalf("pushed %v, want %v drained after the watcher failed", got, paths)
	}
}

func TestSecondSignalAbortsThePush(t *testing.T) {
	stop := t.Context()
	pushCtx, release := abortOnSecondSignal(stop)
	defer release()

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-pushCtx.Done():
		t.Fatal("the first signal aborted the push")
	case <-time.After(200 * time.Millisecond):
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-pushCtx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the second signal did not abort the push")
	}
}
