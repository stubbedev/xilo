// Package storage is the content-addressed blob backend for chunks. Bytes live
// here (local FS or S3), never in the metadata DB — that is what keeps pushes
// lock-free: chunk writes are idempotent Puts that never touch SQLite.
package storage

import (
	"context"
	"fmt"
	"io"

	"github.com/stubbedev/xilo/internal/config"
)

// Storage stores opaque blobs by key. Keys are content-addressed
// ("chunk/ab/abcd…"), so Put is idempotent: writing the same key twice is a
// no-op with identical bytes.
type Storage interface {
	Put(ctx context.Context, key string, r io.Reader) error
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	Has(ctx context.Context, key string) (bool, error)
	Delete(ctx context.Context, key string) error
}

// ChunkKey is the storage key for a chunk given its sha256 hex hash. Sharded by
// the first two hex chars so no single directory/prefix holds every chunk.
func ChunkKey(hash string) string {
	if len(hash) < 2 {
		return "chunk/" + hash
	}
	return "chunk/" + hash[:2] + "/" + hash
}

// New builds the Storage backend selected by cfg.Storage.Backend.
func New(cfg config.Storage) (Storage, error) {
	switch cfg.Backend {
	case "", "local":
		return NewLocal(cfg.Local.Root)
	case "s3":
		return NewS3(cfg.S3)
	default:
		return nil, fmt.Errorf("unknown storage backend %q (want local|s3)", cfg.Backend)
	}
}
