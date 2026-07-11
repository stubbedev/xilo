package server

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/stubbedev/xilo/internal/narinfo"
	"github.com/stubbedev/xilo/internal/store"
)

func (s *Server) handleCacheInfo(w http.ResponseWriter, r *http.Request) {
	c, ok := s.cache(w, r)
	if !ok {
		return
	}
	if !s.requirePull(w, r, c) {
		return
	}
	w.Header().Set("Content-Type", "text/x-nix-cache-info")
	fmt.Fprintf(w, "StoreDir: %s\nWantMassQuery: 1\nPriority: %d\n", narinfo.StoreDir, c.Priority)
}

func (s *Server) handleNarinfo(w http.ResponseWriter, r *http.Request) {
	file := r.PathValue("file")
	if !strings.HasSuffix(file, ".narinfo") {
		// Also catches stray browser URLs like /admin/typo (cache="admin").
		s.notFoundNegotiated(w, r)
		return
	}
	c, ok := s.cache(w, r)
	if !ok {
		return
	}
	if !s.requirePull(w, r, c) {
		return
	}
	storeHash := strings.TrimSuffix(file, ".narinfo")
	p, err := s.db.GetPath(c.ID, storeHash)
	if errors.Is(err, store.ErrNotFound) {
		s.metrics.narinfoMiss.Add(1)
		// Short negative cache so a CDN doesn't hammer us for absent paths.
		w.Header().Set("Cache-Control", "public, max-age=30")
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.metrics.narinfoHit.Add(1)
	s.touchPath(c.ID, storeHash)

	key := narinfoKey{cacheID: c.ID, storeHash: storeHash, narHash: p.NarHash, pubKey: c.PubKey}
	body, cached := s.niCache.get(key)
	if !cached {
		refBases := make([]string, len(p.Refs))
		for i, ref := range p.Refs {
			refBases[i] = narinfo.BaseName(ref)
		}
		ni := &narinfo.NarInfo{
			StorePath:   p.StorePath,
			URL:         "nar/" + storeHash + ".nar",
			Compression: "none",
			FileHash:    p.NarHash,
			FileSize:    p.NarSize,
			NarHash:     p.NarHash,
			NarSize:     p.NarSize,
			References:  refBases,
			Deriver:     p.Deriver,
		}
		fp := narinfo.Fingerprint(p.StorePath, p.NarHash, p.NarSize, p.Refs)
		ni.Sig = []string{narinfo.Sign(c.Name, c.PrivKey, fp)}
		body = ni.String()
		s.niCache.put(key, body)
	}

	w.Header().Set("Content-Type", "text/x-nix-narinfo")
	// Cache hard, but key validators on CONTENT, not the store hash: a re-push
	// can upsert different bytes under the same store hash, and the signature
	// changes on key rotation — a CDN holding the old narinfo against a new
	// NAR would hand clients a permanent hash mismatch.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("ETag", contentETag(p.NarHash, c.PubKey))
	io.WriteString(w, body)
}

func (s *Server) handleNar(w http.ResponseWriter, r *http.Request) {
	c, ok := s.cache(w, r)
	if !ok {
		return
	}
	if !s.requirePull(w, r, c) {
		return
	}
	storeHash := strings.TrimSuffix(r.PathValue("id"), ".nar")
	p, err := s.db.GetPath(c.ID, storeHash)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// LRU by last pull: a download starting now must not be the next eviction
	// victim (same bump as narinfo — nix fetches nar right after).
	s.touchPath(c.ID, storeHash)

	// Resolve all chunk keys up front — if any is missing we can still return a
	// clean error before committing a 200 + Content-Length.
	refs, err := s.db.ChunkKeys(p.Chunks)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// The narinfo advertises Compression: none (FileHash == NarHash). We may
	// still compress the HTTP transfer via Content-Encoding, which Nix's curl
	// transparently decodes — saves bandwidth without touching the NAR hash.
	w.Header().Set("Content-Type", "application/x-nix-nar")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("ETag", contentETag(p.NarHash, "")) // NAR bytes ⇔ NarHash
	s.metrics.narServed.Add(1)

	// zstd clients get the stored frames verbatim: chunks are complete zstd
	// frames and a concatenation of frames is a valid stream (RFC 8878), so
	// the whole transfer costs zero compression CPU and has an exact
	// Content-Length. Identity/gzip clients pay the decompress (+ gzip).
	if negotiateEncoding(r.Header.Get("Accept-Encoding")) == "zstd" {
		var clen int64
		for _, ref := range refs {
			clen += ref.CSize
		}
		w.Header().Set("Content-Encoding", "zstd")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", clen))
		_ = s.eachChunkOrderedRaw(r.Context(), refs, s.readAhead(), func(frame []byte) error {
			s.metrics.narBytes.Add(int64(len(frame)))
			_, werr := w.Write(frame)
			return werr
		})
		return
	}

	out, done := s.narWriter(w, r, p.NarSize)
	defer done()
	err = s.eachChunkOrdered(r.Context(), refs, s.readAhead(), func(raw []byte) error {
		s.metrics.narBytes.Add(int64(len(raw)))
		_, werr := out.Write(raw)
		return werr
	})
	if err != nil {
		return // headers/body already partially sent; best we can do is stop
	}
}

// narWriter picks a transfer encoding from Accept-Encoding and returns the
// writer to stream the NAR into plus a cleanup func. Raw transfers keep a
// Content-Length; gzip ones are chunked. (zstd never reaches here — it is
// served as stored frames in handleNar.)
func (s *Server) narWriter(w http.ResponseWriter, r *http.Request, narSize uint64) (io.Writer, func()) {
	switch negotiateEncoding(r.Header.Get("Accept-Encoding")) {
	case "gzip":
		w.Header().Set("Content-Encoding", "gzip")
		gz, _ := s.gzipPool.Get().(*gzip.Writer)
		if gz == nil {
			gz = gzip.NewWriter(w)
		} else {
			gz.Reset(w)
		}
		return gz, func() { gz.Close(); s.gzipPool.Put(gz) }
	default:
		w.Header().Set("Content-Length", fmt.Sprintf("%d", narSize))
		return w, func() {}
	}
}

// touchPath bumps a path's LRU stamp at most hourly. The in-memory gate
// means the hot path (mass narinfo queries on the same paths) costs one
// sync.Map read instead of a DB read + goroutine per request; the DB write
// still happens via store.TouchPath's own staleness check.
func (s *Server) touchPath(cacheID int64, storeHash string) {
	key := fmt.Sprintf("%d/%s", cacheID, storeHash)
	now := timeNow()
	if v, ok := s.touched.Load(key); ok && now-v.(int64) < 3600 {
		return
	}
	s.touched.Store(key, now)
	go s.db.TouchPath(cacheID, storeHash, now, 3600)
}

// contentETag derives an ETag from what the response actually contains: the
// NAR hash, plus the signing pubkey for narinfo (whose body embeds the
// signature). "sha256:<b32>" → a short quoted tag.
func contentETag(narHash, pubKey string) string {
	tag := strings.TrimPrefix(narHash, "sha256:")
	if pubKey != "" {
		sum := sha256.Sum256([]byte(narHash + "|" + pubKey))
		tag = hex.EncodeToString(sum[:16])
	}
	return `"` + tag + `"`
}

// negotiateEncoding prefers zstd, then gzip, honoring q=0 (explicit refusal).
func negotiateEncoding(accept string) string {
	acc := parseAcceptEncoding(accept)
	if acc["zstd"] {
		return "zstd"
	}
	if acc["gzip"] {
		return "gzip"
	}
	return ""
}

// parseAcceptEncoding returns the set of encodings the client accepts (q>0).
func parseAcceptEncoding(accept string) map[string]bool {
	out := map[string]bool{}
	for _, part := range strings.Split(accept, ",") {
		name := strings.TrimSpace(part)
		q := 1.0
		if i := strings.IndexByte(name, ';'); i >= 0 {
			params := name[i+1:]
			name = strings.TrimSpace(name[:i])
			for _, p := range strings.Split(params, ";") {
				if v, ok := strings.CutPrefix(strings.TrimSpace(p), "q="); ok {
					if f, err := strconv.ParseFloat(v, 64); err == nil {
						q = f
					}
				}
			}
		}
		if name != "" && q > 0 {
			out[name] = true
		}
	}
	return out
}
