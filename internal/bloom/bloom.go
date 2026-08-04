// Package bloom is a bloom filter over hex content hashes, shared by the
// server (which builds one per storage backend from the chunks table) and the
// push client (which uses it to decide what to upload without asking).
//
// Only "absent" is authoritative. A hit may be a false positive, so a caller
// that skips work on a hit needs a backstop — put-path reports any chunk the
// server turns out to be missing, and the pusher retries that path without the
// filter. Sizing below keeps that fp rate around 1e-7, so the backstop fires
// for roughly one push in a thousand.
package bloom

import (
	"encoding/binary"
	"errors"
	"hash/fnv"
	"math"
)

const (
	// bitsPerEntry buys the very low false-positive rate the optimistic pusher
	// needs: 32 bits/entry at the optimal k is ~4e-7 per lookup, so even a
	// 4000-chunk NAR clears the whole filter without a false hit 99.8% of the
	// time. Halving it would make a false hit (and a whole re-dump) likely on
	// every large path.
	bitsPerEntry = 32
	minBits      = 1 << 13 // 1 KiB floor: tiny caches still get a usable filter
	// MaxEntries caps the filter at 32 MiB (8M chunks ≈ 2 TiB of NAR at the
	// default average chunk size). Past that, shrinking bits/entry to fit would
	// push the fp rate into "re-dump every path" territory, so the server
	// serves no filter at all and clients fall back to asking.
	MaxEntries = 8 << 20
	// maxProbes caps k: past the optimum extra probes only cost CPU.
	maxProbes = 24
)

var magic = [4]byte{'x', 'b', 'f', '1'}

// Filter is an immutable-after-build bloom filter. Add is not safe for
// concurrent use; Has is (the bits never change once built).
type Filter struct {
	bits []uint64
	m    uint64 // bit count, always a power of two
	k    uint32
}

// New sizes a filter for n entries, or returns nil when n exceeds MaxEntries
// (see the constant: a filter that big can't stay accurate enough to be worth
// trusting).
func New(n int) *Filter {
	if n > MaxEntries {
		return nil
	}
	if n < 1 {
		n = 1
	}
	want := uint64(n) * bitsPerEntry
	m := uint64(minBits)
	for m < want {
		m <<= 1
	}
	k := uint32(math.Round(float64(m) / float64(n) * math.Ln2))
	return &Filter{bits: make([]uint64, m/64), m: m, k: min(max(k, 1), maxProbes)}
}

// Len reports the marshalled size in bytes, so callers can decide whether
// shipping the filter is worth it before building the body.
func (f *Filter) Len() int { return headerSize + len(f.bits)*8 }

// probes derives the k bit positions for s. The inputs are sha256 hex digests
// (already uniformly distributed), so two FNV-1a variants are ample mixing for
// Kirsch-Mitzenmacher double hashing.
func (f *Filter) probes(s string) (h1, h2 uint64) {
	a := fnv.New64a()
	a.Write([]byte(s))
	h1 = a.Sum64()
	b := fnv.New64()
	b.Write([]byte(s))
	h2 = b.Sum64() | 1 // odd, so h1+i*h2 walks the whole table
	return h1, h2
}

func (f *Filter) Add(s string) error {
	h1, h2 := f.probes(s)
	for i := uint64(0); i < uint64(f.k); i++ {
		bit := (h1 + i*h2) & (f.m - 1)
		f.bits[bit/64] |= 1 << (bit % 64)
	}
	return nil
}

// Has reports whether s may be present: false is certain, true is probable.
func (f *Filter) Has(s string) bool {
	h1, h2 := f.probes(s)
	for i := uint64(0); i < uint64(f.k); i++ {
		bit := (h1 + i*h2) & (f.m - 1)
		if f.bits[bit/64]&(1<<(bit%64)) == 0 {
			return false
		}
	}
	return true
}

const headerSize = 4 + 4 + 8 // magic + k + m

// Marshal encodes the filter: magic, k, bit count, then the bit words.
func (f *Filter) Marshal() []byte {
	out := make([]byte, headerSize+len(f.bits)*8)
	copy(out, magic[:])
	binary.LittleEndian.PutUint32(out[4:], f.k)
	binary.LittleEndian.PutUint64(out[8:], f.m)
	for i, w := range f.bits {
		binary.LittleEndian.PutUint64(out[headerSize+i*8:], w)
	}
	return out
}

var errBadFilter = errors.New("bloom: malformed filter")

// Unmarshal parses a filter body. Every field is validated: the body arrives
// over the network, and a bogus m would index out of range on the first Has.
func Unmarshal(b []byte) (*Filter, error) {
	if len(b) < headerSize || [4]byte(b[:4]) != magic {
		return nil, errBadFilter
	}
	k := binary.LittleEndian.Uint32(b[4:])
	m := binary.LittleEndian.Uint64(b[8:])
	if k < 1 || k > 64 || m < 64 || m&(m-1) != 0 || uint64(len(b)-headerSize) != m/8 {
		return nil, errBadFilter
	}
	f := &Filter{bits: make([]uint64, m/64), m: m, k: k}
	for i := range f.bits {
		f.bits[i] = binary.LittleEndian.Uint64(b[headerSize+i*8:])
	}
	return f, nil
}
