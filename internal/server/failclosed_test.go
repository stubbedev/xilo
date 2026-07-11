package server

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stubbedev/xilo/internal/storage"
)

// TestNarFailsClosedOnMissingBlob pins the guarantee attic lacks (attic#349):
// a NAR whose chunk data is gone must never be served as a clean 200 with a
// silently truncated body. Missing chunk ROW → clean 500 before headers.
// Missing BLOB (row survives, bytes gone) → the client must observe a
// transport-level failure (Content-Length mismatch / stream error), not a
// complete-looking response.
func TestNarFailsClosedOnMissingBlob(t *testing.T) {
	s, db, ts := newTestServer(t, true)
	data := []byte("some nar bytes that will go missing")
	chunkHash, narHash, narSize := fakeNar(data)
	db.CreateCache("default", "c", true, 40)
	pushFake(t, ts, "c", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", data, "")
	_ = narHash
	_ = narSize

	// Sabotage: delete the blob but keep the chunk row (crash/disk-damage state).
	root := s.cfg.Storage.Local.Root
	blob := filepath.Join(root, filepath.FromSlash(storage.ChunkKey(chunkHash)))
	if err := os.Remove(blob); err != nil {
		t.Fatalf("remove blob: %v", err)
	}

	resp, err := http.Get(ts.URL + "/default/c/nar/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.nar")
	if err == nil {
		body, rerr := io.ReadAll(resp.Body)
		resp.Body.Close()
		// Whatever happened, it must not look like a successful full response.
		if resp.StatusCode == http.StatusOK && rerr == nil && int64(len(body)) == resp.ContentLength {
			t.Fatalf("missing blob served as clean 200 with %d bytes — fails open", len(body))
		}
	}

	// Missing chunk ROW: resolved before headers, so the failure is a clean 5xx.
	if err := db.DeleteChunkRows("default", []string{chunkHash}); err != nil {
		t.Fatal(err)
	}
	resp2, err := http.Get(ts.URL + "/default/c/nar/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.nar")
	if err != nil {
		t.Fatalf("row-missing GET: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode == http.StatusOK {
		t.Fatalf("missing chunk row served 200, want error status")
	}
}
