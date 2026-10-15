package push

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stubbedev/xilo/internal/chunk"
)

func fastRetries(t *testing.T) {
	t.Helper()
	old := retryBackoff
	retryBackoff = time.Millisecond
	t.Cleanup(func() { retryBackoff = old })
}

type flakyServer struct {
	mu     sync.Mutex
	codes  []int
	bodies [][]byte
}

func (f *flakyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bodies = append(f.bodies, b)
	code := http.StatusOK
	if len(f.codes) > 0 {
		code, f.codes = f.codes[0], f.codes[1:]
	}
	w.WriteHeader(code)
}

func (f *flakyServer) attempts() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies
}

func testChunk() chunk.Chunk {
	data := []byte(strings.Repeat("xilo chunk ", 100))
	return chunk.Chunk{Hash: chunk.Hash(data), Data: data}
}

func TestPutChunkRetriesTransientGatewayErrors(t *testing.T) {
	fastRetries(t)
	f := &flakyServer{codes: []int{520, http.StatusBadGateway}}
	ts := httptest.NewServer(f)
	defer ts.Close()

	ch := testChunk()
	if err := NewClient(ts.URL, "a/c", "", 1).putChunk(context.Background(), ch); err != nil {
		t.Fatalf("put chunk after two gateway errors: %v", err)
	}
	got := f.attempts()
	if len(got) != 3 {
		t.Fatalf("attempts = %d, want 3", len(got))
	}
	for i, b := range got {
		if !bytes.Equal(b, ch.Data) {
			t.Fatalf("attempt %d sent %d bytes, want the chunk's %d", i, len(b), len(ch.Data))
		}
	}
}

func TestPutChunkGivesUpAfterMaxRetries(t *testing.T) {
	fastRetries(t)
	f := &flakyServer{codes: []int{520, 520, 520, 520, 520}}
	ts := httptest.NewServer(f)
	defer ts.Close()

	err := NewClient(ts.URL, "a/c", "", 1).putChunk(context.Background(), testChunk())
	if err == nil || !strings.Contains(err.Error(), "520") {
		t.Fatalf("err = %v, want the 520 that outlasted the retries", err)
	}
	if n := len(f.attempts()); n != maxRetries+1 {
		t.Fatalf("attempts = %d, want %d", n, maxRetries+1)
	}
}

func TestPutChunkDoesNotRetryClientErrors(t *testing.T) {
	fastRetries(t)
	f := &flakyServer{codes: []int{http.StatusBadRequest}}
	ts := httptest.NewServer(f)
	defer ts.Close()

	if err := NewClient(ts.URL, "a/c", "", 1).putChunk(context.Background(), testChunk()); err == nil {
		t.Fatal("a 400 must fail the upload")
	}
	if n := len(f.attempts()); n != 1 {
		t.Fatalf("attempts = %d, want 1", n)
	}
}

func TestRetryStopsWhenContextIsCancelled(t *testing.T) {
	old := retryBackoff
	retryBackoff = time.Hour
	t.Cleanup(func() { retryBackoff = old })
	f := &flakyServer{codes: []int{http.StatusServiceUnavailable}}
	ts := httptest.NewServer(f)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := NewClient(ts.URL, "a/c", "", 1).putChunk(ctx, testChunk())
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("err = %v, want the 503 and the cancellation", err)
	}
	if n := len(f.attempts()); n != 1 {
		t.Fatalf("attempts = %d, want 1", n)
	}
}
