package server

import (
	"context"
	"io"

	"github.com/stubbedev/xilo/internal/store"
)

// fetchChunk gets one compressed chunk from storage and decompresses it.
func (s *Server) fetchChunk(ctx context.Context, ref store.ChunkRef) ([]byte, error) {
	compressed, err := s.fetchChunkRaw(ctx, ref)
	if err != nil {
		return nil, err
	}
	return s.dec.DecodeAll(compressed, make([]byte, 0, ref.Size))
}

// fetchChunkRaw gets one chunk from storage as stored (a complete zstd frame).
func (s *Server) fetchChunkRaw(ctx context.Context, ref store.ChunkRef) ([]byte, error) {
	rc, err := s.st.Get(ctx, ref.Key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	buf := make([]byte, 0, ref.CSize)
	return readAllInto(buf, rc)
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
	results := make([]chan result, len(refs))
	launch := func(i int) {
		results[i] = make(chan result, 1)
		go func(i int) {
			data, err := fetch(ctx, refs[i])
			results[i] <- result{data, err}
		}(i)
	}
	for i := 0; i < len(refs) && i < ahead; i++ {
		launch(i)
	}
	for i := range refs {
		r := <-results[i]
		results[i] = nil
		if r.err != nil {
			return r.err
		}
		if err := fn(r.data); err != nil {
			return err
		}
		if j := i + ahead; j < len(refs) {
			launch(j)
		}
	}
	return nil
}

// readAhead is the chunk prefetch depth for serving/verification.
func (s *Server) readAhead() int {
	n := s.cfg.Parallelism
	if n < 4 {
		n = 4
	}
	return n
}
