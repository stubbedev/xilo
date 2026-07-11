package store

import (
	"testing"
)

func TestPathsWithMissingChunks(t *testing.T) {
	db := openTest(t)
	c, _ := db.CreateCache("default", "c", true, 40)
	db.PutChunk("default", "ok1", 10, 5, "k1", 100)
	db.PutChunk("default", "ok2", 10, 5, "k2", 100)
	db.PutChunk("default", "bad", 10, 5, "k3", 100)

	putPath(t, db, c.ID, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", []string{"ok1", "ok2"})
	putPath(t, db, c.ID, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", []string{"ok1", "bad"})
	putPath(t, db, c.ID, "cccccccccccccccccccccccccccccccc", []string{"gone-entirely"})

	// No extra bad hashes: only the dangling-row path is broken.
	got, err := db.PathsWithMissingChunks(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].StorePath == "" {
		t.Fatalf("broken = %+v, want just the dangling-ref path", got)
	}

	// Marking "bad" as bad breaks the second path too.
	got, err = db.PathsWithMissingChunks([]string{"default/bad"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("broken = %+v, want 2", got)
	}
}

func TestDeletePathsAndChunkRows(t *testing.T) {
	db := openTest(t)
	c, _ := db.CreateCache("default", "c", true, 40)
	db.PutChunk("default", "h1", 10, 5, "k1", 100)
	putPath(t, db, c.ID, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", []string{"h1"})

	broken, err := db.PathsWithMissingChunks([]string{"default/h1"})
	if err != nil || len(broken) != 1 {
		t.Fatalf("setup: %v %v", broken, err)
	}
	if err := db.DeletePaths([]int64{broken[0].ID}); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteChunkRows("default", []string{"h1"}); err != nil {
		t.Fatal(err)
	}
	if db.HasChunk("default", "h1") {
		t.Fatal("chunk row survived")
	}
	if _, err := db.GetPath(c.ID, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); err == nil {
		t.Fatal("path survived")
	}
	// Empty inputs are no-ops.
	if err := db.DeletePaths(nil); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteChunkRows("default", nil); err != nil {
		t.Fatal(err)
	}
}

func TestOpenDurable(t *testing.T) {
	db, err := OpenDurable(t.TempDir() + "/x.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.CreateCache("default", "c", true, 40); err != nil {
		t.Fatal(err)
	}
	var mode string
	if err := db.r.QueryRow("PRAGMA synchronous").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "2" { // FULL
		t.Fatalf("synchronous = %s, want 2", mode)
	}
}
