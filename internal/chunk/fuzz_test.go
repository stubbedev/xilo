package chunk

import (
	"bytes"
	"testing"
)

// Chunking is the one operation in xilo that must never be creative. Every
// chunk it emits becomes a content-addressed blob other paths dedup against,
// and every boundary it picks is baked into cached push manifests, so the
// properties below are the storage layer's whole contract: the chunks put the
// input back together byte for byte, they respect the size bounds the params
// asked for, and the same bytes always split the same way.

// FuzzSplitReassembles is the property that matters most: whatever the input,
// concatenating the chunks reproduces it exactly. A split that loses or
// duplicates a byte corrupts a NAR that the server will happily sign.
func FuzzSplitReassembles(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte("hello"))
	f.Add(bytes.Repeat([]byte{0}, 1<<20))
	f.Add(bytes.Repeat([]byte("ab"), 700_000))

	f.Fuzz(func(t *testing.T, data []byte) {
		p := Default()
		var out bytes.Buffer
		var sizes []int
		err := Split(bytes.NewReader(data), p, func(c Chunk) error {
			out.Write(c.Data)
			sizes = append(sizes, len(c.Data))
			return nil
		})
		if err != nil {
			t.Fatalf("Split of %d bytes: %v", len(data), err)
		}
		if !bytes.Equal(out.Bytes(), data) {
			t.Fatalf("reassembly differs: %d bytes in, %d out", len(data), out.Len())
		}
		// Bounds hold for every chunk but the last, which is whatever tail is
		// left over. A chunk above MaxSize means the cut logic ran past its
		// own ceiling, which shows up later as a blob no client expects.
		for i, n := range sizes {
			if n > p.MaxSize {
				t.Fatalf("chunk %d is %d bytes, over MaxSize %d", i, n, p.MaxSize)
			}
			if i < len(sizes)-1 && n < p.MinSize {
				t.Fatalf("chunk %d is %d bytes, under MinSize %d and not the tail", i, n, p.MinSize)
			}
		}
		if len(data) == 0 && len(sizes) != 0 {
			t.Fatalf("empty input produced %d chunks", len(sizes))
		}
	})
}

// FuzzSplitDeterministic pins dedup itself: the same bytes must split to the
// same hashes every time, or a re-push stops matching what is already stored
// and the cache grows a second copy of content it already has.
func FuzzSplitDeterministic(f *testing.F) {
	f.Add([]byte("hello"))
	f.Add(bytes.Repeat([]byte("xilo"), 300_000))

	f.Fuzz(func(t *testing.T, data []byte) {
		hashes := func() []string {
			var hs []string
			if err := SplitHashes(bytes.NewReader(data), Default(), func(h string) error {
				hs = append(hs, h)
				return nil
			}); err != nil {
				t.Fatalf("SplitHashes: %v", err)
			}
			return hs
		}
		a, b := hashes(), hashes()
		if len(a) != len(b) {
			t.Fatalf("chunk count differs between runs: %d vs %d", len(a), len(b))
		}
		for i := range a {
			if a[i] != b[i] {
				t.Fatalf("chunk %d hash differs between runs: %s vs %s", i, a[i], b[i])
			}
		}
	})
}

// FuzzSplitHashesMatchesSplit keeps the three entry points telling one story.
// SplitHashes and SplitRaw exist so a caller can skip buffering chunk bytes,
// and a client that trusts them while the server uses Split would negotiate
// against hashes the server never stored.
func FuzzSplitHashesMatchesSplit(f *testing.F) {
	f.Add([]byte("hello"))
	f.Add(bytes.Repeat([]byte{7}, 1<<19))

	f.Fuzz(func(t *testing.T, data []byte) {
		var viaSplit, viaHashes, viaRaw []string
		if err := Split(bytes.NewReader(data), Default(), func(c Chunk) error {
			viaSplit = append(viaSplit, c.Hash)
			if got := Hash(c.Data); got != c.Hash {
				t.Fatalf("Chunk.Hash %s disagrees with Hash(Data) %s", c.Hash, got)
			}
			return nil
		}); err != nil {
			t.Fatalf("Split: %v", err)
		}
		if err := SplitHashes(bytes.NewReader(data), Default(), func(h string) error {
			viaHashes = append(viaHashes, h)
			return nil
		}); err != nil {
			t.Fatalf("SplitHashes: %v", err)
		}
		if err := SplitRaw(bytes.NewReader(data), Default(), func(h string, _ []byte) error {
			viaRaw = append(viaRaw, h)
			return nil
		}); err != nil {
			t.Fatalf("SplitRaw: %v", err)
		}
		if len(viaSplit) != len(viaHashes) || len(viaSplit) != len(viaRaw) {
			t.Fatalf("chunk counts differ: Split %d, SplitHashes %d, SplitRaw %d",
				len(viaSplit), len(viaHashes), len(viaRaw))
		}
		for i := range viaSplit {
			if viaSplit[i] != viaHashes[i] || viaSplit[i] != viaRaw[i] {
				t.Fatalf("chunk %d differs: Split %s, SplitHashes %s, SplitRaw %s",
					i, viaSplit[i], viaHashes[i], viaRaw[i])
			}
		}
	})
}
