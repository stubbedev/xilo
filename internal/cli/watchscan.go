//go:build linux || darwin

package cli

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"unsafe"

	memory "github.com/stubbedev/memory-go"
	"github.com/stubbedev/memory-go/hashmap"
)

type storeScan struct {
	storeDir   string
	alloc      memory.Local
	present    *hashmap.Strings[uint64]
	locked     map[string]struct{}
	listed     map[string]struct{}
	candidates []string
	epoch      uint64
	names      int
	primed     bool
}

func newStoreScan(storeDir string) (*storeScan, error) {
	s := &storeScan{storeDir: storeDir, locked: map[string]struct{}{}, listed: map[string]struct{}{}}
	present, err := hashmap.NewStrings[uint64](&s.alloc, 1<<12)
	if err != nil {
		return nil, fmt.Errorf("allocate the listing of %s: %w", storeDir, errors.Join(err, s.alloc.Close()))
	}
	s.present = present
	return s, nil
}

func (s *storeScan) Close() error { return s.alloc.Close() }

func (s *storeScan) begin() {
	s.epoch++
	s.names = 0
	clear(s.listed)
	s.candidates = s.candidates[:0]
}

func (s *storeScan) entry(name []byte) error {
	if string(name) == "." || string(name) == ".." {
		return nil
	}
	if base, ok := bytes.CutSuffix(name, []byte(lockSuffix)); ok {
		s.listed[string(base)] = struct{}{}
		s.locked[string(base)] = struct{}{}
		return nil
	}
	s.names++
	key := unsafe.String(unsafe.SliceData(name), len(name))
	fresh := true
	if last, ok := s.present.Ptr(key); ok {
		fresh = *last != s.epoch-1
		*last = s.epoch
	} else if err := s.present.Set(key, s.epoch); err != nil {
		return fmt.Errorf("record %s/%s: %w", s.storeDir, name, err)
	}
	if !s.primed {
		return nil
	}
	if _, held := s.locked[string(name)]; fresh || held {
		s.candidates = append(s.candidates, string(name))
	}
	return nil
}

func (s *storeScan) finish(onValid func(path string)) error {
	for base := range s.locked {
		if _, ok := s.listed[base]; !ok {
			delete(s.locked, base)
		}
	}
	for _, name := range s.candidates {
		if _, ok := s.listed[name]; ok {
			continue
		}
		path, ok := storePath(s.storeDir, name)
		if !ok {
			continue
		}
		_, err := os.Lstat(path + lockSuffix)
		if err == nil {
			s.locked[name] = struct{}{}
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("check the lock of %s: %w", path, err)
		}
		onValid(path)
	}
	s.primed = true
	if s.present.Len() > s.names {
		return s.forgetUnlisted()
	}
	return nil
}

func (s *storeScan) saw(name string) error {
	if last, ok := s.present.Ptr(name); ok {
		*last = s.epoch
		return nil
	}
	if err := s.present.Set(name, s.epoch); err != nil {
		return fmt.Errorf("record %s/%s: %w", s.storeDir, name, err)
	}
	return nil
}

func (s *storeScan) forgetUnlisted() error {
	var gone []string
	for name, epoch := range s.present.All() {
		if epoch != s.epoch {
			gone = append(gone, strings.Clone(name))
		}
	}
	for _, name := range gone {
		if _, err := s.present.Delete(name); err != nil {
			return fmt.Errorf("forget %s/%s: %w", s.storeDir, name, err)
		}
	}
	return nil
}

const (
	direntIno    = unsafe.Offsetof(syscall.Dirent{}.Ino)
	direntReclen = unsafe.Offsetof(syscall.Dirent{}.Reclen)
	direntName   = unsafe.Offsetof(syscall.Dirent{}.Name)
	listBufSize  = 1 << 20
)

type storeLister struct {
	dir string
	fd  int
	buf []byte
}

func openStoreLister(dir string) (*storeLister, error) {
	fd, err := syscall.Open(dir, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s for listing: %w", dir, err)
	}
	return &storeLister{dir: dir, fd: fd, buf: make([]byte, listBufSize)}, nil
}

func (l *storeLister) close() { syscall.Close(l.fd) }

func (l *storeLister) list(scan *storeScan, onValid func(path string)) error {
	scan.begin()
	if err := l.each(scan.entry); err != nil {
		return err
	}
	return scan.finish(onValid)
}

func (l *storeLister) rewind() error {
	if _, err := syscall.Seek(l.fd, 0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind %s: %w", l.dir, err)
	}
	return nil
}

func (l *storeLister) entries(recs []byte, fn func(name []byte) error) error {
	for len(recs) > 0 {
		if len(recs) < int(direntName) {
			return fmt.Errorf("list %s: truncated directory entry of %d bytes", l.dir, len(recs))
		}
		reclen := int(binary.NativeEndian.Uint16(recs[direntReclen:]))
		if reclen < int(direntName) || reclen > len(recs) {
			return fmt.Errorf("list %s: malformed directory entry of %d bytes with %d left", l.dir, reclen, len(recs))
		}
		rec := recs[:reclen]
		recs = recs[reclen:]
		if binary.NativeEndian.Uint64(rec[direntIno:]) == 0 {
			continue
		}
		namlen := direntNameLen(rec)
		if namlen < 0 || int(direntName)+namlen > reclen {
			return fmt.Errorf("list %s: directory entry name overruns its %d-byte record", l.dir, reclen)
		}
		if err := fn(rec[direntName : int(direntName)+namlen]); err != nil {
			return err
		}
	}
	return nil
}
