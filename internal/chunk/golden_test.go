package chunk

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Chunk boundaries are a storage contract, not an implementation detail. Every
// blob in every cache was cut by this algorithm at these parameters, and the
// push client's manifest cache is keyed by them, so moving the cuts does not
// "improve chunking": it stops new pushes deduping against everything stored
// so far, silently, while every existing test still passes.
// TestSplitReassembleAndDeterministic only proves one binary agrees with
// itself, which is true of any wrong answer too.
//
// So the boundaries are pinned. A diff here means one of:
//
//   - the fastcdc dependency changed its gear table or cut logic, which is a
//     dedup-breaking upgrade needing a deliberate decision (and on a live
//     instance a re-push, or a bump of the chunking params the server serves);
//   - Default() was retuned, same consequence;
//   - the hash function changed, which changes every storage key.
//
// None of those are wrong to do. All of them have to be done on purpose:
//
//	go test ./internal/chunk/ -run TestChunkBoundaries -update
//
// then commit the new vector and say in the message why the numbers moved.

var update = flag.Bool("update", false, "rewrite the pinned chunk-boundary vector")

const goldenFile = "testdata/chunk-boundaries.txt"

// The corpus is generated rather than stored, so the repo carries a list of
// hashes instead of 3 MiB of noise: a deterministic LCG, whose output has no
// long runs and so exercises the rolling hash the way real NAR bytes do.
// 3 MiB at a 256 KiB average is enough for a dozen or so cuts.
func goldenCorpus() []byte {
	b := make([]byte, 3<<20)
	x := uint32(0x9e3779b9)
	for i := range b {
		x = x*1664525 + 1013904223
		b[i] = byte(x >> 24)
	}
	return b
}

func TestChunkBoundaries(t *testing.T) {
	data := goldenCorpus()

	var got []string
	var sizes []int
	var back bytes.Buffer
	if err := SplitRaw(bytes.NewReader(data), Default(), func(h string, b []byte) error {
		got = append(got, h)
		sizes = append(sizes, len(b))
		back.Write(b)
		return nil
	}); err != nil {
		t.Fatalf("SplitRaw: %v", err)
	}

	// Reassembly first: a boundary that moved is a decision to make, but bytes
	// that went missing are a bug whatever the boundaries are.
	if !bytes.Equal(back.Bytes(), data) {
		t.Fatalf("chunks do not reassemble: %d bytes in, %d out", len(data), back.Len())
	}
	if len(got) < 4 {
		t.Fatalf("corpus produced only %d chunks; it exists to exercise many cuts", len(got))
	}

	if *update {
		if err := os.MkdirAll(filepath.Dir(goldenFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenFile, []byte(strings.Join(got, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %d boundaries to %s", len(got), goldenFile)
		return
	}

	raw, err := os.ReadFile(goldenFile)
	if err != nil {
		t.Fatalf("reading the pinned vector: %v (create it with -update)", err)
	}
	want := strings.Fields(string(raw))

	if len(want) != len(got) {
		t.Fatalf("chunk count changed: %d pinned, %d now. If this was deliberate "+
			"(fastcdc upgrade, retuned Default(), new hash function), rerun with "+
			"-update and say why in the commit message.", len(want), len(got))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("chunk %d moved: pinned %s, now %s (%d bytes). Boundaries are a "+
				"storage contract, so everything already stored was cut the old way "+
				"and new pushes stop deduping against it. Rerun with -update only if "+
				"that is intended.", i, want[i], got[i], sizes[i])
		}
	}
}

// The parameters are part of the same contract: the server hands them to every
// client over /api/chunking so dedup stays global, and a client chunking at
// other sizes uploads blobs nothing will ever match.
func TestDefaultParamsAreStable(t *testing.T) {
	p := Default()
	if p.MinSize != 64<<10 || p.AvgSize != 256<<10 || p.MaxSize != 1<<20 {
		t.Fatalf("chunking params changed to %+v. Every stored blob was cut with "+
			"min=64KiB avg=256KiB max=1MiB; changing them means new pushes stop "+
			"deduping against what is already there.", p)
	}
}
