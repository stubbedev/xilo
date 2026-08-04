package server

import (
	"bytes"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stubbedev/xilo/internal/bloom"
)

func getFilter(t *testing.T, url, token, etag string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, body
}

func TestChunkFilterServesPresentChunks(t *testing.T) {
	_, db, ts := newTestServerCfg(t, nil)
	if _, err := db.CreateCache("default", "c", true, 40); err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("filtered-"), 100)
	chunkHash, _, _ := fakeNar(data)
	pushFake(t, ts, "c", h32, data, "")

	resp, body := getFilter(t, ts.URL+"/c/default/c/api/chunk-filter", "", "")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d %s", resp.StatusCode, body)
	}
	if resp.Header.Get("ETag") == "" {
		t.Fatal("no ETag: clients could never revalidate and would re-download every push")
	}
	f, err := bloom.Unmarshal(body)
	if err != nil {
		t.Fatalf("unparseable filter body: %v", err)
	}
	if !f.Has(chunkHash) {
		t.Fatal("stored chunk absent from the filter — the client would re-upload every chunk")
	}
	// A chunk the server has never seen must read absent (that is the direction
	// the pusher relies on to upload without asking).
	absent, _, _ := fakeNar([]byte("never uploaded"))
	if f.Has(absent) {
		t.Fatal("unknown chunk reported present")
	}
}

func TestChunkFilterRevalidates(t *testing.T) {
	_, db, ts := newTestServerCfg(t, nil)
	if _, err := db.CreateCache("default", "c", true, 40); err != nil {
		t.Fatal(err)
	}
	pushFake(t, ts, "c", h32, []byte("some bytes"), "")
	url := ts.URL + "/c/default/c/api/chunk-filter"

	resp, body := getFilter(t, url, "", "")
	etag := resp.Header.Get("ETag")
	if resp.StatusCode != 200 || len(body) == 0 {
		t.Fatalf("first fetch: %d, %d bytes", resp.StatusCode, len(body))
	}

	resp, body = getFilter(t, url, "", etag)
	if resp.StatusCode != http.StatusNotModified {
		t.Fatalf("matching ETag → %d, want 304", resp.StatusCode)
	}
	if len(body) != 0 {
		t.Fatalf("304 carried %d bytes of body", len(body))
	}

	resp, _ = getFilter(t, url, "", `"deadbeefdeadbeef"`)
	if resp.StatusCode != 200 {
		t.Fatalf("stale ETag → %d, want a fresh 200", resp.StatusCode)
	}
}

// The memo is what keeps a busy cache from rebuilding (and re-shipping) the
// filter on every push: inside the TTL the body and ETag stay put even after
// new chunks land.
func TestChunkFilterMemoized(t *testing.T) {
	s, db, ts := newTestServerCfg(t, nil)
	if _, err := db.CreateCache("default", "c", true, 40); err != nil {
		t.Fatal(err)
	}
	pushFake(t, ts, "c", h32, []byte("first"), "")
	url := ts.URL + "/c/default/c/api/chunk-filter"
	resp, first := getFilter(t, url, "", "")
	etag := resp.Header.Get("ETag")

	pushFake(t, ts, "c", h32b, []byte("second, added after the build"), "")
	resp, second := getFilter(t, url, "", "")
	if resp.Header.Get("ETag") != etag || !bytes.Equal(first, second) {
		t.Fatal("filter rebuilt inside the TTL; a busy cache would re-download it every push")
	}

	// Force the memo stale: the rebuild must pick up the new chunk and change
	// the ETag, or a client would never learn about it.
	v, _ := s.chunkFilters.Load("default")
	fb := v.(*chunkFilterBody)
	fb.mu.Lock()
	fb.built = time.Now().Add(-2 * chunkFilterTTL)
	fb.mu.Unlock()

	resp, third := getFilter(t, url, "", "")
	if resp.StatusCode != 200 {
		t.Fatalf("rebuild: %d", resp.StatusCode)
	}
	if resp.Header.Get("ETag") == etag || bytes.Equal(second, third) {
		t.Fatal("rebuilt filter identical to the stale one; the new chunk was missed")
	}
	f, err := bloom.Unmarshal(third)
	if err != nil {
		t.Fatal(err)
	}
	newChunk, _, _ := fakeNar([]byte("second, added after the build"))
	if !f.Has(newChunk) {
		t.Fatal("rebuild missed the chunk added since the last build")
	}
}

// A backend with more chunks than the filter can cover accurately: 204, so the
// client keeps asking instead of trusting a filter with a high hit error rate.
func TestChunkFilterNoContentWhenUnfilterable(t *testing.T) {
	s, db, ts := newTestServerCfg(t, nil)
	if _, err := db.CreateCache("default", "c", true, 40); err != nil {
		t.Fatal(err)
	}
	s.chunkFilters.Store("default", &chunkFilterBody{built: time.Now()}) // body nil
	resp, body := getFilter(t, ts.URL+"/c/default/c/api/chunk-filter", "", "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if len(body) != 0 {
		t.Fatalf("204 with %d bytes of body", len(body))
	}
}

func TestChunkFilterEmptyCache(t *testing.T) {
	_, db, ts := newTestServerCfg(t, nil)
	if _, err := db.CreateCache("default", "c", true, 40); err != nil {
		t.Fatal(err)
	}
	resp, body := getFilter(t, ts.URL+"/c/default/c/api/chunk-filter", "", "")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 with an empty filter", resp.StatusCode)
	}
	f, err := bloom.Unmarshal(body)
	if err != nil {
		t.Fatal(err)
	}
	h, _, _ := fakeNar([]byte("anything"))
	if f.Has(h) {
		t.Fatal("empty cache's filter claims to hold a chunk")
	}
}

// The filter is push-authed like the rest of the push API: it describes a
// storage backend's contents and must not leak to anonymous callers.
func TestChunkFilterRequiresPush(t *testing.T) {
	_, db, ts := newTestServerCfg(t, nil)
	if _, err := db.CreateCache("default", "pub", true, 40); err != nil {
		t.Fatal(err)
	}
	pushTok, _, _ := db.CreateToken(0, "ci", []string{"default/pub"}, []string{"push"}, 0)
	pullTok, _, _ := db.CreateToken(0, "rd", []string{"default/pub"}, []string{"pull"}, 0)
	url := ts.URL + "/c/default/pub/api/chunk-filter"

	if resp, _ := getFilter(t, url, "", ""); resp.StatusCode != 401 {
		t.Fatalf("anonymous → %d, want 401", resp.StatusCode)
	}
	if resp, _ := getFilter(t, url, pullTok, ""); resp.StatusCode != 401 {
		t.Fatalf("pull-only token → %d, want 401", resp.StatusCode)
	}
	if resp, _ := getFilter(t, url, pushTok, ""); resp.StatusCode != 200 {
		t.Fatalf("push token → %d, want 200", resp.StatusCode)
	}
}

func TestChunkFilterUnknownCache(t *testing.T) {
	_, _, ts := newTestServerCfg(t, nil)
	if resp, _ := getFilter(t, ts.URL+"/c/default/nope/api/chunk-filter", "", ""); resp.StatusCode != 404 {
		t.Fatalf("unknown cache → %d, want 404", resp.StatusCode)
	}
}

// Two backends must not share a filter: a chunk in one is not available to a
// cache on the other.
func TestChunkFilterPerStorage(t *testing.T) {
	s, db, ts := newTestServerCfg(t, nil)
	if _, err := db.CreateCache("default", "c", true, 40); err != nil {
		t.Fatal(err)
	}
	data := []byte("only in default")
	chunkHash, _, _ := fakeNar(data)
	pushFake(t, ts, "c", h32, data, "")

	dflt, _, err := s.chunkFilter("default")
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := s.chunkFilter("elsewhere")
	if err != nil {
		t.Fatal(err)
	}
	fd, err := bloom.Unmarshal(dflt)
	if err != nil {
		t.Fatal(err)
	}
	fo, err := bloom.Unmarshal(other)
	if err != nil {
		t.Fatal(err)
	}
	if !fd.Has(chunkHash) || fo.Has(chunkHash) {
		t.Fatal("filters are not per-storage-backend")
	}
}
