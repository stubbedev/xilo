package server

import (
	"context"
	"io"
)

// fetchChunk gets one compressed chunk from storage and decompresses it.
func (s *Server) fetchChunk(ctx context.Context, key string) ([]byte, error) {
	rc, err := s.st.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	compressed, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	return s.dec.DecodeAll(compressed, nil)
}

// eachChunkOrdered fetches+decompresses chunks for keys and calls fn with each
// raw chunk STRICTLY IN ORDER, while prefetching up to `ahead` chunks
// concurrently. On S3 this overlaps GET latency instead of paying it serially,
// and memory stays bounded to ~ahead chunks. Used by both NAR serving and
// upload verification.
func (s *Server) eachChunkOrdered(ctx context.Context, keys []string, ahead int, fn func(raw []byte) error) error {
	if ahead < 1 {
		ahead = 1
	}
	type result struct {
		data []byte
		err  error
	}
	results := make([]chan result, len(keys))
	launch := func(i int) {
		results[i] = make(chan result, 1)
		go func(i int) {
			data, err := s.fetchChunk(ctx, keys[i])
			results[i] <- result{data, err}
		}(i)
	}
	for i := 0; i < len(keys) && i < ahead; i++ {
		launch(i)
	}
	for i := range keys {
		r := <-results[i]
		results[i] = nil
		if r.err != nil {
			return r.err
		}
		if err := fn(r.data); err != nil {
			return err
		}
		if j := i + ahead; j < len(keys) {
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
