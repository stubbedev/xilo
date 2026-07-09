package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/stubbedev/xilo/internal/api"
	"github.com/stubbedev/xilo/internal/narinfo"
	"github.com/stubbedev/xilo/internal/storage"
	"github.com/stubbedev/xilo/internal/store"
)

// maxJSONBody caps hash-list request bodies (get-missing-*, put-path) to guard
// against memory-exhaustion; a hash is ~64 bytes so this holds ~150k of them.
const maxJSONBody = 16 << 20

func timeNow() int64 { return time.Now().Unix() }

// maxChunkBody is the per-upload cap for a single chunk. Derived from the
// server's configured chunking bounds so raising max_size/nar_threshold above
// the old hardcoded 4 MiB doesn't silently truncate uploads.
func (s *Server) maxChunkBody() int64 {
	n := s.cfg.Chunking.MaxSize
	if s.cfg.Chunking.NarThreshold > n {
		n = s.cfg.Chunking.NarThreshold
	}
	return int64(n) + (1 << 20) // slack
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	c, ok := s.cache(w, r)
	if !ok {
		return
	}
	// Private caches: don't leak existence/pubkey to anonymous callers. A push
	// OR pull token suffices (push clients need config too).
	if !c.Public {
		tok := extractToken(r)
		now := timeNow()
		if !s.db.Authorize(tok, c.Name, "pull", now) && !s.db.Authorize(tok, c.Name, "push", now) {
			unauthorized(w)
			s.metrics.authFailures.Add(1)
			return
		}
	}
	writeJSON(w, api.ConfigResp{
		MinSize:      s.cfg.Chunking.MinSize,
		AvgSize:      s.cfg.Chunking.AvgSize,
		MaxSize:      s.cfg.Chunking.MaxSize,
		NarThreshold: s.cfg.Chunking.NarThreshold,
		Parallelism:  s.cfg.Parallelism,
		UpstreamKeys: s.cfg.UpstreamKeys,
		PublicKey:    c.PubKey,
		Public:       c.Public,
	})
}

func (s *Server) handleMissingPaths(w http.ResponseWriter, r *http.Request) {
	c, ok := s.cache(w, r)
	if !ok {
		return
	}
	if !s.requirePush(w, r, c) {
		return
	}
	var req api.MissingReq
	if !decodeJSON(w, r, &req) {
		return
	}
	missing, err := s.db.MissingPaths(c.ID, req.Hashes)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, api.MissingResp{Missing: missing})
}

func (s *Server) handleMissingChunks(w http.ResponseWriter, r *http.Request) {
	c, ok := s.cache(w, r)
	if !ok {
		return
	}
	if !s.requirePush(w, r, c) {
		return
	}
	var req api.MissingReq
	if !decodeJSON(w, r, &req) {
		return
	}
	missing, err := s.db.MissingChunks(req.Hashes)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, api.MissingResp{Missing: missing})
}

// handlePutChunk stores one chunk. Body is the raw (uncompressed) chunk; the
// server verifies the content hash, then compresses it at rest. Idempotent.
func (s *Server) handlePutChunk(w http.ResponseWriter, r *http.Request) {
	c, ok := s.cache(w, r)
	if !ok {
		return
	}
	if !s.requirePush(w, r, c) {
		return
	}
	want := r.PathValue("hash")

	// Bound concurrent read+encode across all pushers to cap memory.
	s.uploadSem <- struct{}{}
	defer func() { <-s.uploadSem }()

	raw, err := io.ReadAll(io.LimitReader(r.Body, s.maxChunkBody()))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	sum := sha256.Sum256(raw)
	got := hex.EncodeToString(sum[:])
	if got != want {
		http.Error(w, fmt.Sprintf("chunk hash mismatch: want %s got %s", want, got), http.StatusBadRequest)
		return
	}

	// Skip a chunk already recorded (row+blob present) — idempotent, saves the
	// compress+write. Checking the DB row (not just the blob) keeps them consistent.
	if s.db.HasChunk(want) {
		s.metrics.chunksDedup.Add(1)
		w.WriteHeader(http.StatusOK)
		return
	}

	key := storage.ChunkKey(want)
	compressed := s.enc.EncodeAll(raw, nil)
	if err := s.st.Put(r.Context(), key, bytes.NewReader(compressed)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.db.PutChunk(want, int64(len(raw)), int64(len(compressed)), key, timeNow()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.metrics.chunksRecv.Add(1)
	w.WriteHeader(http.StatusOK)
}

// handlePutPath registers a store path after its chunks are uploaded.
func (s *Server) handlePutPath(w http.ResponseWriter, r *http.Request) {
	c, ok := s.cache(w, r)
	if !ok {
		return
	}
	if !s.requirePush(w, r, c) {
		return
	}
	var req api.PathReq
	if !decodeJSON(w, r, &req) {
		return
	}

	// All referenced chunks must already be present.
	missing, err := s.db.MissingChunks(req.Chunks)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(missing) > 0 {
		http.Error(w, fmt.Sprintf("path references %d unuploaded chunks", len(missing)), http.StatusBadRequest)
		return
	}

	narHash, err := narinfo.NarHash(req.NarHash)
	if err != nil {
		http.Error(w, "bad narHash: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Proof of possession: the chunk list must actually reassemble to the
	// claimed NarHash. A client without the real NAR cannot produce a chunk
	// list that hashes correctly, so it cannot claim someone else's path.
	if !s.cfg.Security.SkipUploadVerify {
		if err := s.verifyReassembly(r, req.Chunks, narHash, req.NarSize); err != nil {
			http.Error(w, "upload verification failed: "+err.Error(), http.StatusBadRequest)
			return
		}
	}

	p := &store.Path{
		StorePath: req.StorePath,
		NarHash:   narHash,
		NarSize:   req.NarSize,
		Deriver:   narinfo.BaseName(req.Deriver),
		Refs:      req.References,
		Chunks:    req.Chunks,
	}
	if err := s.db.PutPath(c.ID, narinfo.StoreHash(req.StorePath), p); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.metrics.pathsPushed.Add(1)
	w.WriteHeader(http.StatusOK)
}

// verifyReassembly streams the referenced chunks through sha256 (fetched with
// bounded look-ahead) and checks the digest + total size against the claimed
// NarHash/NarSize.
func (s *Server) verifyReassembly(r *http.Request, chunkHashes []string, narHash string, narSize uint64) error {
	keys, err := s.db.ChunkKeys(chunkHashes)
	if err != nil {
		return err
	}
	h := sha256.New()
	var total uint64
	err = s.eachChunkOrdered(r.Context(), keys, s.readAhead(), func(raw []byte) error {
		h.Write(raw)
		total += uint64(len(raw))
		return nil
	})
	if err != nil {
		return err
	}
	if total != narSize {
		return fmt.Errorf("nar size mismatch: got %d want %d", total, narSize)
	}
	got := "sha256:" + narinfo.Base32Encode(h.Sum(nil))
	if got != narHash {
		return fmt.Errorf("nar hash mismatch")
	}
	return nil
}

// ---- helpers ----

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody)).Decode(v); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
