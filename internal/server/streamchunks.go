package server

import (
	"context"
	"io"
	"math/bits"
	"sync"

	"github.com/klauspost/compress/zstd"

	"github.com/stubbedev/xilo/internal/store"
)

// chunkBufPools recycle chunk-sized buffers across requests, one pool per
// power-of-two size class. Under parallel pulls the prefetch pipeline otherwise
// allocates (raw + compressed) per chunk and leaves the garbage to GC.
var chunkBufPools [chunkBufMaxShift - chunkBufMinShift + 1]sync.Pool

const (
	chunkBufMinShift = 12
	chunkBufMaxShift = 22
)

func chunkBufClass(n int64) (int, bool) {
	if n > 1<<chunkBufMaxShift {
		return 0, false
	}
	return max(bits.Len64(uint64(max(n, 1)-1)), chunkBufMinShift) - chunkBufMinShift, true
}

func getChunkBuf(capHint int64) []byte {
	class, ok := chunkBufClass(capHint)
	if !ok {
		return make([]byte, 0, capHint)
	}
	if b, ok := chunkBufPools[class].Get().(*[]byte); ok {
		return (*b)[:0]
	}
	return make([]byte, 0, 1<<(class+chunkBufMinShift))
}

func putChunkBuf(b []byte) {
	class, ok := chunkBufClass(int64(cap(b)))
	if !ok || cap(b) != 1<<(class+chunkBufMinShift) {
		return
	}
	chunkBufPools[class].Put(&b)
}

func decodedSizeHint(frame []byte, limit int64) int64 {
	var h zstd.Header
	if h.Decode(frame) != nil || !h.HasFCS {
		return 0
	}
	return int64(min(h.FrameContentSize, uint64(limit)))
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
	buf, err := readAllInto(getChunkBuf(ref.CSize+1), rc)
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

type window struct {
	depth  int
	extra  int
	shared chan struct{}
}

// eachChunkOrdered fetches+decompresses chunks and calls fn with each raw
// chunk STRICTLY IN ORDER, while prefetching a window of chunks concurrently.
// On S3 this overlaps GET latency instead of paying it serially. Used by both
// NAR serving and upload verification.
func (s *Server) eachChunkOrdered(ctx context.Context, refs []store.ChunkRef, w window, fn func(raw []byte) error) error {
	return eachOrdered(ctx, refs, w, s.fetchChunk, fn)
}

// eachChunkOrderedRaw is eachChunkOrdered without the decompression: chunks
// are delivered as the stored zstd frames.
func (s *Server) eachChunkOrderedRaw(ctx context.Context, refs []store.ChunkRef, w window, fn func(frame []byte) error) error {
	return eachOrdered(ctx, refs, w, s.fetchChunkRaw, fn)
}

func eachOrdered(ctx context.Context, refs []store.ChunkRef, w window,
	fetch func(context.Context, store.ChunkRef) ([]byte, error), fn func([]byte) error) error {
	type result struct {
		data []byte
		err  error
	}
	results := make([]chan result, len(refs))
	depth := max(w.depth, 1)
	borrowed, next := 0, 0
	defer func() {
		for range borrowed {
			<-w.shared
		}
	}()
	fill := func(delivered int) {
		for next < len(refs) {
			if next-delivered >= depth+borrowed {
				if borrowed >= w.extra {
					return
				}
				select {
				case w.shared <- struct{}{}:
					borrowed++
				default:
					return
				}
			}
			results[next] = make(chan result, 1)
			go func(i int) {
				data, err := fetch(ctx, refs[i])
				results[i] <- result{data, err}
			}(next)
			next++
		}
	}
	fill(0)
	for i := range refs {
		r := <-results[i]
		results[i] = nil
		if r.err != nil {
			return r.err
		}
		err := fn(r.data)
		putChunkBuf(r.data)
		if err != nil {
			return err
		}
		fill(i + 1)
	}
	return nil
}

const (
	remoteAhead = 16
	sharedAhead = 48
)

// readAhead is the chunk prefetch window for serving/verification from the
// named backend. Deep prefetch exists to hide per-read latency; a local disk
// has none worth hiding, so a shallow window there halves pull-path memory at
// no throughput cost. A local root on a network mount is latency-bound like
// S3 and gets the deep window: at 4 in flight a CIFS-mounted root served a
// cold NAR at 2.7 MB/s.
func (s *Server) readAhead(storageName string) window {
	if !s.stOf(storageName).LatencyBound() {
		return window{depth: 4}
	}
	depth := max(s.cfg.Parallelism, 4)
	return window{depth: depth, extra: max(remoteAhead-depth, 0), shared: s.sharedAhead}
}
