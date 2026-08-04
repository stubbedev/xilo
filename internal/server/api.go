package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
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
		if !s.db.Authorize(tok, c.Account, c.Name, "pull", now) && !s.db.Authorize(tok, c.Account, c.Name, "push", now) {
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
		AcceptZstd:   true,
		ChunkFilter:  true,
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
	if err := s.checkStorageQuota(c); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
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
	writeJSON(w, api.MissingResp{Missing: s.adopt(r, c, missing, req.Paths)})
}

// adopt copies, for each still-missing path the client offered a NarHash for, a
// bit-identical path another cache on the same storage backend already holds.
// The chunks are already stored there (dedup is per-backend), so this turns a
// full dump + chunk + upload into one row insert. Returns the paths still
// missing.
//
// Two things keep it safe. The NarHash must equal the stored one, and a NAR
// hash is a content hash: the copy is exactly what the client would have
// uploaded. And the caller must already be able to read the source cache —
// it's public, the token may pull it, or the token is an admin token. Without
// that check adoption would be a read primitive: knowing a store path and its
// NarHash would import another tenant's bytes, a far lower bar than put-path's
// proof of possession (which needs the exact chunk list, derivable only from
// the real NAR). Tokens are scoped to a single cache, so in practice this
// fires for public source caches and for admin-token pushes.
//
// Best-effort throughout: any failure just leaves the path in the missing list
// and the client pushes it the normal way.
func (s *Server) adopt(r *http.Request, c *store.Cache, missing []string, refs []api.PathRef) []string {
	if len(missing) == 0 || len(refs) == 0 {
		return missing
	}
	stillMissing := make(map[string]bool, len(missing))
	for _, h := range missing {
		stillMissing[h] = true
	}
	want := make(map[string]string, len(refs)) // store hash -> normalized NAR hash
	for _, ref := range refs {
		if !stillMissing[ref.Hash] {
			continue
		}
		if nh, err := narinfo.NarHash(ref.NarHash); err == nil {
			want[ref.Hash] = nh
		}
	}
	if len(want) == 0 {
		return missing
	}
	hashes := make([]string, 0, len(want))
	for h := range want {
		hashes = append(hashes, h)
	}
	cands, err := s.db.AdoptCandidates(c.Storage, c.ID, hashes)
	if err != nil {
		return missing
	}
	tok := extractToken(r)
	now := timeNow()
	isAdmin := tok != "" && s.db.AuthorizeAdmin(tok, now)
	adopted := 0
	for _, cd := range cands {
		if !stillMissing[cd.StoreHash] || want[cd.StoreHash] != cd.Path.NarHash {
			continue
		}
		if !cd.Public && !isAdmin && !s.db.Authorize(tok, cd.Account, cd.Cache, "pull", now) {
			continue
		}
		// Stamp before checking, like put-path: a chunk the sweeper takes in
		// between then reads as missing and we decline to adopt, rather than
		// registering a dangling path.
		if err := s.db.TouchChunks(c.Storage, cd.Path.Chunks, now); err != nil {
			continue
		}
		if miss, err := s.db.MissingChunks(c.Storage, cd.Path.Chunks); err != nil || len(miss) > 0 {
			continue
		}
		if err := s.db.PutPath(c.ID, cd.StoreHash, &cd.Path); err != nil {
			continue
		}
		stillMissing[cd.StoreHash] = false
		adopted++
		s.metrics.pathsAdopted.Add(1)
	}
	if adopted == 0 {
		return missing
	}
	out := make([]string, 0, len(missing)-adopted)
	for _, h := range missing {
		if stillMissing[h] {
			out = append(out, h)
		}
	}
	return out
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
	missing, err := s.db.MissingChunks(c.Storage, req.Hashes)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Everything we just promised as present will be skipped by the pusher —
	// re-stamp created so the GC grace window covers the rest of its push.
	if len(missing) < len(req.Hashes) {
		if err := s.db.TouchChunks(c.Storage, present(req.Hashes, missing), timeNow()); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	writeJSON(w, api.MissingResp{Missing: missing})
}

// present returns hashes minus the missing subset.
func present(hashes, missing []string) []string {
	miss := make(map[string]bool, len(missing))
	for _, h := range missing {
		miss[h] = true
	}
	out := make([]string, 0, len(hashes)-len(missing))
	for _, h := range hashes {
		if !miss[h] {
			out = append(out, h)
		}
	}
	return out
}

// handlePutChunk stores one chunk. The body is the raw chunk, or — when the
// client sets Content-Encoding: zstd — a zstd frame the fat client compressed
// so the wire carries far fewer bytes. Either way the server verifies the
// content hash of the RAW chunk. A pre-compressed body is stored as-is (the
// server skips its own encode, saving CPU on small boxes); a raw body is
// compressed at rest as before. Idempotent.
func (s *Server) handlePutChunk(w http.ResponseWriter, r *http.Request) {
	c, ok := s.cache(w, r)
	if !ok {
		return
	}
	if !s.requirePush(w, r, c) {
		return
	}
	want := r.PathValue("hash")

	body, err := io.ReadAll(io.LimitReader(r.Body, s.maxChunkBody()))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// raw is what we hash; compressed (if non-nil) is the blob to store as-is.
	var raw, compressed []byte
	switch enc := r.Header.Get("Content-Encoding"); enc {
	case "":
		raw = body
	case "zstd":
		// s.dec is capped via WithDecoderMaxMemory, so a bomb can't blow the
		// heap; the explicit length check backstops it. Endpoint is push-authed.
		compressed = body
		if raw, err = s.dec.DecodeAll(body, nil); err != nil {
			http.Error(w, "zstd decode: "+err.Error(), http.StatusBadRequest)
			return
		}
		if int64(len(raw)) > s.maxChunkBody() {
			http.Error(w, "chunk too large", http.StatusBadRequest)
			return
		}
	default:
		http.Error(w, "unsupported Content-Encoding: "+enc, http.StatusBadRequest)
		return
	}
	sum := sha256.Sum256(raw)
	got := hex.EncodeToString(sum[:])
	if got != want {
		http.Error(w, fmt.Sprintf("chunk hash mismatch: want %s got %s", want, got), http.StatusBadRequest)
		return
	}

	// Skip a chunk already recorded (row+blob present) — idempotent, saves the
	// compress+write. Checking the DB row (not just the blob) keeps them
	// consistent. Re-stamp created: this client will rely on the chunk staying.
	if s.db.HasChunk(c.Storage, want) {
		_ = s.db.TouchChunks(c.Storage, []string{want}, timeNow())
		s.metrics.chunksDedup.Add(1)
		w.WriteHeader(http.StatusOK)
		return
	}

	// Bound concurrent encode+store to cap memory — acquired AFTER the body
	// read so a slow uploader link can't starve fast pushers of slots.
	s.uploadSem <- struct{}{}
	defer func() { <-s.uploadSem }()

	key := storage.ChunkKey(want)
	if compressed == nil { // raw upload: compress at rest ourselves
		compressed = s.enc.EncodeAll(raw, nil)
	}
	if err := s.stOf(c.Storage).Put(r.Context(), key, bytes.NewReader(compressed)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.db.PutChunk(c.Storage, want, int64(len(raw)), int64(len(compressed)), key, timeNow()); err != nil {
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
	if err := s.checkStorageQuota(c); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	var req api.PathReq
	if !decodeJSON(w, r, &req) {
		return
	}
	// A NarSize past MaxInt64 wraps negative when summed for quota accounting
	// (stored as int64), so an over-quota push could slip through. Reject it
	// here unconditionally — the reassembly check below is skippable, this is
	// not. No real NAR approaches 2^63 bytes.
	if req.NarSize > math.MaxInt64 {
		http.Error(w, "narSize too large", http.StatusBadRequest)
		return
	}

	// Re-stamp all referenced chunks BEFORE checking presence: from here to the
	// PutPath commit the GC grace window must cover them, and the stamp-then-
	// check order means a chunk the sweeper deletes in between is reported
	// missing (client re-uploads) instead of registered dangling.
	if err := s.db.TouchChunks(c.Storage, req.Chunks, timeNow()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	missing, err := s.db.MissingChunks(c.Storage, req.Chunks)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(missing) > 0 {
		// 409 + the actual hashes, so an optimistic pusher (one that skipped
		// asking, trusting the presence filter or a cached manifest) can tell
		// this apart from a real error and retry the path pessimistically.
		// Capped: a client only needs to know that it guessed wrong.
		writeJSONStatus(w, http.StatusConflict, api.MissingResp{Missing: missing[:min(len(missing), 1000)]})
		return
	}

	narHash, err := narinfo.NarHash(req.NarHash)
	if err != nil {
		http.Error(w, "bad narHash: "+err.Error(), http.StatusBadRequest)
		return
	}

	// The narinfo format is newline-delimited; reject any store path/reference/
	// deriver that isn't well-formed so a push can't inject extra header lines
	// (e.g. a second Sig or a bogus URL) into every served narinfo for this path.
	if !narinfo.ValidStorePath(req.StorePath) {
		http.Error(w, "invalid storePath", http.StatusBadRequest)
		return
	}
	for _, ref := range req.References {
		if !narinfo.ValidStorePath(ref) {
			http.Error(w, "invalid reference", http.StatusBadRequest)
			return
		}
	}
	if req.Deriver != "" && !narinfo.ValidStoreName(narinfo.BaseName(req.Deriver)) {
		http.Error(w, "invalid deriver", http.StatusBadRequest)
		return
	}

	// Proof of possession: the chunk list must actually reassemble to the
	// claimed NarHash. A client without the real NAR cannot produce a chunk
	// list that hashes correctly, so it cannot claim someone else's path.
	//
	// The dedup pool is shared across tenants, so this check is the ONLY thing
	// stopping one tenant from registering a path that references another
	// tenant's chunk hashes and reading their private bytes. Never skip it in
	// multi-tenant mode, regardless of the operator's SkipUploadVerify setting.
	if s.cfg.MultiTenant || !s.cfg.Security.SkipUploadVerify {
		if err := s.verifyReassembly(r, c.Storage, req.Chunks, narHash, req.NarSize); err != nil {
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
func (s *Server) verifyReassembly(r *http.Request, storageName string, chunkHashes []string, narHash string, narSize uint64) error {
	refs, err := s.db.ChunkKeys(storageName, chunkHashes)
	if err != nil {
		return err
	}
	h := sha256.New()
	var total uint64
	err = s.eachChunkOrdered(r.Context(), refs, s.readAhead(), func(raw []byte) error {
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

// writeJSONStatus is writeJSON with an explicit status; the header has to be
// set before WriteHeader or it's dropped.
func writeJSONStatus(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
