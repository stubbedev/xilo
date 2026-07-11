package server

import (
	"fmt"
	"testing"
)

func TestNarinfoCacheLRU(t *testing.T) {
	c := newNarinfoCache(2)
	k := func(i int) narinfoKey {
		return narinfoKey{cacheID: 1, storeHash: fmt.Sprint(i), narHash: "h", pubKey: "k"}
	}
	c.put(k(1), "one")
	c.put(k(2), "two")
	if v, ok := c.get(k(1)); !ok || v != "one" {
		t.Fatal("miss on fresh entry")
	}
	c.put(k(3), "three") // evicts k(2): k(1) was touched more recently
	if _, ok := c.get(k(2)); ok {
		t.Fatal("LRU kept the stale entry")
	}
	if _, ok := c.get(k(1)); !ok {
		t.Fatal("LRU evicted the recently used entry")
	}
	// overwrite in place
	c.put(k(1), "uno")
	if v, _ := c.get(k(1)); v != "uno" {
		t.Fatal("put did not replace")
	}
}

// The cache must never serve a stale body across the two mutation paths that
// change a narinfo without changing the store hash: content upsert (new
// NarHash) and key rotation (new PubKey). Both are part of the key, so the
// old entry is simply unreachable.
func TestNarinfoCacheKeyedOnContentAndKey(t *testing.T) {
	c := newNarinfoCache(8)
	base := narinfoKey{cacheID: 1, storeHash: "s", narHash: "old", pubKey: "k1"}
	c.put(base, "old-body")

	repushed := base
	repushed.narHash = "new"
	if _, ok := c.get(repushed); ok {
		t.Fatal("upserted path hit the stale cache entry")
	}
	rotated := base
	rotated.pubKey = "k2"
	if _, ok := c.get(rotated); ok {
		t.Fatal("rotated key hit the stale cache entry")
	}
	if _, ok := c.get(base); !ok {
		t.Fatal("original entry should still resolve for identical inputs")
	}
}
