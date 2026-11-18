package server

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestZstdNarIsTheStoredFramesInOrder(t *testing.T) {
	s, db, ts := newTestServer(t, true)
	if _, err := db.CreateCache("default", "c", true, 40); err != nil {
		t.Fatal(err)
	}
	parts := [][]byte{
		bytes.Repeat([]byte("alpha-"), 4000),
		bytes.Repeat([]byte("beta-"), 9000),
		bytes.Repeat([]byte("gamma-"), 100),
	}
	pushChunked(t, ts, "c", h32, parts, nil, "")
	want := bytes.Join(parts, nil)
	served := s.metrics.narBytes.Load()

	resp := rawGet(t, ts, "/c/default/c/nar/"+h32+".nar", "zstd", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Encoding") != "zstd" {
		t.Fatalf("status %d, Content-Encoding %q", resp.StatusCode, resp.Header.Get("Content-Encoding"))
	}
	wire, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Header.Get("Content-Length"); got != strconv.Itoa(len(wire)) {
		t.Fatalf("Content-Length %s for %d bytes on the wire", got, len(wire))
	}
	dec, err := zstd.NewReader(bytes.NewReader(wire))
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	got, err := io.ReadAll(dec)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("decoded NAR is %d bytes, want %d", len(got), len(want))
	}
	if delta := s.metrics.narBytes.Load() - served; delta != int64(len(want)) {
		t.Fatalf("narBytes grew by %d, want the NAR's %d", delta, len(want))
	}
}
