package server

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sync"
	"time"

	"github.com/stubbedev/xilo/internal/bloom"
)

// chunkFilterTTL bounds how often the filter is rebuilt (a full scan of the
// storage backend's chunk rows) and, through the ETag, how often a pushing
// client re-downloads it. A stale filter is safe: it only lacks chunks added
// since the build, so the client uploads a few chunks that already exist
// instead of skipping ones that don't.
const chunkFilterTTL = 10 * time.Minute

// chunkFilterBody is one memoized filter for a storage backend.
type chunkFilterBody struct {
	mu    sync.Mutex
	built time.Time
	etag  string
	body  []byte // nil when the backend has too many chunks to filter
}

// handleChunkFilter serves the storage backend's chunk presence filter, so a
// pushing client can decide what to upload without a round trip per window.
// The filter reveals nothing get-missing-chunks doesn't already answer for any
// hash a push-authorized caller cares to ask about — less, in fact, since a
// hit is only probable.
func (s *Server) handleChunkFilter(w http.ResponseWriter, r *http.Request) {
	c, ok := s.cache(w, r)
	if !ok {
		return
	}
	if !s.requirePush(w, r, c) {
		return
	}
	body, etag, err := s.chunkFilter(c.Storage)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if body == nil {
		// Too many chunks to filter accurately — clients fall back to asking.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "max-age=600")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(body)
}

// chunkFilter returns the memoized filter for a storage backend, rebuilding it
// at most once per chunkFilterTTL. Concurrent callers for the same backend
// share one build; different backends build independently.
func (s *Server) chunkFilter(storage string) (body []byte, etag string, err error) {
	v, _ := s.chunkFilters.LoadOrStore(storage, &chunkFilterBody{})
	f := v.(*chunkFilterBody)
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.built.IsZero() && time.Since(f.built) < chunkFilterTTL {
		return f.body, f.etag, nil
	}
	n, err := s.db.CountChunks(storage)
	if err != nil {
		return nil, "", err
	}
	f.built = time.Now()
	f.body, f.etag = nil, ""
	if bf := bloom.New(int(n)); bf != nil {
		if err := s.db.EachChunkHash(storage, bf.Add); err != nil {
			f.built = time.Time{} // don't cache a partial scan
			return nil, "", err
		}
		f.body = bf.Marshal()
		// Content-derived ETag: the filter changes only when its bits do, so a
		// client's cached copy revalidates to 304 across rebuilds that added
		// nothing.
		sum := sha256.Sum256(f.body)
		f.etag = `"` + hex.EncodeToString(sum[:8]) + `"`
	}
	return f.body, f.etag, nil
}
