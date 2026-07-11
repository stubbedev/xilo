package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/stubbedev/xilo/internal/storage"
	"github.com/stubbedev/xilo/internal/store"
)

// fsckWorld builds a DB + local storage holding two chunks and two paths:
// "good" fully intact, "victim" whose chunk blob the test then destroys.
func fsckWorld(t *testing.T) (*store.DB, storage.Storage, string, func(hash string) string) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	st, err := storage.NewLocal(filepath.Join(dir, "storage"))
	if err != nil {
		t.Fatal(err)
	}
	enc, _ := zstd.NewWriter(nil)

	put := func(name string, data []byte) string {
		sum := sha256.Sum256(data)
		hash := hex.EncodeToString(sum[:])
		key := storage.ChunkKey(hash)
		if err := st.Put(context.Background(), key, bytes.NewReader(enc.EncodeAll(data, nil))); err != nil {
			t.Fatal(err)
		}
		if err := db.PutChunk(hash, int64(len(data)), 0, key, 100); err != nil {
			t.Fatal(err)
		}
		c, err := db.GetCache("default", "c")
		if err != nil {
			if c, err = db.CreateCache("default", "c", true, 40); err != nil {
				t.Fatal(err)
			}
		}
		p := &store.Path{
			StorePath: "/nix/store/" + strings.Repeat(name[:1], 32) + "-" + name,
			NarHash:   "sha256:h", NarSize: uint64(len(data)), Chunks: []string{hash},
		}
		if err := db.PutPath(c.ID, strings.Repeat(name[:1], 32), p); err != nil {
			t.Fatal(err)
		}
		return hash
	}
	good := put("good", []byte("intact chunk data"))
	victim := put("victim", []byte("this blob will die"))
	_ = good
	return db, st, victim, func(hash string) string { return storage.ChunkKey(hash) }
}

func fsckRun(t *testing.T, db *store.DB, st storage.Storage, content, repair bool) (string, error) {
	t.Helper()
	c := fsckCmd() // for the cobra plumbing; call runFsck directly with a buffer
	var buf bytes.Buffer
	c.SetOut(&buf)
	err := runFsck(context.Background(), c, db, st, content, repair)
	return buf.String(), err
}

func TestFsckCleanStore(t *testing.T) {
	db, st, _, _ := fsckWorld(t)
	out, err := fsckRun(t, db, st, true, false)
	if err != nil {
		t.Fatalf("clean store: %v\n%s", err, out)
	}
	if !strings.Contains(out, "chunks OK") {
		t.Fatalf("output: %s", out)
	}
}

func TestFsckDetectsMissingBlob(t *testing.T) {
	db, st, victim, key := fsckWorld(t)
	// Destroy the blob behind the row — the unhealable state.
	if err := st.Delete(context.Background(), key(victim)); err != nil {
		t.Fatal(err)
	}
	out, err := fsckRun(t, db, st, false, false)
	if err == nil {
		t.Fatalf("fsck passed with a missing blob:\n%s", out)
	}
	if !strings.Contains(out, "MISSING BLOB") || !strings.Contains(out, "BROKEN PATH") {
		t.Fatalf("output: %s", out)
	}
}

func TestFsckDetectsCorruptBlob(t *testing.T) {
	db, st, victim, key := fsckWorld(t)
	enc, _ := zstd.NewWriter(nil)
	// Valid zstd, wrong content: only --content catches it.
	if err := st.Put(context.Background(), key(victim), bytes.NewReader(enc.EncodeAll([]byte("evil"), nil))); err != nil {
		t.Fatal(err)
	}
	if out, err := fsckRun(t, db, st, false, false); err != nil {
		t.Fatalf("existence-only fsck should pass on corrupt content: %v\n%s", err, out)
	}
	out, err := fsckRun(t, db, st, true, false)
	if err == nil {
		t.Fatalf("--content missed corruption:\n%s", out)
	}
	if !strings.Contains(out, "CORRUPT BLOB") {
		t.Fatalf("output: %s", out)
	}
}

func TestFsckRepairHeals(t *testing.T) {
	db, st, victim, key := fsckWorld(t)
	if err := st.Delete(context.Background(), key(victim)); err != nil {
		t.Fatal(err)
	}
	out, err := fsckRun(t, db, st, false, true)
	if err != nil {
		t.Fatalf("repair errored: %v\n%s", err, out)
	}
	if !strings.Contains(out, "repaired") {
		t.Fatalf("output: %s", out)
	}
	// Healed: victim row + its path gone (dedup will re-accept an upload),
	// good path untouched, second run is clean.
	if db.HasChunk(victim) {
		t.Fatal("bad chunk row survived repair")
	}
	if _, err := db.GetPath(mustCache(t, db), strings.Repeat("v", 32)); err == nil {
		t.Fatal("broken path survived repair")
	}
	if _, err := db.GetPath(mustCache(t, db), strings.Repeat("g", 32)); err != nil {
		t.Fatal("good path was harmed by repair")
	}
	if out2, err := fsckRun(t, db, st, true, false); err != nil {
		t.Fatalf("post-repair fsck: %v\n%s", err, out2)
	}
}

func mustCache(t *testing.T, db *store.DB) int64 {
	t.Helper()
	c, err := db.GetCache("default", "c")
	if err != nil {
		t.Fatal(err)
	}
	return c.ID
}
