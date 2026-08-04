package store

import (
	"database/sql"
	"fmt"
	"sort"
	"testing"
)

// putTestPath registers a path with two chunks in a cache.
func putTestPath(t *testing.T, db *DB, cacheID int64, storeHash, narHash string, chunks []string) {
	t.Helper()
	p := &Path{
		StorePath: "/nix/store/" + storeHash + "-pkg",
		NarHash:   narHash,
		NarSize:   4242,
		Deriver:   "pkg.drv",
		Refs:      []string{"/nix/store/" + storeHash + "-dep"},
		Chunks:    chunks,
	}
	if err := db.PutPath(cacheID, storeHash, p); err != nil {
		t.Fatal(err)
	}
}

// CreateCache returns a Cache whose Storage field is empty; the column default
// is "default", which is what every server read sees. Use that name here.
const testStorage = "default"

func TestAdoptCandidates(t *testing.T) {
	db := openTest(t)
	a, err := db.CreateCache("acme", "src", true, 40)
	if err != nil {
		t.Fatal(err)
	}
	b, err := db.CreateCache("acme", "dst", false, 40)
	if err != nil {
		t.Fatal(err)
	}
	const sh = "abcdfghijklmnpqrsvwxyz0123456789"
	chunks := []string{"aa", "bb"}
	putTestPath(t, db, a.ID, sh, "sha256:abc", chunks)

	t.Run("finds the path in the other cache on the same storage", func(t *testing.T) {
		got, err := db.AdoptCandidates(testStorage, b.ID, []string{sh})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("candidates = %d, want 1", len(got))
		}
		cd := got[0]
		if cd.StoreHash != sh || cd.Account != "acme" || cd.Cache != "src" || !cd.Public {
			t.Fatalf("wrong source identity: %+v", cd)
		}
		// The whole payload has to come back: it is copied verbatim into the
		// target cache, so a dropped field would serve a wrong narinfo.
		if cd.Path.StorePath != "/nix/store/"+sh+"-pkg" || cd.Path.NarHash != "sha256:abc" ||
			cd.Path.NarSize != 4242 || cd.Path.Deriver != "pkg.drv" ||
			len(cd.Path.Refs) != 1 || cd.Path.Refs[0] != "/nix/store/"+sh+"-dep" ||
			len(cd.Path.Chunks) != 2 || cd.Path.Chunks[0] != "aa" || cd.Path.Chunks[1] != "bb" {
			t.Fatalf("payload not round-tripped: %+v", cd.Path)
		}
	})

	t.Run("excludes the target cache itself", func(t *testing.T) {
		got, err := db.AdoptCandidates(testStorage, a.ID, []string{sh})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Fatalf("candidates = %d, want 0 (own path is not an adoption source)", len(got))
		}
	})

	t.Run("excludes other storage backends", func(t *testing.T) {
		got, err := db.AdoptCandidates("elsewhere", b.ID, []string{sh})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Fatalf("candidates = %d, want 0: chunks in one backend can't serve another", len(got))
		}
	})

	t.Run("unknown hash finds nothing", func(t *testing.T) {
		got, err := db.AdoptCandidates(testStorage, b.ID, []string{"zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Fatalf("candidates = %d, want 0", len(got))
		}
	})

	t.Run("empty input", func(t *testing.T) {
		got, err := db.AdoptCandidates(testStorage, b.ID, nil)
		if err != nil || len(got) != 0 {
			t.Fatalf("got %v, %v", got, err)
		}
	})

	// A cache whose account is soft-deleted must never be an adoption source:
	// its bytes are on their way out and it no longer serves anyone.
	t.Run("skips caches under a deleted account", func(t *testing.T) {
		if err := db.write(func(tx *sql.Tx) error {
			_, err := tx.Exec(`UPDATE accounts SET status='deleted' WHERE slug='acme'`)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		got, err := db.AdoptCandidates(testStorage, b.ID, []string{sh})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Fatalf("candidates = %d, want 0 for a soft-deleted account", len(got))
		}
	})
}

// More candidates than one query's placeholder budget, to exercise batching.
func TestAdoptCandidatesBatches(t *testing.T) {
	db := openTest(t)
	src, err := db.CreateCache("acme", "src", true, 40)
	if err != nil {
		t.Fatal(err)
	}
	dst, err := db.CreateCache("acme", "dst", true, 40)
	if err != nil {
		t.Fatal(err)
	}
	const n = batchVars + 50
	want := make([]string, 0, n)
	for i := 0; i < n; i++ {
		sh := fmt.Sprintf("%032d", i)
		putTestPath(t, db, src.ID, sh, "sha256:h", []string{"c"})
		want = append(want, sh)
	}
	got, err := db.AdoptCandidates(testStorage, dst.ID, want)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != n {
		t.Fatalf("candidates = %d, want %d (batching dropped rows)", len(got), n)
	}
}

func TestCountAndEachChunkHash(t *testing.T) {
	db := openTest(t)
	if n, err := db.CountChunks("default"); err != nil || n != 0 {
		t.Fatalf("empty backend: %d, %v", n, err)
	}
	var want []string
	for i := 0; i < 250; i++ {
		h := fmt.Sprintf("hash-%03d", i)
		if err := db.PutChunk("default", h, 10, 5, "k/"+h, 1); err != nil {
			t.Fatal(err)
		}
		want = append(want, h)
	}
	if err := db.PutChunk("other", "elsewhere", 10, 5, "k/e", 1); err != nil {
		t.Fatal(err)
	}

	n, err := db.CountChunks("default")
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(want)) {
		t.Fatalf("CountChunks = %d, want %d", n, len(want))
	}
	var got []string
	if err := db.EachChunkHash("default", func(h string) error {
		got = append(got, h)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("EachChunkHash yielded %d hashes, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("hash %d = %q, want %q", i, got[i], want[i])
		}
	}

	t.Run("callback error propagates", func(t *testing.T) {
		boom := fmt.Errorf("boom")
		if err := db.EachChunkHash("default", func(string) error { return boom }); err != boom {
			t.Fatalf("err = %v, want boom", err)
		}
	})
}
