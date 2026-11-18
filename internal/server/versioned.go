package server

import (
	"sync"
	"sync/atomic"

	"github.com/stubbedev/xilo/internal/store"
)

type versioned[K comparable, V any] struct {
	limit   int64
	current atomic.Pointer[generation[K, V]]
}

type generation[K comparable, V any] struct {
	version store.Version
	entries sync.Map
	weight  atomic.Int64
}

func newVersioned[K comparable, V any](limit int64) *versioned[K, V] {
	return &versioned[K, V]{limit: limit}
}

func (c *versioned[K, V]) get(version store.Version, key K) (V, bool) {
	g := c.current.Load()
	if g == nil || g.version != version {
		var zero V
		return zero, false
	}
	v, ok := g.entries.Load(key)
	if !ok {
		var zero V
		return zero, false
	}
	return v.(V), true
}

func (c *versioned[K, V]) put(version store.Version, key K, value V, weight int64) {
	g := c.current.Load()
	if g == nil || g.version != version {
		g = &generation[K, V]{version: version}
		c.current.Store(g)
	}
	if g.weight.Add(weight) > c.limit {
		g.weight.Add(-weight)
		return
	}
	if _, held := g.entries.LoadOrStore(key, value); held {
		g.weight.Add(-weight)
	}
}

func (c *versioned[K, V]) through(db *store.DB, key K, weigh func(V) int64, load func() (V, error)) (V, error) {
	version, ok := db.Version()
	if !ok {
		return load()
	}
	if v, hit := c.get(version, key); hit {
		return v, nil
	}
	v, err := load()
	if err == nil {
		c.put(version, key, v, weigh(v))
	}
	return v, err
}

func unitWeight[V any](V) int64 { return 1 }
