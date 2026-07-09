// Package api defines the JSON wire types shared by the push client and server
// so the two can never drift.
package api

// ConfigResp is the server-dictated push configuration. Clients fetch it so
// chunking stays consistent (global dedup), parallelism matches server
// capacity, and upstream-signed paths are skipped automatically.
type ConfigResp struct {
	MinSize      int      `json:"minSize"`
	AvgSize      int      `json:"avgSize"`
	MaxSize      int      `json:"maxSize"`
	NarThreshold int      `json:"narThreshold"`
	Parallelism  int      `json:"parallelism"`
	UpstreamKeys []string `json:"upstreamKeys"`
	PublicKey    string   `json:"publicKey"` // "<cache>:<base64>" for trusted-public-keys
	Public       bool     `json:"public"`    // false → pull needs a token (netrc)
}

// MissingReq is the body for get-missing-paths / get-missing-chunks.
type MissingReq struct {
	// Store hashes (get-missing-paths) or chunk hashes (get-missing-chunks).
	Hashes []string `json:"hashes"`
}

// MissingResp lists the subset of the requested hashes the server does NOT have.
type MissingResp struct {
	Missing []string `json:"missing"`
}

// PathReq registers one store path in a cache after its chunks are uploaded.
type PathReq struct {
	StorePath  string   `json:"storePath"` // full /nix/store/<hash>-<name>
	NarHash    string   `json:"narHash"`   // sha256:<base32>
	NarSize    uint64   `json:"narSize"`
	Deriver    string   `json:"deriver"`    // base name, may be ""
	References []string `json:"references"` // full store paths
	Chunks     []string `json:"chunks"`     // ordered chunk sha256 hex
}
