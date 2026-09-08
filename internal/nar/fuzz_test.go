package nar

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Dump writes the archive whose sha256 becomes a path's NarHash, so its output
// is not allowed to depend on anything but the tree it was given. These
// targets fuzz the parts of a tree a store path can actually vary: the file
// name, the contents, and the executable bit. TestDumpMatchesNixStoreDump
// pins the layout against real nix; these pin the properties that must hold
// for names nix would accept but the differential cases never happen to use.

// A NAR is a stream of length-prefixed, padded strings. Whatever the name and
// contents, the archive has to stay well formed under that framing and stay
// byte-identical across runs, because two dumps of one path that differ mean
// two different NarHashes for the same store path.
func FuzzDumpSingleFile(f *testing.F) {
	f.Add("hello", []byte("world"), false)
	f.Add("a", []byte{}, true)
	f.Add("name with spaces", []byte{0, 1, 2, 3}, false)
	f.Add("ünïcødé", []byte("x"), false)
	f.Add(strings.Repeat("n", 200), bytes.Repeat([]byte{9}, 5000), true)

	f.Fuzz(func(t *testing.T, name string, content []byte, executable bool) {
		if !legalEntryName(name) {
			t.Skip("not a legal single path element")
		}

		dir := t.TempDir()
		path := filepath.Join(dir, name)
		mode := os.FileMode(0o644)
		if executable {
			mode = 0o755
		}
		if err := os.WriteFile(path, content, mode); err != nil {
			t.Skipf("the filesystem would not hold %q: %v", name, err)
		}

		var first bytes.Buffer
		if err := Dump(&first, path); err != nil {
			t.Fatalf("Dump of a %d-byte file: %v", len(content), err)
		}
		fr := frames(t, first.Bytes())
		if len(fr) == 0 || fr[0] != "nix-archive-1" {
			t.Fatalf("archive does not open with the magic: %q", fr)
		}

		var second bytes.Buffer
		if err := Dump(&second, path); err != nil {
			t.Fatalf("second Dump: %v", err)
		}
		if !bytes.Equal(first.Bytes(), second.Bytes()) {
			t.Fatal("two dumps of one file differ, so its NarHash is not stable")
		}
	})
}

// Directory entries are emitted in sorted order, which is the part of the
// format most easily broken by a locale or a map iteration: nix compares
// names byte-wise, and an archive sorted any other way hashes differently on
// a machine that disagrees.
func FuzzDumpDirectoryOrdering(f *testing.F) {
	f.Add("b\na\nc")
	f.Add("A\na\nB\nb")
	f.Add("z\n0\n_\n-")
	f.Add("é\ne")
	f.Add("name\nnode\nentry") // names that collide with the format's own words

	f.Fuzz(func(t *testing.T, spec string) {
		names := map[string]bool{}
		for n := range strings.SplitSeq(spec, "\n") {
			if legalEntryName(n) {
				names[n] = true
			}
		}
		if len(names) < 2 {
			t.Skip("need at least two entries to have an order")
		}

		dir := t.TempDir()
		tree := filepath.Join(dir, "tree")
		if err := os.Mkdir(tree, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		written := map[string]bool{}
		for n := range names {
			if err := os.WriteFile(filepath.Join(tree, n), nil, 0o644); err != nil {
				t.Skipf("the filesystem would not hold %q: %v", n, err)
			}
			written[n] = true
		}

		var buf bytes.Buffer
		if err := Dump(&buf, tree); err != nil {
			t.Fatalf("Dump: %v", err)
		}
		got := entryNames(t, frames(t, buf.Bytes()))
		want := sortedKeys(written)
		if len(got) != len(want) {
			t.Fatalf("archive lists %d entries (%q), the directory holds %d (%q)",
				len(got), got, len(want), want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("entry %d is %q, byte order says %q (archive: %q)", i, got[i], want[i], got)
			}
		}
	})
}

// legalEntryName keeps the fuzzer to strings that name one path element, which
// is all Dump is ever handed. Names that collide with the format's own words
// ("name", "entry") are deliberately allowed: entryNames reads the structure
// rather than searching for bytes, so those are interesting rather than
// ambiguous.
func legalEntryName(n string) bool {
	return n != "" && n != "." && n != ".." &&
		len(n) <= 100 && !strings.ContainsAny(n, "/\x00")
}

// frames splits an archive into the length-prefixed, 8-byte-padded strings the
// format is made of, failing if any frame claims more bytes than remain. A
// miscounted archive would otherwise get signed as if it were whole.
func frames(t *testing.T, b []byte) []string {
	t.Helper()
	if len(b)%8 != 0 {
		t.Fatalf("archive is %d bytes, not a multiple of 8", len(b))
	}
	var out []string
	for rest := b; len(rest) > 0; {
		if len(rest) < 8 {
			t.Fatalf("%d trailing bytes, too few for a length prefix", len(rest))
		}
		n := binary.LittleEndian.Uint64(rest)
		if n > uint64(len(rest)) {
			t.Fatalf("frame claims %d bytes with only %d left", n, len(rest)-8)
		}
		padded := (n + 7) / 8 * 8
		if 8+padded > uint64(len(rest)) {
			t.Fatalf("frame of %d bytes pads to %d, past the %d remaining", n, padded, len(rest)-8)
		}
		out = append(out, string(rest[8:8+n]))
		rest = rest[8+padded:]
	}
	return out
}

// entryNames pulls a directory's entry names out in archive order, anchored on
// the `entry ( name <name>` structure rather than on the bytes of the name.
// Searching the raw archive cannot work: a 1-byte frame's padding followed by
// the length prefix of "name" reproduces the encoding of a 1-byte name, so
// bytes.Index finds entries that are not there.
func entryNames(t *testing.T, fr []string) []string {
	t.Helper()
	var out []string
	for i := 0; i+3 < len(fr); i++ {
		if fr[i] == "entry" && fr[i+1] == "(" && fr[i+2] == "name" {
			out = append(out, fr[i+3])
			i += 3
		}
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// Byte order, the order nix compares names in.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
