package store

import "testing"

// A cache's footprint counts only chunks in its own backend. The lookup used
// to match on hash alone, so an identical chunk stored in a second backend was
// summed too — inflating the reported size — and, because the chunks table is
// keyed (storage, hash), the unscoped query could not use that key and
// full-scanned the table once per batch.
func TestCacheStatsScopedToItsBackend(t *testing.T) {
	db := openTest(t)
	c, err := db.CreateCache("acct", "one", true, 40)
	if err != nil {
		t.Fatal(err)
	}
	const h = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	// The same chunk hash in this cache's backend and in another one.
	if err := db.PutChunk(DefaultStorage, h, 4096, 1000, "k/"+h, 1); err != nil {
		t.Fatal(err)
	}
	if err := db.PutChunk("other", h, 4096, 7777, "k2/"+h, 1); err != nil {
		t.Fatal(err)
	}
	if err := db.PutPath(c.ID, "00000000000000000000000000000000", &Path{
		StorePath: "/nix/store/00000000000000000000000000000000-p",
		NarHash:   "sha256:x", NarSize: 4096, Chunks: []string{h},
	}); err != nil {
		t.Fatal(err)
	}

	st, err := db.CacheStats(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st.Chunks != 1 || st.PhysicalBytes != 1000 {
		t.Fatalf("stats = %+v, want 1 chunk / 1000 bytes from the default backend only", st)
	}
	if st.Paths != 1 || st.LogicalBytes != 4096 {
		t.Fatalf("stats = %+v", st)
	}
}
