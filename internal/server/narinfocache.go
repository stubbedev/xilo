package server

import (
	"container/list"
	"sync"
)

// narinfoCache memoizes rendered+signed narinfo bodies. Signing costs ~40µs
// of ed25519 per request; during a mass query (nix evaluating a big closure)
// the same handful of paths are hit thousands of times — this turns the hot
// path into one map lookup. Keyed by everything the body depends on: path
// identity, content hash, and the cache's current pubkey (rotation and
// re-push upserts change the key, so stale entries are unreachable, and the
// LRU evicts them).
type narinfoCache struct {
	mu  sync.Mutex
	max int
	ll  *list.List // front = most recent
	m   map[narinfoKey]*list.Element
}

type narinfoKey struct {
	cacheID   int64
	storeHash string
	narHash   string
	pubKey    string
}

type narinfoEnt struct {
	body string
	etag string
}

type narinfoElem struct {
	k narinfoKey
	narinfoEnt
}

func newNarinfoCache(max int) *narinfoCache {
	return &narinfoCache{max: max, ll: list.New(), m: make(map[narinfoKey]*list.Element, max)}
}

func (c *narinfoCache) get(k narinfoKey) (narinfoEnt, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.m[k]; ok {
		c.ll.MoveToFront(el)
		return el.Value.(*narinfoElem).narinfoEnt, true
	}
	return narinfoEnt{}, false
}

func (c *narinfoCache) put(k narinfoKey, ent narinfoEnt) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.m[k]; ok {
		c.ll.MoveToFront(el)
		el.Value.(*narinfoElem).narinfoEnt = ent
		return
	}
	c.m[k] = c.ll.PushFront(&narinfoElem{k: k, narinfoEnt: ent})
	if c.ll.Len() > c.max {
		old := c.ll.Back()
		c.ll.Remove(old)
		delete(c.m, old.Value.(*narinfoElem).k)
	}
}
