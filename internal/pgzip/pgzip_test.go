package pgzip

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"math/rand/v2"
	"testing"

	kgzip "github.com/klauspost/compress/gzip"
)

const alphabet = "nix-store-path-/bin/lib/share\x00\x7fELF"

func testData(n int) []byte {
	rng := rand.New(rand.NewPCG(7, 11))
	out := make([]byte, n)
	for i := range out {
		switch (i / 4096) % 3 {
		case 0:
			out[i] = byte(rng.Uint32())
		default:
			out[i] = alphabet[rng.IntN(len(alphabet))]
		}
	}
	return out
}

func decodeSingleMember(t *testing.T, compressed []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatalf("open gzip stream: %v", err)
	}
	zr.Multistream(false)
	got, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("decode gzip stream: %v", err)
	}
	if err := zr.Reset(bytes.NewReader(nil)); !errors.Is(err, io.EOF) {
		t.Fatalf("read past the single member: %v", err)
	}
	return got
}

func TestRoundTrip(t *testing.T) {
	sizes := []int{0, 1, dictSize, blockSize - 1, blockSize, blockSize + 1, 3*blockSize + blockSize/2, 5 * blockSize}
	for _, workers := range []int{1, 4} {
		for _, n := range sizes {
			want := testData(n)
			var buf bytes.Buffer
			z := newWriter(&buf, workers)
			for p := want; len(p) > 0; {
				k := min(len(p), 1+len(p)%100003)
				if _, err := z.Write(p[:k]); err != nil {
					t.Fatalf("workers=%d size=%d: write: %v", workers, n, err)
				}
				p = p[k:]
			}
			if err := z.Close(); err != nil {
				t.Fatalf("workers=%d size=%d: close: %v", workers, n, err)
			}
			if got := decodeSingleMember(t, buf.Bytes()); !bytes.Equal(got, want) {
				t.Fatalf("workers=%d size=%d: decoded %d bytes, mismatch", workers, n, len(got))
			}
		}
	}
}

func TestDictionaryKeepsCrossBlockMatches(t *testing.T) {
	unit := make([]byte, dictSize/2)
	_, _ = rand.NewChaCha8([32]byte{3}).Read(unit)
	want := bytes.Repeat(unit, 8*blockSize/len(unit))
	var buf bytes.Buffer
	z := newWriter(&buf, 4)
	if _, err := z.Write(want); err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	if got := decodeSingleMember(t, buf.Bytes()); !bytes.Equal(got, want) {
		t.Fatal("decoded mismatch")
	}
	var ref bytes.Buffer
	zr, err := kgzip.NewWriterLevel(&ref, level)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zr.Write(want); err != nil {
		t.Fatal(err)
	}
	if err := zr.Close(); err != nil {
		t.Fatal(err)
	}
	if limit := ref.Len() + ref.Len()/20; buf.Len() > limit {
		t.Fatalf("%d repeats of %d random bytes compressed to %d bytes, single stream %d: blocks are not matching across their boundary", len(want)/len(unit), len(unit), buf.Len(), ref.Len())
	}
}

type failWriter struct{ after int }

var errSink = errors.New("sink failed")

func (f *failWriter) Write(p []byte) (int, error) {
	if f.after <= 0 {
		return 0, errSink
	}
	f.after--
	return len(p), nil
}

func TestWriteErrorIsSticky(t *testing.T) {
	z := newWriter(&failWriter{after: 2}, 2)
	var err error
	for range 16 {
		if _, err = z.Write(testData(blockSize)); err != nil {
			break
		}
	}
	if !errors.Is(err, errSink) {
		t.Fatalf("write error = %v, want %v", err, errSink)
	}
	if err := z.Close(); !errors.Is(err, errSink) {
		t.Fatalf("close error = %v, want %v", err, errSink)
	}
	if _, err := z.Write([]byte("x")); !errors.Is(err, errClosed) {
		t.Fatalf("write after close = %v, want %v", err, errClosed)
	}
}
