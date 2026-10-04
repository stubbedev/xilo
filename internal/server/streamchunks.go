package server

import (
	"context"
	"io"
	"sync"
	"sync/atomic"

	"github.com/stubbedev/xilo/internal/store"
)

// chunkBufPool recycles chunk-sized buffers across requests. Under parallel
// pulls the prefetch pipeline otherwise allocates (raw + compressed) per
// chunk and leaves the garbage to GC — pooling keeps steady-state RSS flat.
var chunkBufPool = sync.Pool{New: func() any { b := make([]byte, 0, 1<<20); return &b }}

func getChunkBuf(capHint int64) []byte {
	b := *(chunkBufPool.Get().(*[]byte))
	if int64(cap(b)) < capHint {
		return make([]byte, 0, capHint)
	}
	return b[:0]
}

func putChunkBuf(b []byte) {
	if cap(b) > 4<<20 { // don't hoard oversized one-offs
		return
	}
	chunkBufPool.Put(&b)
}

// fetchChunk gets one compressed chunk from storage and decompresses it.
// The returned buffer is pool-owned: eachOrdered returns it to the pool after
// the consumer callback runs.
func (s *Server) fetchChunk(ctx context.Context, ref store.ChunkRef) ([]byte, error) {
	compressed, err := s.fetchChunkRaw(ctx, ref)
	if err != nil {
		return nil, err
	}
	raw, err := s.dec.DecodeAll(compressed, getChunkBuf(ref.Size))
	putChunkBuf(compressed)
	if err != nil {
		putChunkBuf(raw)
		return nil, err
	}
	return raw, nil
}

// fetchChunkRaw gets one chunk from storage as stored (a complete zstd frame).
// The returned buffer is pool-owned (see fetchChunk).
func (s *Server) fetchChunkRaw(ctx context.Context, ref store.ChunkRef) ([]byte, error) {
	rc, err := s.stOf(ref.Storage).Get(ctx, ref.Key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	buf, err := readAllInto(getChunkBuf(ref.CSize), rc)
	if err != nil {
		putChunkBuf(buf)
		return nil, err
	}
	return buf, nil
}

// readAllInto is io.ReadAll into a pre-sized buffer (csize is known from the
// DB, so the usual grow-and-copy churn is avoidable).
func readAllInto(buf []byte, r io.Reader) ([]byte, error) {
	for {
		if len(buf) == cap(buf) {
			buf = append(buf, 0)[:len(buf)]
		}
		n, err := r.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if err == io.EOF {
			return buf, nil
		}
		if err != nil {
			return buf, err
		}
	}
}

// eachChunkOrdered fetches+decompresses chunks and calls fn with each raw
// chunk STRICTLY IN ORDER, while prefetching up to `ahead` chunks
// concurrently. On S3 this overlaps GET latency instead of paying it serially,
// and memory stays bounded to ~ahead chunks. Used by both NAR serving and
// upload verification.
func (s *Server) eachChunkOrdered(ctx context.Context, refs []store.ChunkRef, ahead int, fn func(raw []byte) error) error {
	return eachOrdered(ctx, refs, ahead, s.fetchChunk, fn)
}

// eachChunkOrderedRaw is eachChunkOrdered without the decompression: chunks
// are delivered as the stored zstd frames.
func (s *Server) eachChunkOrderedRaw(ctx context.Context, refs []store.ChunkRef, ahead int, fn func(frame []byte) error) error {
	return eachOrdered(ctx, refs, ahead, s.fetchChunkRaw, fn)
}

func eachOrdered(ctx context.Context, refs []store.ChunkRef, ahead int,
	fetch func(context.Context, store.ChunkRef) ([]byte, error), fn func([]byte) error) error {
	if ahead < 1 {
		ahead = 1
	}
	type result struct {
		data []byte
		err  error
	}
	results := make([]result, len(refs))
	var next atomic.Int64 // next index a worker may fetch
	var stop atomic.Bool  // the first failure; workers stop taking more
	// Completions carry their index: receiving i synchronizes with the
	// worker that filled results[i], which is what makes the in-order
	// reads below race-free. The buffer holds every index, so a worker
	// never blocks on send, even once the consumer stopped reading after
	// an error.
	done := make(chan int, len(refs))
	var wg sync.WaitGroup
	for range min(ahead, len(refs)) {
		wg.Go(func() {
			for !stop.Load() {
				i := int(next.Add(1)) - 1
				if i >= len(refs) {
					return
				}
				data, err := fetch(ctx, refs[i])
				results[i] = result{data, err}
				done <- i
			}
		})
	}
	consume := func(i int) error {
		r := results[i]
		results[i] = result{}
		if r.err != nil {
			return r.err
		}
		err := fn(r.data)
		putChunkBuf(r.data)
		return err
	}
	// got tracks which completions have been received, so events for later
	// chunks can arrive first without being lost.
	got := make([]bool, len(refs))
	for i := range refs {
		for !got[i] {
			got[<-done] = true
		}
		if err := consume(i); err != nil {
			stop.Store(true)
			return err
		}
	}
	return nil
}

// readAhead is the chunk prefetch depth for serving/verification. Deep
// prefetch exists to hide per-object GET latency; local disk has none worth
// hiding, so a shallow window there halves pull-path memory at no throughput
// cost.
func (s *Server) readAhead() int {
	if s.cfg.Storage.Backend == "local" {
		return 4
	}
	n := max(s.cfg.Parallelism, 4)
	return n
}
