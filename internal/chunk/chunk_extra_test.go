package chunk

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"testing"
	"testing/iotest"
)

func TestHash(t *testing.T) {
	// sha256("abc"), a fixed vector.
	const want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got := Hash([]byte("abc")); got != want {
		t.Fatalf("Hash = %s, want %s", got, want)
	}
	if got := Hash(nil); got != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Fatalf("Hash(nil) = %s", got)
	}
}

func TestDefault(t *testing.T) {
	p := Default()
	if p.MinSize != MinSize || p.AvgSize != AvgSize || p.MaxSize != MaxSize {
		t.Fatalf("Default() = %+v", p)
	}
}

func TestSplitEmptyInput(t *testing.T) {
	n := 0
	err := Split(bytes.NewReader(nil), Default(), func(Chunk) error { n++; return nil })
	if err != nil || n != 0 {
		t.Fatalf("empty input: n=%d err=%v, want 0/nil", n, err)
	}
}

func TestSplitSizeBounds(t *testing.T) {
	data := make([]byte, 5<<20)
	rand.New(rand.NewSource(2)).Read(data)
	var sizes []int
	if err := Split(bytes.NewReader(data), Default(), func(c Chunk) error {
		sizes = append(sizes, len(c.Data))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for i, s := range sizes {
		if s > MaxSize {
			t.Fatalf("chunk %d size %d > MaxSize", i, s)
		}
		if s < MinSize && i != len(sizes)-1 {
			t.Fatalf("non-tail chunk %d size %d < MinSize", i, s)
		}
	}
}

// Input below MinSize yields exactly one sub-min tail chunk with the bytes intact.
func TestSplitSubMinTail(t *testing.T) {
	data := []byte("tiny")
	cs := collect(t, data)
	if len(cs) != 1 || !bytes.Equal(cs[0].Data, data) || cs[0].Hash != Hash(data) {
		t.Fatalf("sub-min input: %+v", cs)
	}
}

// Exactly MinSize bytes come back as one chunk.
func TestSplitExactlyMinSize(t *testing.T) {
	data := make([]byte, MinSize)
	rand.New(rand.NewSource(3)).Read(data)
	cs := collect(t, data)
	if len(cs) != 1 || len(cs[0].Data) != MinSize {
		t.Fatalf("got %d chunks, first len %d", len(cs), len(cs[0].Data))
	}
	if !bytes.Equal(cs[0].Data, data) {
		t.Fatal("chunk data differs from input")
	}
}

func TestSplitCallbackError(t *testing.T) {
	boom := errors.New("boom")
	err := Split(bytes.NewReader(make([]byte, 1<<20)), Default(), func(Chunk) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("callback error not propagated: %v", err)
	}
}

func TestSplitReaderError(t *testing.T) {
	boom := errors.New("read fail")
	r := io.MultiReader(bytes.NewReader(make([]byte, 1024)), iotest.ErrReader(boom))
	err := Split(r, Default(), func(Chunk) error { return nil })
	if !errors.Is(err, boom) {
		t.Fatalf("reader error not propagated: %v", err)
	}
}

// Invalid params must surface fastcdc's option validation error.
func TestSplitBadParams(t *testing.T) {
	err := Split(bytes.NewReader([]byte("x")), Params{MinSize: 10, AvgSize: 4, MaxSize: 20}, func(Chunk) error { return nil })
	if err == nil {
		t.Fatal("bad params should error")
	}
}

func TestSplitHashesMatchesSplit(t *testing.T) {
	data := make([]byte, 3<<20)
	rand.New(rand.NewSource(4)).Read(data)
	full := collect(t, data)
	var hashes []string
	if err := SplitHashes(bytes.NewReader(data), Default(), func(h string) error {
		hashes = append(hashes, h)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(hashes) != len(full) {
		t.Fatalf("hash count %d != chunk count %d", len(hashes), len(full))
	}
	for i := range hashes {
		if hashes[i] != full[i].Hash {
			t.Fatalf("hash %d differs", i)
		}
	}
	// callback error propagates here too
	boom := errors.New("boom")
	err := SplitHashes(bytes.NewReader(data), Default(), func(string) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("SplitHashes error not propagated: %v", err)
	}
}

// fn owns a fresh copy: mutating a delivered chunk must not corrupt later ones.
func TestSplitDataIsCopy(t *testing.T) {
	data := make([]byte, 3<<20)
	rand.New(rand.NewSource(5)).Read(data)
	var got []Chunk
	if err := Split(bytes.NewReader(data), Default(), func(c Chunk) error {
		for i := range c.Data {
			c.Data[i] = 0 // scribble; must not affect subsequent chunks
		}
		got = append(got, c)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Re-split untouched: hashes must match what was delivered before scribbling.
	want := collect(t, data)
	if len(got) != len(want) {
		t.Fatalf("count %d != %d", len(got), len(want))
	}
	for i := range got {
		if got[i].Hash != want[i].Hash {
			t.Fatalf("chunk %d hash differs — buffer aliasing", i)
		}
	}
}
