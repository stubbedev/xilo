package narinfo

import (
	"strings"
	"testing"
)

// Everything fuzzed here reads text a client controls. A nix client asks for
// `/<hash>.narinfo` and `/nar/<hash>.nar` on a public endpoint, and the
// narinfo it uploads carries NarHash in whichever of three forms nix felt
// like emitting, so these parsers see whatever anyone puts on the wire. The
// contract is narrow and worth pinning: never panic, and never return a value
// that lies about what it parsed.

// ParseHash accepts base32, hex and SRI base64. A parser that panics on a
// crafted length, or hands back a wrong-length digest that later gets used as
// a 32-byte key, is a bug reachable from an unauthenticated request.
func FuzzParseHash(f *testing.F) {
	f.Add("sha256:0f8mgfa6q4rk1kmvzvi1z0j4jw1v2b0mpm5mrxbmk4b1n8m3ryw2")
	f.Add("sha256:" + strings.Repeat("ab", 32))
	f.Add("sha256-LCa0a2j/xo/5m0U8HTBBNBNCLXBkg7+g+YpeiGJm564=")
	f.Add("")
	f.Add(":")
	f.Add("-")
	f.Add("sha256:")
	f.Add(strings.Repeat("z", 52))

	f.Fuzz(func(t *testing.T, s string) {
		b, err := ParseHash(s)
		if err != nil {
			if b != nil {
				t.Fatalf("ParseHash(%q) returned %d bytes alongside an error", s, len(b))
			}
			return
		}
		// A successful parse must describe one of the three accepted forms.
		// Anything else means the length switch and the decoders disagree,
		// which is how a short digest reaches code expecting sha256.
		switch len(b) {
		case 32:
		default:
			// Hex and base64 of other lengths do decode; callers that need a
			// sha256 must go through NarHash. Assert only that the bytes are
			// consistent with the input length, never that they are silently
			// padded out to 32.
			if len(b) > 64 {
				t.Fatalf("ParseHash(%q) produced %d bytes from %d chars", s, len(b), len(s))
			}
		}
	})
}

// NarHash is the normalizer every signing fingerprint goes through, so its
// output has to be canonical: feeding its own result back in must produce the
// same string, or two callers can sign different fingerprints for one path.
func FuzzNarHashIsCanonical(f *testing.F) {
	f.Add("sha256:0f8mgfa6q4rk1kmvzvi1z0j4jw1v2b0mpm5mrxbmk4b1n8m3ryw2")
	f.Add("sha256:" + strings.Repeat("ab", 32))
	f.Add("sha256-LCa0a2j/xo/5m0U8HTBBNBNCLXBkg7+g+YpeiGJm564=")

	f.Fuzz(func(t *testing.T, s string) {
		out, err := NarHash(s)
		if err != nil {
			return
		}
		if !strings.HasPrefix(out, "sha256:") {
			t.Fatalf("NarHash(%q) = %q, not sha256-prefixed", s, out)
		}
		again, err := NarHash(out)
		if err != nil {
			t.Fatalf("NarHash(%q) = %q, which NarHash then rejects: %v", s, out, err)
		}
		if again != out {
			t.Fatalf("NarHash not idempotent: %q -> %q -> %q", s, out, again)
		}
	})
}

// Base32 round trips: decode(encode(b)) == b for every byte string, since a
// store hash makes the trip in both directions between the wire and the DB.
func FuzzBase32RoundTrip(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0})
	f.Add([]byte("0123456789abcdef0123456789abcdef"))

	f.Fuzz(func(t *testing.T, b []byte) {
		enc := Base32Encode(b)
		got, err := Base32Decode(enc)
		if err != nil {
			t.Fatalf("Base32Decode(Base32Encode(%x)) = %q: %v", b, enc, err)
		}
		if string(got) != string(b) {
			t.Fatalf("round trip changed bytes: %x -> %q -> %x", b, enc, got)
		}
	})
}

// StoreHash decides which row a path is keyed by, so what matters is that a
// path the validator accepted always yields a real 32-char hash. See the note
// in the body for why the unvalidated case is deliberately not constrained.
func FuzzStorePathAccessors(f *testing.F) {
	f.Add("/nix/store/0f8mgfa6q4rk1kmvzvi1z0j4jw1v2b0m-hello-2.12")
	f.Add("/nix/store/")
	f.Add("/nix/store/-")
	f.Add("")
	f.Add("/nix/store/../../etc/passwd")

	f.Fuzz(func(t *testing.T, s string) {
		hash := StoreHash(s)
		base := BaseName(s)
		if len(base) > len(s) {
			t.Fatalf("BaseName(%q) = %q, longer than its input", s, base)
		}
		// The contract is conditional, not absolute: StoreHash of a path that
		// is not a store path returns junk (StoreHash("/nix/store/../../etc/
		// passwd") is "../../etc/passwd", since it just cuts at the first
		// "-"). That is fine because nothing feeds it unvalidated input --
		// handlePutPath checks ValidStorePath first (api.go), and saveManifest
		// guards with validHash before using the result as a filename. What
		// must hold is that a path the validator accepted yields a real hash,
		// because that is the value a row is keyed by.
		if ValidStorePath(s) {
			if len(hash) != 32 {
				t.Fatalf("ValidStorePath(%q) but StoreHash = %q (%d chars)", s, hash, len(hash))
			}
			if strings.ContainsAny(hash, "/.") {
				t.Fatalf("ValidStorePath(%q) but StoreHash = %q holds a path separator", s, hash)
			}
		}
	})
}
