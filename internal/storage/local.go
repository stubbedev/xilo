package storage

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// Local stores blobs as files under Root. Writes are atomic (temp file +
// rename) so a crashed push never leaves a half-written chunk.
type Local struct{ root string }

func NewLocal(root string) (*Local, error) {
	if root == "" {
		return nil, errors.New("storage.local.root is required")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &Local{root: root}, nil
}

func (l *Local) path(key string) string { return filepath.Join(l.root, filepath.FromSlash(key)) }

func (l *Local) Put(ctx context.Context, key string, r io.Reader) error {
	dst := l.path(key)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		return err
	}
	// fsync before rename: a power loss must never leave a truncated blob
	// behind a durable DB row — dedup would then trust the poisoned chunk
	// forever (it is never re-uploaded and never GC'd while referenced).
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return err
	}
	return syncDir(filepath.Dir(dst))
}

// syncDir fsyncs a directory so a just-renamed file survives power loss.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (l *Local) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return os.Open(l.path(key))
}

func (l *Local) Has(ctx context.Context, key string) (bool, error) {
	_, err := os.Stat(l.path(key))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func (l *Local) Delete(ctx context.Context, key string) error {
	err := os.Remove(l.path(key))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// DeleteMany just loops: a local unlink is cheap, there is no batch syscall.
func (l *Local) DeleteMany(ctx context.Context, keys []string) error {
	for _, k := range keys {
		if err := l.Delete(ctx, k); err != nil {
			return err
		}
	}
	return nil
}
