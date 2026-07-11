// Package chunk splits a byte stream into content-defined chunks (FastCDC) so
// identical byte runs across different NARs dedup to the same content-addressed
// chunk. Shared by the push client and the server.
package chunk

import (
	"crypto/sha256"
	"encoding/hex"
	"io"

	fastcdc "github.com/jotfs/fastcdc-go"
)

// Default chunk sizes tuned for NARs: big enough to keep per-chunk overhead
// low, small enough to still dedup shared file content.
const (
	MinSize = 64 << 10
	AvgSize = 256 << 10
	MaxSize = 1 << 20
)

// Params are the content-defined chunking parameters. The server dictates them
// (via GET /{cache}/api/chunking) so every push client chunks identically and
// dedup stays global.
type Params struct {
	MinSize int
	AvgSize int
	MaxSize int
}

// Default returns the built-in chunking parameters.
func Default() Params { return Params{MinSize: MinSize, AvgSize: AvgSize, MaxSize: MaxSize} }

// Chunk is one content-defined chunk: its sha256 (hex) and raw bytes.
type Chunk struct {
	Hash string
	Data []byte
}

// Hash returns the content hash for arbitrary bytes (used for whole-NAR chunks
// below the chunking threshold).
func Hash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Split reads r to EOF, calling fn for each chunk in order. fn owns Data (it is
// a fresh copy). Boundaries are deterministic: same input + params → same chunks.
func Split(r io.Reader, p Params, fn func(Chunk) error) error {
	return split(r, p, func(hash string, data []byte) error {
		cp := make([]byte, len(data)) // Next() reuses its buffer; copy out
		copy(cp, data)
		return fn(Chunk{Hash: hash, Data: cp})
	})
}

// SplitHashes is like Split but only reports the ordered chunk hashes, without
// copying chunk bytes — for callers that need the hash list but not the data.
func SplitHashes(r io.Reader, p Params, fn func(hash string) error) error {
	return split(r, p, func(hash string, _ []byte) error { return fn(hash) })
}

// SplitRaw is like Split but hands fn a transient buffer: data is only valid
// during the call (the underlying chunker reuses it), so fn must copy any
// bytes it wants to keep. This lets the push client copy only the chunks it
// may actually upload instead of every chunk in the stream.
func SplitRaw(r io.Reader, p Params, fn func(hash string, data []byte) error) error {
	return split(r, p, fn)
}

func split(r io.Reader, p Params, fn func(hash string, data []byte) error) error {
	ch, err := fastcdc.NewChunker(r, fastcdc.Options{
		MinSize:     p.MinSize,
		AverageSize: p.AvgSize,
		MaxSize:     p.MaxSize,
	})
	if err != nil {
		return err
	}
	for {
		c, err := ch.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		sum := sha256.Sum256(c.Data)
		if err := fn(hex.EncodeToString(sum[:]), c.Data); err != nil {
			return err
		}
	}
}
