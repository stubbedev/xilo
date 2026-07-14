// Package narinfo builds, signs, and formats the .narinfo metadata that Nix
// fetches from a binary cache, plus the ed25519 key handling for signatures.
package narinfo

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"
)

const StoreDir = "/nix/store"

// Store-path shapes: 32 nixbase32 hash chars, a dash, then a name. The narinfo
// wire format is newline-delimited, so StorePath/References/Deriver must be
// validated on ingest — an embedded newline or space would otherwise let a
// push inject extra header lines into every served narinfo.
var (
	reStorePath = regexp.MustCompile(`^/nix/store/[0-9a-df-np-sv-z]{32}-[a-zA-Z0-9+._?=-]+$`)
	reStoreName = regexp.MustCompile(`^[0-9a-df-np-sv-z]{32}-[a-zA-Z0-9+._?=-]+$`)
)

// ValidStorePath reports whether s is a well-formed full Nix store path.
func ValidStorePath(s string) bool { return reStorePath.MatchString(s) }

// ValidStoreName reports whether s is a well-formed store-path base name
// (hash-name, no /nix/store prefix), as used for References and Deriver.
func ValidStoreName(s string) bool { return reStoreName.MatchString(s) }

// NarInfo is one store path's cache metadata. References/Deriver are base names
// (no /nix/store prefix), matching the on-the-wire narinfo format.
type NarInfo struct {
	StorePath   string // full /nix/store/<hash>-<name>
	URL         string // relative, e.g. "nar/<hash>.nar"
	Compression string // "none" — we serve reassembled raw NARs
	FileHash    string // sha256:<base32>; == NarHash when Compression=none
	FileSize    uint64 // == NarSize when Compression=none
	NarHash     string // sha256:<base32>
	NarSize     uint64
	References  []string // base names
	Deriver     string   // base name, may be ""
	Sig         []string
}

// Fingerprint is the exact string Nix signs/verifies for a narinfo. refs must be
// FULL store paths (with /nix/store prefix), comma-joined.
func Fingerprint(storePath, narHash string, narSize uint64, refs []string) string {
	return fmt.Sprintf("1;%s;%s;%d;%s", storePath, narHash, narSize, strings.Join(refs, ","))
}

// Sign returns a "<keyName>:<base64 sig>" signature line over fp.
func Sign(keyName string, priv ed25519.PrivateKey, fp string) string {
	sig := ed25519.Sign(priv, []byte(fp))
	return keyName + ":" + base64.StdEncoding.EncodeToString(sig)
}

// PublicKeyString formats a public key the way trusted-public-keys wants it:
// "<keyName>:<base64 pubkey>".
func PublicKeyString(keyName string, pub ed25519.PublicKey) string {
	return keyName + ":" + base64.StdEncoding.EncodeToString(pub)
}

// String renders the narinfo in the canonical field order Nix expects.
func (n *NarInfo) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "StorePath: %s\n", n.StorePath)
	fmt.Fprintf(&b, "URL: %s\n", n.URL)
	fmt.Fprintf(&b, "Compression: %s\n", n.Compression)
	fmt.Fprintf(&b, "FileHash: %s\n", n.FileHash)
	fmt.Fprintf(&b, "FileSize: %d\n", n.FileSize)
	fmt.Fprintf(&b, "NarHash: %s\n", n.NarHash)
	fmt.Fprintf(&b, "NarSize: %d\n", n.NarSize)
	fmt.Fprintf(&b, "References: %s\n", strings.Join(n.References, " "))
	if n.Deriver != "" {
		fmt.Fprintf(&b, "Deriver: %s\n", n.Deriver)
	}
	for _, s := range n.Sig {
		fmt.Fprintf(&b, "Sig: %s\n", s)
	}
	return b.String()
}

// BaseName strips the /nix/store/ prefix from a full store path.
func BaseName(storePath string) string {
	return strings.TrimPrefix(storePath, StoreDir+"/")
}

// StoreHash returns the 32-char hash part of a store path ("/nix/store/<hash>-<name>").
func StoreHash(storePath string) string {
	base := BaseName(storePath)
	if i := strings.IndexByte(base, '-'); i >= 0 {
		return base[:i]
	}
	return base
}
