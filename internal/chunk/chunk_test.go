package chunk

import (
	"bytes"
	"math/rand"
	"testing"
)

func collect(t *testing.T, data []byte) []Chunk {
	t.Helper()
	var out []Chunk
	if err := Split(bytes.NewReader(data), Default(), func(c Chunk) error {
		out = append(out, c)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSplitReassembleAndDeterministic(t *testing.T) {
	data := make([]byte, 5<<20)
	rand.New(rand.NewSource(1)).Read(data)

	a := collect(t, data)
	if len(a) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(a))
	}

	// Reassembly is byte-identical.
	var buf bytes.Buffer
	for _, c := range a {
		buf.Write(c.Data)
	}
	if !bytes.Equal(buf.Bytes(), data) {
		t.Fatal("reassembled bytes differ from input")
	}

	// Deterministic boundaries: same input → same chunk hashes.
	b := collect(t, data)
	if len(a) != len(b) {
		t.Fatalf("chunk count differs: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].Hash != b[i].Hash {
			t.Fatalf("chunk %d hash differs", i)
		}
	}
}
