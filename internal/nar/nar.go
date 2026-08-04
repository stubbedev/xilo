// Package nar serializes a filesystem path into the Nix Archive (NAR) format,
// byte-for-byte identically to `nix-store --dump`.
//
// The push client used to shell out to `nix-store --dump` once per store path.
// A closure is thousands of paths, so that is thousands of fork+exec pairs and
// nix-daemon round trips before a single byte is hashed, and the whole-NAR
// variant buffered the archive in memory. Producing it here is a filesystem
// walk instead.
//
// The format is fully specified by what nix writes: every field is a string
// (u64 little-endian length, bytes, zero padding to the next 8-byte boundary),
// and a node is a parenthesized sequence of them. Directory entries are emitted
// in byte order of their names, which is what makes the output deterministic.
package nar

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// Magic is the archive header string.
const Magic = "nix-archive-1"

// Dump writes the NAR serialization of path to w. Symlinks are archived as
// symlinks (never followed), and anything that is not a regular file, directory
// or symlink is an error — nix refuses those too, so a store path cannot
// contain one.
func Dump(w io.Writer, path string) error {
	e := &encoder{w: w}
	e.str(Magic)
	if err := e.node(path); err != nil {
		return err
	}
	return e.err
}

type encoder struct {
	w   io.Writer
	err error
}

// write is a no-op once something has failed, so the walk can stay linear and
// check err at the end.
func (e *encoder) write(b []byte) {
	if e.err != nil {
		return
	}
	_, e.err = e.w.Write(b)
}

// str writes one NAR string: length, bytes, padding to an 8-byte boundary.
func (e *encoder) str(s string) {
	var hdr [8]byte
	binary.LittleEndian.PutUint64(hdr[:], uint64(len(s)))
	e.write(hdr[:])
	e.write([]byte(s))
	e.pad(len(s))
}

var zeros [8]byte

func (e *encoder) pad(n int) {
	if r := n % 8; r != 0 {
		e.write(zeros[:8-r])
	}
}

func (e *encoder) node(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	e.str("(")
	e.str("type")
	switch {
	case fi.Mode().IsRegular():
		e.str("regular")
		// Nix records one bit of mode: executable or not.
		if fi.Mode().Perm()&0o111 != 0 {
			e.str("executable")
			e.str("")
		}
		e.str("contents")
		if err := e.contents(path, fi.Size()); err != nil {
			return err
		}
	case fi.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			return err
		}
		e.str("symlink")
		e.str("target")
		e.str(target)
	case fi.IsDir():
		e.str("directory")
		names, err := dirNames(path)
		if err != nil {
			return err
		}
		for _, name := range names {
			e.str("entry")
			e.str("(")
			e.str("name")
			e.str(name)
			e.str("node")
			if err := e.node(filepath.Join(path, name)); err != nil {
				return err
			}
			e.str(")")
		}
	default:
		return fmt.Errorf("nar: %s: unsupported file type %s", path, fi.Mode().Type())
	}
	e.str(")")
	return e.err
}

// contents streams the file with its size declared up front, so it never has to
// be buffered. A file that changed size underneath us would desync the archive,
// so that is an error rather than a short or padded write — store paths are
// immutable, so it means something is very wrong.
func (e *encoder) contents(path string, size int64) error {
	var hdr [8]byte
	binary.LittleEndian.PutUint64(hdr[:], uint64(size))
	e.write(hdr[:])
	if e.err != nil {
		return e.err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := io.Copy(e.w, io.LimitReader(f, size))
	if err != nil {
		return err
	}
	if n != size {
		return fmt.Errorf("nar: %s: read %d bytes, expected %d (file changed?)", path, n, size)
	}
	// Anything left means the file grew; the archive would be wrong.
	if extra, err := io.Copy(io.Discard, f); err == nil && extra > 0 {
		return fmt.Errorf("nar: %s: file grew by %d bytes while being archived", path, extra)
	}
	e.pad(int(size % 8))
	return e.err
}

// dirNames lists a directory's entries sorted by byte order — the order nix
// uses, and the reason two dumps of the same tree are identical.
func dirNames(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	names, err := f.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}
