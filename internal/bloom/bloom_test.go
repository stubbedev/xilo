package bloom

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"testing"
)

// hashes returns n distinct sha256 hex digests, deterministically.
func hashes(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s-%d", prefix, i)))
		out[i] = hex.EncodeToString(sum[:])
	}
	return out
}

func TestNoFalseNegatives(t *testing.T) {
	in := hashes("in", 5000)
	f := New(len(in))
	for _, h := range in {
		if err := f.Add(h); err != nil {
			t.Fatal(err)
		}
	}
	for _, h := range in {
		if !f.Has(h) {
			t.Fatalf("added hash reported absent: %s", h)
		}
	}
}

// The pusher trusts a hit and skips the upload, so the fp rate is a correctness
// budget, not a nicety: at 32 bits/entry it must stay far below the 1-in-4000
// that would make a false hit likely on every large NAR.
func TestFalsePositiveRate(t *testing.T) {
	const n = 20000
	f := New(n)
	for _, h := range hashes("member", n) {
		f.Add(h)
	}
	probe := hashes("stranger", 200000)
	fp := 0
	for _, h := range probe {
		if f.Has(h) {
			fp++
		}
	}
	// Expected fp at k=optimal, m/n>=32 is ~4e-7, i.e. 0 hits in 200k probes.
	// Allow a couple so the test isn't hostage to the hash mixing.
	if fp > 2 {
		t.Fatalf("false positives = %d/%d, want <= 2", fp, len(probe))
	}
}

func TestEmptyFilterHasNothing(t *testing.T) {
	f := New(1000)
	for _, h := range hashes("any", 1000) {
		if f.Has(h) {
			t.Fatal("empty filter reported a hit")
		}
	}
}

func TestSizing(t *testing.T) {
	if f := New(MaxEntries + 1); f != nil {
		t.Fatal("New past MaxEntries must return nil so the server serves no filter")
	}
	if f := New(MaxEntries); f == nil {
		t.Fatal("New at MaxEntries must work")
	}
	for _, n := range []int{-5, 0, 1} {
		f := New(n)
		if f == nil || f.m < 64 || f.m&(f.m-1) != 0 || f.k < 1 {
			t.Fatalf("New(%d) = %+v, want a usable filter", n, f)
		}
	}
	// m is a power of two >= 32 bits/entry, k stays within the probe cap.
	f := New(100000)
	if f.m < 32*100000 || f.m&(f.m-1) != 0 {
		t.Fatalf("m = %d, want a power of two >= %d", f.m, 32*100000)
	}
	if f.k < 1 || f.k > maxProbes {
		t.Fatalf("k = %d, want 1..%d", f.k, maxProbes)
	}
	if want := headerSize + len(f.bits)*8; f.Len() != want {
		t.Fatalf("Len = %d, want %d", f.Len(), want)
	}
	if got := len(f.Marshal()); got != f.Len() {
		t.Fatalf("Marshal wrote %d bytes, Len said %d", got, f.Len())
	}
}

func TestMarshalRoundTrip(t *testing.T) {
	in := hashes("rt", 2000)
	f := New(len(in))
	for _, h := range in {
		f.Add(h)
	}
	got, err := Unmarshal(f.Marshal())
	if err != nil {
		t.Fatal(err)
	}
	if got.m != f.m || got.k != f.k {
		t.Fatalf("params drifted: m %d→%d k %d→%d", f.m, got.m, f.k, got.k)
	}
	for _, h := range in {
		if !got.Has(h) {
			t.Fatalf("member lost in round trip: %s", h)
		}
	}
	// Identical verdicts on non-members too (same bits, same probes).
	for _, h := range hashes("rt-out", 2000) {
		if got.Has(h) != f.Has(h) {
			t.Fatalf("verdict differs after round trip for %s", h)
		}
	}
}

func TestUnmarshalRejectsGarbage(t *testing.T) {
	good := New(64).Marshal()

	corrupt := func(mut func([]byte) []byte) []byte {
		b := make([]byte, len(good))
		copy(b, good)
		return mut(b)
	}
	cases := map[string][]byte{
		"empty":        nil,
		"short header": good[:headerSize-1],
		"bad magic":    corrupt(func(b []byte) []byte { b[0] = 'y'; return b }),
		"k=0":          corrupt(func(b []byte) []byte { binary.LittleEndian.PutUint32(b[4:], 0); return b }),
		"k too big":    corrupt(func(b []byte) []byte { binary.LittleEndian.PutUint32(b[4:], 65); return b }),
		"m not pow2":   corrupt(func(b []byte) []byte { binary.LittleEndian.PutUint64(b[8:], 100); return b }),
		"m tiny":       corrupt(func(b []byte) []byte { binary.LittleEndian.PutUint64(b[8:], 8); return b }),
		"m mismatched": corrupt(func(b []byte) []byte { binary.LittleEndian.PutUint64(b[8:], 1<<20); return b }),
		"truncated":    good[:len(good)-8],
		"trailing":     append(append([]byte{}, good...), 0),
	}
	for name, body := range cases {
		if f, err := Unmarshal(body); err == nil {
			t.Fatalf("%s: accepted, got %+v", name, f)
		}
	}
}

func BenchmarkHas(b *testing.B) {
	f := New(1 << 20)
	hs := hashes("bench", 1024)
	for _, h := range hs {
		f.Add(h)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f.Has(hs[i%len(hs)])
	}
}
