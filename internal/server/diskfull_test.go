package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"syscall"
	"testing"

	"github.com/stubbedev/xilo/internal/storage"
)

type fullDisk struct{ storage.Storage }

func (fullDisk) Put(context.Context, string, io.Reader) error {
	return fmt.Errorf("write /data/storage/chunk/a7/.tmp-267854644: %w", syscall.ENOSPC)
}

func TestPutChunkOnAFullDiskIs507WithoutServerPaths(t *testing.T) {
	s, db, ts := newTestServerCfg(t, nil)
	s.sts["default"] = fullDisk{s.sts["default"]}
	if _, err := db.CreateCache("default", "c", true, 40); err != nil {
		t.Fatal(err)
	}
	data := []byte("a chunk for a full disk")
	ch, _, _ := fakeNar(data)

	resp := put(t, ts, "/c/default/c/api/chunk/"+ch, data, "")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusInsufficientStorage {
		t.Fatalf("status = %d (%s), want 507", resp.StatusCode, body)
	}
	if strings.Contains(string(body), "/data") || strings.Contains(string(body), ".tmp-") {
		t.Fatalf("body leaks a server path: %q", body)
	}
	if db.HasChunk("default", ch) {
		t.Fatal("chunk row recorded although its blob was never written")
	}
}
