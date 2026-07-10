package narinfo

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

// nixBase32 is Nix's custom base32 alphabet (omits e, o, u, t to dodge
// accidental words and confusable chars). NOT RFC 4648.
const nixBase32 = "0123456789abcdfghijklmnpqrsvwxyz"

// Base32Encode mirrors Nix's nixbase32 (see nix/src/libutil/hash.cc). A 32-byte
// sha256 encodes to 52 chars.
func Base32Encode(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	n := (len(b)*8-1)/5 + 1
	out := make([]byte, n)
	for i := n - 1; i >= 0; i-- {
		bit := i * 5
		byteIdx := bit / 8
		shift := bit % 8
		c := b[byteIdx] >> shift
		if byteIdx+1 < len(b) {
			c |= b[byteIdx+1] << (8 - shift)
		}
		out[n-1-i] = nixBase32[c&0x1f]
	}
	return string(out)
}

// Base32Decode is the inverse of Base32Encode.
func Base32Decode(s string) ([]byte, error) {
	outLen := len(s) * 5 / 8
	if len(s) > 0 && outLen == 0 {
		// 1 char = 5 bits < 1 byte; indexing out[0] below would panic.
		return nil, fmt.Errorf("invalid nix-base32 %q: too short", s)
	}
	out := make([]byte, outLen)
	for n := 0; n < len(s); n++ {
		c := s[len(s)-1-n]
		digit := strings.IndexByte(nixBase32, c)
		if digit < 0 {
			return nil, fmt.Errorf("invalid nix-base32 char %q", c)
		}
		bit := n * 5
		i := bit / 8
		shift := bit % 8
		out[i] |= byte(digit) << shift
		carry := byte(digit) >> (8 - shift)
		if i+1 < outLen {
			out[i+1] |= carry
		} else if carry != 0 {
			return nil, fmt.Errorf("invalid nix-base32 %q: nonzero carry", s)
		}
	}
	return out, nil
}

// ParseHash accepts a sha256 hash in any form Nix emits — "sha256:<base32>",
// "sha256:<hex>", or SRI "sha256-<base64>" — and returns the 32 raw bytes.
func ParseHash(s string) ([]byte, error) {
	rest := s
	if i := strings.IndexAny(s, ":-"); i >= 0 {
		rest = s[i+1:]
	}
	switch len(rest) {
	case 52: // nix-base32 of 32 bytes
		return Base32Decode(rest)
	case 64: // hex
		return hex.DecodeString(rest)
	case 43, 44: // SRI base64 (std, with or without padding)
		if b, err := base64.StdEncoding.DecodeString(rest); err == nil {
			return b, nil
		}
		return base64.RawStdEncoding.DecodeString(rest)
	default:
		return nil, fmt.Errorf("unrecognized sha256 hash %q", s)
	}
}

// NarHash normalizes any accepted hash form to the canonical "sha256:<base32>"
// that narinfo NarHash fields and signing fingerprints use.
func NarHash(s string) (string, error) {
	b, err := ParseHash(s)
	if err != nil {
		return "", err
	}
	if len(b) != 32 {
		return "", fmt.Errorf("expected 32-byte sha256, got %d bytes", len(b))
	}
	return "sha256:" + Base32Encode(b), nil
}
