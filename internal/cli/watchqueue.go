package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

type pushFunc func(context.Context, []string) error

type watchFunc func(ctx context.Context, storeDir string, onValid func(path string)) error

type pathQueue struct {
	mu      sync.Mutex
	pending []string
	queued  map[string]bool
	wake    chan struct{}
}

func newPathQueue() *pathQueue {
	return &pathQueue{queued: map[string]bool{}, wake: make(chan struct{}, 1)}
}

func (q *pathQueue) add(path string) {
	q.mu.Lock()
	if !q.queued[path] {
		q.queued[path] = true
		q.pending = append(q.pending, path)
	}
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *pathQueue) take() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	batch := q.pending
	q.pending = nil
	clear(q.queued)
	return batch
}

func (q *pathQueue) run(watched <-chan struct{}, pushCtx context.Context, push pushFunc) error {
	for {
		batch := q.take()
		if len(batch) == 0 {
			select {
			case <-q.wake:
				continue
			case <-watched:
				if batch = q.take(); len(batch) == 0 {
					return nil
				}
			case <-pushCtx.Done():
				if left := q.take(); len(left) > 0 {
					return notPushed(left, context.Cause(pushCtx))
				}
				return nil
			}
		}
		if err := push(pushCtx, batch); err != nil {
			return notPushed(append(batch, q.take()...), err)
		}
	}
}

func notPushed(paths []string, cause error) error {
	return fmt.Errorf("%d store paths not pushed: %s: %w", len(paths), strings.Join(paths, ", "), cause)
}

func watchAndPush(stopCtx, pushCtx context.Context, storeDir string, watch watchFunc, push pushFunc) error {
	watchCtx, stopWatching := context.WithCancel(stopCtx)
	defer stopWatching()
	q := newPathQueue()
	watched := make(chan struct{})
	var watchErr error
	go func() {
		watchErr = watch(watchCtx, storeDir, q.add)
		close(watched)
	}()
	pushErr := q.run(watched, pushCtx, push)
	stopWatching()
	<-watched
	return errors.Join(watchErr, pushErr)
}
