package views

import (
	"sync"

	twmerge "github.com/Oudwins/tailwind-merge-go"
	twcache "github.com/Oudwins/tailwind-merge-go/pkg/cache"
	twpkg "github.com/Oudwins/tailwind-merge-go/pkg/twmerge"
)

// templui resolves Tailwind class conflicts through the process-global
// twmerge.Merge, whose default backing cache (an LRU) is not safe for
// concurrent use: every render writes it (cache set + recency reorder), and
// xilo renders views from many concurrent HTTP handler goroutines with no
// happens-before between them — a data race the -race detector trips on.
//
// Swap the global for a merger backed by a mutex-guarded cache and warm its
// lazy init once here (init runs single-threaded, before any handler serves),
// so all later renders only read shared state or take the lock. The class-
// string keyspace is bounded by the templates, so an unbounded map never grows
// without limit — no eviction, hence no LRU needed.
// ponytail: plain locked map; if the keyspace ever became unbounded, swap in a
// size-capped sync cache.
func init() {
	twmerge.Merge = twpkg.CreateTwMerge(nil, &syncCache{m: make(map[string]string)})
	twmerge.Merge("") // force the lazy init on this goroutine
}

type syncCache struct {
	mu sync.Mutex
	m  map[string]string
}

var _ twcache.ICache = (*syncCache)(nil)

func (c *syncCache) Get(k string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[k]
}

func (c *syncCache) Set(k, v string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[k] = v
}
