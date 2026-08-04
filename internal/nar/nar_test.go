package nar

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// str is the expected wire form of one NAR string, used to hand-build the
// golden archives below.
func str(s string) []byte {
	var out []byte
	var hdr [8]byte
	binary.LittleEndian.PutUint64(hdr[:], uint64(len(s)))
	out = append(out, hdr[:]...)
	out = append(out, s...)
	if r := len(s) % 8; r != 0 {
		out = append(out, make([]byte, 8-r)...)
	}
	return out
}

func cat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func dumpTo(t *testing.T, path string) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := Dump(&buf, path); err != nil {
		t.Fatalf("Dump(%s): %v", path, err)
	}
	return buf.Bytes()
}

func TestDumpRegularFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	if err := os.WriteFile(p, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	want := cat(str(Magic), str("("), str("type"), str("regular"), str("contents"), str("hello"), str(")"))
	if got := dumpTo(t, p); !bytes.Equal(got, want) {
		t.Fatalf("regular file:\n got %x\nwant %x", got, want)
	}
}

func TestDumpExecutableAndEmpty(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "x")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	want := cat(str(Magic), str("("), str("type"), str("regular"),
		str("executable"), str(""), str("contents"), str("#!/bin/sh\n"), str(")"))
	if got := dumpTo(t, exe); !bytes.Equal(got, want) {
		t.Fatalf("executable:\n got %x\nwant %x", got, want)
	}

	empty := filepath.Join(dir, "e")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	want = cat(str(Magic), str("("), str("type"), str("regular"), str("contents"), str(""), str(")"))
	if got := dumpTo(t, empty); !bytes.Equal(got, want) {
		t.Fatalf("empty file:\n got %x\nwant %x", got, want)
	}
}

func TestDumpSymlink(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "l")
	if err := os.Symlink("../target/elsewhere", link); err != nil {
		t.Fatal(err)
	}
	want := cat(str(Magic), str("("), str("type"), str("symlink"), str("target"), str("../target/elsewhere"), str(")"))
	if got := dumpTo(t, link); !bytes.Equal(got, want) {
		t.Fatalf("symlink:\n got %x\nwant %x", got, want)
	}
	// A dangling symlink is archived, not followed or rejected.
	dangling := filepath.Join(dir, "d")
	if err := os.Symlink("/nowhere", dangling); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "d")); err == nil {
		t.Skip("dangling symlink resolved; skipping")
	}
	if got := dumpTo(t, dangling); len(got) == 0 {
		t.Fatal("dangling symlink produced nothing")
	}
}

// Entries must come out in byte order, whatever order the filesystem lists
// them in — that is what makes the NAR hash reproducible.
func TestDumpDirectoryIsSorted(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Created in deliberately wrong order, with names that sort differently
	// under a locale-aware comparison than byte-wise.
	for _, name := range []string{"zeta", "Alpha", "beta", "_under", "10", "2"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := dumpTo(t, root)

	// The names must appear in byte order in the stream.
	pos := -1
	for _, name := range []string{"10", "2", "Alpha", "_under", "beta", "sub", "zeta"} {
		at := bytes.Index(got, str(name))
		if at < 0 {
			t.Fatalf("entry %q missing from the archive", name)
		}
		if at < pos {
			t.Fatalf("entry %q is out of order", name)
		}
		pos = at
	}

	empty := filepath.Join(dir, "emptydir")
	if err := os.Mkdir(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	want := cat(str(Magic), str("("), str("type"), str("directory"), str(")"))
	if got := dumpTo(t, empty); !bytes.Equal(got, want) {
		t.Fatalf("empty directory:\n got %x\nwant %x", got, want)
	}
}

func TestDumpRejectsUnsupportedFileType(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "p")
	if out, err := exec.Command("mkfifo", fifo).CombinedOutput(); err != nil {
		t.Skipf("mkfifo unavailable: %v %s", err, out)
	}
	var buf bytes.Buffer
	err := Dump(&buf, fifo)
	if err == nil || !strings.Contains(err.Error(), "unsupported file type") {
		t.Fatalf("err = %v, want an unsupported-file-type error", err)
	}
}

func TestDumpMissingPath(t *testing.T) {
	var buf bytes.Buffer
	if err := Dump(&buf, filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("expected an error for a missing path")
	}
}

// writeErr fails after n bytes, standing in for a broken pipe.
type writeErr struct {
	n    int
	seen int
}

func (w *writeErr) Write(p []byte) (int, error) {
	w.seen += len(p)
	if w.seen > w.n {
		return 0, os.ErrClosed
	}
	return len(p), nil
}

func TestDumpPropagatesWriteError(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a", "b", "c"} {
		if err := os.WriteFile(filepath.Join(root, n), bytes.Repeat([]byte(n), 100), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := Dump(&writeErr{n: 16}, root); err == nil {
		t.Fatal("a failing writer must surface as an error, not a silent short archive")
	}
}

// buildTree lays out every shape a store path can contain, including names and
// sizes that exercise the padding rules.
func buildTree(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "tree")
	mkdir := func(p string) {
		if err := os.MkdirAll(filepath.Join(root, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p string, data []byte, mode os.FileMode) {
		if err := os.WriteFile(filepath.Join(root, p), data, mode); err != nil {
			t.Fatal(err)
		}
	}
	mkdir("bin")
	mkdir("share/doc")
	mkdir("empty")
	mkdir("nested/deep/deeper")
	write("bin/prog", []byte("#!/bin/sh\necho hi\n"), 0o755)
	write("bin/lib.so", bytes.Repeat([]byte{0x7f, 'E', 'L', 'F'}, 4096), 0o644)
	write("share/doc/README", []byte("docs\n"), 0o644)
	write("share/doc/empty-file", nil, 0o644)
	// Sizes straddling the 8-byte padding boundary.
	for i, n := range []int{1, 7, 8, 9, 15, 16, 17} {
		write(filepath.Join("share", "pad"+string(rune('a'+i))), bytes.Repeat([]byte("p"), n), 0o644)
	}
	// Names whose lengths straddle it too, plus a unicode name.
	write("n", []byte("x"), 0o644)
	write("nnnnnnn", []byte("x"), 0o644)
	write("nnnnnnnn", []byte("x"), 0o644)
	write("nnnnnnnnn", []byte("x"), 0o644)
	write("ünïcøde-имя", []byte("x"), 0o644)
	write("with spaces and-dashes.txt", []byte("x"), 0o644)
	write("nested/deep/deeper/leaf", []byte("leaf\n"), 0o644)
	if err := os.Symlink("../bin/prog", filepath.Join(root, "share", "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/absolute/target", filepath.Join(root, "abslink")); err != nil {
		t.Fatal(err)
	}
	return root
}

// The contract: byte-for-byte identical to `nix-store --dump`. Skipped where nix
// isn't installed; CI's build+test job has it, and so does the dev shell.
func TestDumpMatchesNixStoreDump(t *testing.T) {
	nixStore, err := exec.LookPath("nix-store")
	if err != nil {
		t.Skip("nix-store not on PATH")
	}
	for _, tc := range []struct {
		name string
		make func(t *testing.T) string
	}{
		{"full tree", buildTree},
		{"single file", func(t *testing.T) string {
			p := filepath.Join(t.TempDir(), "solo")
			if err := os.WriteFile(p, bytes.Repeat([]byte("solo"), 1000), 0o644); err != nil {
				t.Fatal(err)
			}
			return p
		}},
		{"single executable", func(t *testing.T) string {
			p := filepath.Join(t.TempDir(), "runme")
			if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			return p
		}},
		{"single symlink", func(t *testing.T) string {
			p := filepath.Join(t.TempDir(), "ln")
			if err := os.Symlink("target/of/link", p); err != nil {
				t.Fatal(err)
			}
			return p
		}},
		{"empty dir", func(t *testing.T) string {
			p := filepath.Join(t.TempDir(), "hollow")
			if err := os.Mkdir(p, 0o755); err != nil {
				t.Fatal(err)
			}
			return p
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.make(t)
			want, err := exec.Command(nixStore, "--dump", path).Output()
			if err != nil {
				t.Fatalf("nix-store --dump: %v", err)
			}
			got := dumpTo(t, path)
			if !bytes.Equal(got, want) {
				// Report where they diverge; whole archives are unreadable.
				n := 0
				for n < len(got) && n < len(want) && got[n] == want[n] {
					n++
				}
				lo := max(0, n-32)
				t.Fatalf("archives differ at byte %d (ours %d bytes, nix %d)\n ours: %x\n  nix: %x",
					n, len(got), len(want),
					got[lo:min(len(got), n+32)], want[lo:min(len(want), n+32)])
			}
			// The hash is what the server verifies, so state it explicitly.
			gs, ws := sha256.Sum256(got), sha256.Sum256(want)
			if hex.EncodeToString(gs[:]) != hex.EncodeToString(ws[:]) {
				t.Fatalf("NAR hash mismatch")
			}
		})
	}
}

// Same tree twice must produce identical bytes (no map iteration order leaking
// into the archive).
func TestDumpDeterministic(t *testing.T) {
	root := buildTree(t)
	a, b := dumpTo(t, root), dumpTo(t, root)
	if !bytes.Equal(a, b) {
		t.Fatal("two dumps of the same tree differ")
	}
}

func BenchmarkDumpTree(b *testing.B) {
	dir := b.TempDir()
	root := filepath.Join(dir, "t")
	if err := os.MkdirAll(root, 0o755); err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		if err := os.WriteFile(filepath.Join(root, string(rune('a'+i%26))+strings.Repeat("x", i%12)+".f"),
			bytes.Repeat([]byte("data"), 256), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := Dump(io.Discard, root); err != nil {
			b.Fatal(err)
		}
	}
}
