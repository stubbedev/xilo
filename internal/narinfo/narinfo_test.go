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
