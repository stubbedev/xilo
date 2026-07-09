package narinfo

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"math/rand"
	"strings"
	"testing"
)

func TestParseHashFourEncodingsAgree(t *testing.T) {
	const b32 = "1impfw8zdgisxkghq9a3q7cn7jb9zyzgxdydiamp8z2nlyyl0h5h"
	raw, err := Base32Decode(b32)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 32 {
		t.Fatalf("fixture is %d bytes, want 32", len(raw))
	}

	forms := map[string]string{
		"hex":        hex.EncodeToString(raw),                               // 64
		"nix-base32": b32,                                                   // 52
		"sri-padded": "sha256-" + base64.StdEncoding.EncodeToString(raw),    // 44
		"sri-raw":    "sha256-" + base64.RawStdEncoding.EncodeToString(raw), // 43
	}
	// sanity on encoded lengths
	if len(forms["hex"]) != 64 || len(b32) != 52 ||
		len(strings.TrimPrefix(forms["sri-padded"], "sha256-")) != 44 ||
		len(strings.TrimPrefix(forms["sri-raw"], "sha256-")) != 43 {
		t.Fatalf("unexpected encoded lengths: %v", forms)
	}

	for name, in := range forms {
		got, err := ParseHash(in)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !bytes.Equal(got, raw) {
			t.Fatalf("%s: decoded %x, want %x", name, got, raw)
		}
	}
}

func TestParseHashErrors(t *testing.T) {
	// 'e','o','u','t' are not in the nix-base32 alphabet.
	bad := "e" + strings.Repeat("0", 51)
	if _, err := Base32Decode(bad); err == nil {
		t.Fatal("expected error for invalid nix-base32 char")
	}
	// Wrong length (40) matches no known encoding.
	if _, err := ParseHash("sha256:" + strings.Repeat("0", 40)); err == nil {
		t.Fatal("expected error for wrong length")
	}
	// NarHash on a non-32-byte input: 33 bytes -> 44 base64 chars, a recognized
	// SRI length that ParseHash accepts but decodes to the wrong size.
	notThirtyTwo := "sha256-" + base64.StdEncoding.EncodeToString(make([]byte, 33))
	if _, err := NarHash(notThirtyTwo); err == nil {
		t.Fatal("expected 32-byte error")
	} else if !strings.Contains(err.Error(), "expected 32-byte") {
		t.Fatalf("err = %v, want 'expected 32-byte'", err)
	}
}

func TestBase32RoundTripRandom(t *testing.T) {
	r := rand.New(rand.NewSource(42))
	for i := 0; i < 20; i++ {
		b := make([]byte, 32)
		r.Read(b)
		enc := Base32Encode(b)
		dec, err := Base32Decode(enc)
		if err != nil {
			t.Fatalf("iter %d: decode: %v", i, err)
		}
		if !bytes.Equal(dec, b) {
			t.Fatalf("iter %d: roundtrip %x -> %x", i, b, dec)
		}
	}
}

func TestStoreHash(t *testing.T) {
	h := strings.Repeat("a", 32)
	cases := map[string]string{
		"/nix/store/" + h + "-name": h, // normal path
		"/nix/store/" + h:           h, // no "-" -> whole basename
		h + "-name":                 h, // no /nix/store prefix -> basename before "-"
		"basename":                  "basename",
		"":                          "",
	}
	for in, want := range cases {
		if got := StoreHash(in); got != want {
			t.Fatalf("StoreHash(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFingerprintUsesFullPathsSignVerify(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	fullRefs := []string{"/nix/store/bbb-bar", "/nix/store/ccc-baz"}
	fp := Fingerprint("/nix/store/aaa-foo", "sha256:"+strings.Repeat("0", 52), 123, fullRefs)

	// Fingerprint uses FULL store paths, comma-joined — not basenames.
	if !strings.Contains(fp, "/nix/store/bbb-bar,/nix/store/ccc-baz") {
		t.Fatalf("fingerprint missing full comma-joined refs: %q", fp)
	}
	if strings.Contains(fp, "bbb-bar,ccc-baz") && !strings.Contains(fp, "/nix/store/bbb-bar") {
		t.Fatalf("fingerprint used basenames, want full paths: %q", fp)
	}

	sigLine := Sign("mycache", priv, fp)
	_, b64, _ := strings.Cut(sigLine, ":")
	sig, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(pub, []byte(fp), sig) {
		t.Fatal("signature did not verify")
	}
}
