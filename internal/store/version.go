package store

import (
	"encoding/binary"
	"fmt"
	"os"
	"syscall"
)

const (
	walIndexHeaderSize = 48
	walIndexFormat     = 3007000
)

type Version [walIndexHeaderSize]byte

func (db *DB) Version() (Version, bool) {
	if db.wal == nil {
		return Version{}, false
	}
	return Version(db.wal.header), true
}

type walIndex struct{ header []byte }

func openWALIndex(dbPath string) (*walIndex, error) {
	path := dbPath + "-shm"
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open wal-index: %w", err)
	}
	defer f.Close()
	header, err := syscall.Mmap(int(f.Fd()), 0, walIndexHeaderSize, syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("map wal-index %s: %w", path, err)
	}
	if got := binary.NativeEndian.Uint32(header); got != walIndexFormat {
		_ = syscall.Munmap(header)
		return nil, fmt.Errorf("wal-index %s has format %d, want %d", path, got, walIndexFormat)
	}
	return &walIndex{header: header}, nil
}
