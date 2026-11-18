package gc

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stubbedev/xilo/internal/config"
	"github.com/stubbedev/xilo/internal/storage"
	"github.com/stubbedev/xilo/internal/store"
)

type memStorage struct {
	mu    sync.Mutex
	blobs map[string][]byte
}

func newMem() *memStorage { return &memStorage{blobs: map[string][]byte{}} }

func (m *memStorage) Put(_ context.Context, key string, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.blobs[key] = b
	return nil
}

func (m *memStorage) Get(_ context.Context, key string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return io.NopCloser(bytes.NewReader(m.blobs[key])), nil
}

func (m *memStorage) LatencyBound() bool { return false }

func (m *memStorage) Has(_ context.Context, key string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.blobs[key]
	return ok, nil
}

func (m *memStorage) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.blobs, key)
	return nil
}

type spaceStorage struct {
	*memStorage
	space storage.Space
}

func (s spaceStorage) Space() (storage.Space, error) { return s.space, nil }

type fixture struct {
	db    *store.DB
	cache *store.Cache
	t0    int64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetKeyHost("xilo.test"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	c, err := db.CreateCache("acct", "c", true, 40)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{db: db, cache: c, t0: time.Now().Unix()}
}

func storeHash(i int) string { return fmt.Sprintf("%032d", i) }

func (f *fixture) addPath(t *testing.T, i int, csize int64, accessedOffset int64) {
	t.Helper()
	h := fmt.Sprintf("chunk%d", i)
	if err := f.db.PutChunk("default", h, csize, csize, storage.ChunkKey(h), 1); err != nil {
		t.Fatal(err)
	}
	p := &store.Path{StorePath: "/nix/store/" + storeHash(i) + "-p", NarHash: "sha256:h", NarSize: uint64(csize), Chunks: []string{h}}
	if err := f.db.PutPath(f.cache.ID, storeHash(i), p); err != nil {
		t.Fatal(err)
	}
	f.db.TouchPath(f.cache.ID, storeHash(i), f.t0+accessedOffset, 0)
}

func (f *fixture) present(t *testing.T) []int {
	t.Helper()
	var hashes []string
	for i := range 10 {
		hashes = append(hashes, storeHash(i))
	}
	missing, err := f.db.MissingPaths(f.cache.ID, hashes)
	if err != nil {
		t.Fatal(err)
	}
	gone := map[string]bool{}
	for _, h := range missing {
		gone[h] = true
	}
	var out []int
	for i, h := range hashes {
		if !gone[h] {
			out = append(out, i)
		}
	}
	return out
}

func (f *fixture) configure(t *testing.T, retention, maxBytes int64) {
	t.Helper()
	if err := f.db.UpdateCache(f.cache.ID, f.cache.Public, f.cache.Priority, retention, maxBytes); err != nil {
		t.Fatal(err)
	}
}

func sweepAt(t *testing.T, f *fixture, sts map[string]storage.Storage, sw Sweep, now int64) Result {
	t.Helper()
	res, err := Run(context.Background(), f.db, sts, sw, time.Unix(now, 0))
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func equal(a []int, b ...int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestRunEvictsLeastRecentlyPulledUntilTheCacheFitsItsCap(t *testing.T) {
	f := newFixture(t)
	for i := range 4 {
		f.addPath(t, i, 100, int64(10*i))
	}
	f.configure(t, 0, 250)

	res := sweepAt(t, f, map[string]storage.Storage{"default": newMem()}, Sweep{}, f.t0+100)
	if res.Evicted != 2 {
		t.Fatalf("evicted %d, want 2", res.Evicted)
	}
	if got := f.present(t); !equal(got, 2, 3) {
		t.Fatalf("present %v, want the two least recently pulled gone", got)
	}
	st, err := f.db.StatsFor(f.cache.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st.Paths != 2 || st.PhysicalBytes != 200 {
		t.Fatalf("stored stats after gc = %+v, want 2 paths and 200 physical bytes", st)
	}
}

func TestRunEvictsPathsPastTheirCacheRetention(t *testing.T) {
	f := newFixture(t)
	f.addPath(t, 0, 10, 0)
	f.addPath(t, 1, 10, 3000)
	f.configure(t, 3600, 0)

	res := sweepAt(t, f, map[string]storage.Storage{"default": newMem()}, Sweep{}, f.t0+4000)
	if res.Evicted != 1 || !equal(f.present(t), 1) {
		t.Fatalf("evicted %d, present %v; want only the path past retention gone", res.Evicted, f.present(t))
	}
}

func TestRunEvictsPathsPastTheGlobalRetentionAndOlderThan(t *testing.T) {
	f := newFixture(t)
	f.addPath(t, 0, 10, 0)
	f.addPath(t, 1, 10, 500)
	f.addPath(t, 2, 10, 900)

	sweepAt(t, f, map[string]storage.Storage{"default": newMem()}, Sweep{Policy: config.GCPolicy{Retention: 800 * time.Second}}, f.t0+1000)
	if got := f.present(t); !equal(got, 1, 2) {
		t.Fatalf("after global retention: present %v", got)
	}
	sweepAt(t, f, map[string]storage.Storage{"default": newMem()}, Sweep{OlderThan: 200 * time.Second}, f.t0+1000)
	if got := f.present(t); !equal(got, 2) {
		t.Fatalf("after --older-than: present %v", got)
	}
}

func TestRunEnforcesTheInstanceCap(t *testing.T) {
	f := newFixture(t)
	for i := range 3 {
		f.addPath(t, i, 100, int64(i))
	}
	res := sweepAt(t, f, map[string]storage.Storage{"default": newMem()}, Sweep{ConfiguredCap: 150}, f.t0+10)
	if res.Evicted != 2 || !equal(f.present(t), 2) {
		t.Fatalf("evicted %d, present %v; want only the newest left", res.Evicted, f.present(t))
	}
	if err := f.db.SetSetting(store.SettingInstanceCap, "1000"); err != nil {
		t.Fatal(err)
	}
	f.addPath(t, 3, 100, 20)
	if res := sweepAt(t, f, map[string]storage.Storage{"default": newMem()}, Sweep{ConfiguredCap: 150}, f.t0+30); res.Evicted != 0 {
		t.Fatalf("instance rule above usage still evicted %d", res.Evicted)
	}
}

func TestRunWithinEveryLimitLosesNothing(t *testing.T) {
	f := newFixture(t)
	for i := range 3 {
		f.addPath(t, i, 100, int64(i))
	}
	f.configure(t, 3600, 1000)
	res := sweepAt(t, f, map[string]storage.Storage{"default": newMem()},
		Sweep{Policy: config.GCPolicy{Retention: time.Hour}, ConfiguredCap: 1000}, f.t0+10)
	if res.Evicted != 0 || !equal(f.present(t), 0, 1, 2) {
		t.Fatalf("evicted %d, present %v; want nothing evicted", res.Evicted, f.present(t))
	}
}

func TestRunEvictsUntilTheFreeSpaceFloorIsMet(t *testing.T) {
	f := newFixture(t)
	for i := range 4 {
		f.addPath(t, i, 100, int64(i))
	}
	floor, err := config.ParseFreeFloor("10%")
	if err != nil {
		t.Fatal(err)
	}
	mem := newMem()
	for i := range 4 {
		mem.blobs[storage.ChunkKey(fmt.Sprintf("chunk%d", i))] = []byte{1}
	}
	st := spaceStorage{memStorage: mem, space: storage.Space{Total: 10000, Free: 850}}
	res := sweepAt(t, f, map[string]storage.Storage{"default": st}, Sweep{Policy: config.GCPolicy{MinFree: floor}}, f.t0+10)
	if res.Evicted != 2 || !equal(f.present(t), 2, 3) {
		t.Fatalf("evicted %d, present %v; want the two least recently pulled gone to free 150 bytes", res.Evicted, f.present(t))
	}
	if res.Deleted != 2 || res.Freed != 200 {
		t.Fatalf("swept %d chunks freeing %d bytes, want 2 and 200", res.Deleted, res.Freed)
	}
}

func TestRunAboveTheFreeSpaceFloorEvictsNothing(t *testing.T) {
	f := newFixture(t)
	for i := range 4 {
		f.addPath(t, i, 100, int64(i))
	}
	floor, err := config.ParseFreeFloor("10%")
	if err != nil {
		t.Fatal(err)
	}
	st := spaceStorage{memStorage: newMem(), space: storage.Space{Total: 10000, Free: 1000}}
	if res := sweepAt(t, f, map[string]storage.Storage{"default": st}, Sweep{Policy: config.GCPolicy{MinFree: floor}}, f.t0+10); res.Evicted != 0 {
		t.Fatalf("evicted %d above the floor", res.Evicted)
	}
}
