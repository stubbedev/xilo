package server

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
)

// verifyCache remembers chunk lists that already reassembled to their claimed
// NarHash. put-path's reassembly check streams every referenced chunk out of
// the blob store — on S3 that is the whole NAR downloaded again — and it is
// pure repeated work whenever the same path is registered twice: a re-push
// (upsert), a push to a second cache on the same backend, or the pessimistic
// retry after a 409.
//
// What it caches is an immutable arithmetic fact ("these chunk hashes, in this
// order, hash to this NarHash"), so entries never go stale and need no TTL.
// Crucially it does NOT stand in for the presence check: put-path re-stamps and
// re-queries the chunks on every call, before consulting this, so a chunk swept
// since the last verify is still caught.
//
// Keyed per storage backend on purpose. The fact itself is backend-independent
// (chunks are content-addressed and verified on upload), but the check is also
// what would notice bit rot in a backend, and skipping it for a backend that
// never verified these bytes would trust one backend's integrity for another's.
type verifyCache struct {
	mu  sync.Mutex
	max int
	ll  *list.List // front = most recent
	m   map[string]*list.Element
}

type verifyEnt struct{ k string }

func newVerifyCache(max int) *verifyCache {
	return &verifyCache{max: max, ll: list.New(), m: make(map[string]*list.Element, max)}
}

// verifyKey folds the inputs into one short key. Hashed rather than
// concatenated: a chunk list can be thousands of hashes, and the map would
// otherwise retain megabytes per entry.
func verifyKey(storage, narHash string, chunks []string) string {
	h := sha256.New()
	h.Write([]byte(storage))
	h.Write([]byte{0})
	h.Write([]byte(narHash))
	h.Write([]byte{0})
	h.Write([]byte(strings.Join(chunks, "\n")))
	return hex.EncodeToString(h.Sum(nil)[:16])
}

func (c *verifyCache) has(k string) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.m[k]
	if ok {
		c.ll.MoveToFront(el)
	}
	return ok
}

func (c *verifyCache) add(k string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.m[k]; ok {
		c.ll.MoveToFront(el)
		return
	}
	c.m[k] = c.ll.PushFront(&verifyEnt{k: k})
	if c.ll.Len() > c.max {
		old := c.ll.Back()
		c.ll.Remove(old)
		delete(c.m, old.Value.(*verifyEnt).k)
	}
}

func (c *verifyCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}
