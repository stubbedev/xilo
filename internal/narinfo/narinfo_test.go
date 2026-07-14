package narinfo

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"strings"
	"testing"
)

func TestBase32ZerosAndRoundtrip(t *testing.T) {
	// 32 zero bytes → 52 zero chars.
	zeros := make([]byte, 32)
	if got := Base32Encode(zeros); got != strings.Repeat("0", 52) {
		t.Fatalf("zeros encode = %q", got)
	}
	// Roundtrip a real nix narHash body.
	const b32 = "1impfw8zdgisxkghq9a3q7cn7jb9zyzgxdydiamp8z2nlyyl0h5h"
	raw, err := Base32Decode(b32)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 32 {
		t.Fatalf("decoded %d bytes, want 32", len(raw))
	}
	if got := Base32Encode(raw); got != b32 {
		t.Fatalf("roundtrip = %q want %q", got, b32)
	}
}

func TestParseHashForms(t *testing.T) {
	const b32 = "1impfw8zdgisxkghq9a3q7cn7jb9zyzgxdydiamp8z2nlyyl0h5h"
	raw, _ := Base32Decode(b32)
	sri := "sha256-" + base64.StdEncoding.EncodeToString(raw)
	for _, in := range []string{"sha256:" + b32, sri} {
		got, err := NarHash(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if got != "sha256:"+b32 {
			t.Fatalf("%s => %s", in, got)
		}
	}
}

func TestSignVerify(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	fp := Fingerprint("/nix/store/aaa-foo", "sha256:"+strings.Repeat("0", 52), 123,
		[]string{"/nix/store/bbb-bar"})
	sigLine := Sign("mycache", priv, fp)
	name, b64, _ := strings.Cut(sigLine, ":")
	if name != "mycache" {
		t.Fatalf("key name %q", name)
	}
	sig, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(pub, []byte(fp), sig) {
		t.Fatal("signature did not verify")
	}
}

func TestBase32EncodeEmpty(t *testing.T) {
	if got := Base32Encode(nil); got != "" {
		t.Fatalf("Base32Encode(nil) = %q, want \"\"", got)
	}
}

func TestBase32DecodeNonzeroCarry(t *testing.T) {
	// "z0": the high digit overflows the single output byte → carry error.
	if _, err := Base32Decode("z0"); err == nil {
		t.Fatal("expected nonzero-carry error")
	}
}

func TestNarHashParseError(t *testing.T) {
	if _, err := NarHash("bogus"); err == nil {
		t.Fatal("expected error for unrecognized hash")
	}
	// 64 chars matching hex length but with non-hex chars.
	if _, err := ParseHash("sha256:" + strings.Repeat("z", 64)); err == nil {
		t.Fatal("expected hex decode error")
	}
}

func TestPublicKeyString(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	got := PublicKeyString("mycache", pub)
	want := "mycache:" + base64.StdEncoding.EncodeToString(pub)
	if got != want {
		t.Fatalf("PublicKeyString = %q, want %q", got, want)
	}
}

func TestNarInfoStringMinimal(t *testing.T) {
	// No deriver, no sigs: those lines must be absent.
	ni := &NarInfo{StorePath: "/nix/store/aaa-foo", URL: "nar/aaa.nar", Compression: "none"}
	out := ni.String()
	if strings.Contains(out, "Deriver:") || strings.Contains(out, "Sig:") {
		t.Fatalf("minimal render has optional lines:\n%s", out)
	}
	if !strings.Contains(out, "References: \n") {
		t.Fatalf("missing empty References line:\n%s", out)
	}
}

func TestBaseName(t *testing.T) {
	if got := BaseName("/nix/store/aaa-foo"); got != "aaa-foo" {
		t.Fatalf("BaseName = %q", got)
	}
	if got := BaseName("aaa-foo"); got != "aaa-foo" {
		t.Fatalf("BaseName no-prefix = %q", got)
	}
	if got := BaseName(""); got != "" {
		t.Fatalf("BaseName empty = %q", got)
	}
}

func TestNarInfoString(t *testing.T) {
	ni := &NarInfo{
		StorePath: "/nix/store/aaa-foo", URL: "nar/aaa.nar", Compression: "none",
		FileHash: "sha256:x", FileSize: 10, NarHash: "sha256:x", NarSize: 10,
		References: []string{"bbb-bar"}, Deriver: "ccc-foo.drv", Sig: []string{"c:sig"},
	}
	out := ni.String()
	for _, want := range []string{"StorePath: /nix/store/aaa-foo", "Compression: none", "Sig: c:sig", "Deriver: ccc-foo.drv"} {
		if !bytes.Contains([]byte(out), []byte(want)) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

func TestValidStorePathRejectsInjection(t *testing.T) {
	good := "/nix/store/00000000000000000000000000000000-foo-1.0"
	if !ValidStorePath(good) {
		t.Fatalf("ValidStorePath(%q) = false, want true", good)
	}
	for _, bad := range []string{
		"/nix/store/00000000000000000000000000000000-foo\nSig: evil:sig", // newline injection
		"/nix/store/short-foo",                          // hash too short
		"/nix/store/0000000000000000000000000000000e-x", // 'e' not in nixbase32
		"/etc/passwd",
		"00000000000000000000000000000000-foo", // missing prefix
		"",
	} {
		if ValidStorePath(bad) {
			t.Errorf("ValidStorePath(%q) = true, want false", bad)
		}
	}
	if !ValidStoreName("00000000000000000000000000000000-foo") {
		t.Fatal("ValidStoreName rejected a valid base name")
	}
	if ValidStoreName("00000000000000000000000000000000-foo bar") {
		t.Fatal("ValidStoreName accepted a name with a space")
	}
}
