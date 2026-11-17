package cli

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

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

func (q *pathQueue) run(ctx context.Context, push func(context.Context, []string) error) error {
	for {
		select {
		case <-ctx.Done():
			if left := q.take(); len(left) > 0 {
				return fmt.Errorf("stopped with %d store paths not pushed: %s", len(left), strings.Join(left, ", "))
			}
			return nil
		case <-q.wake:
		}
		batch := q.take()
		if len(batch) == 0 {
			continue
		}
		if err := push(ctx, batch); err != nil {
			return fmt.Errorf("pushing %s: %w", strings.Join(batch, ", "), err)
		}
	}
}
