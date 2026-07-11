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

// Cache is a cache's settings as returned by the admin API.
type Cache struct {
	Name      string `json:"name"`
	Public    bool   `json:"public"`
	Priority  int    `json:"priority"`
	Retention int64  `json:"retention"` // seconds; 0 = global default
	MaxBytes  int64  `json:"maxBytes"`  // 0 = unlimited
	PubKey    string `json:"publicKey"`
	Created   int64  `json:"created"`
}

// CacheDetail is Cache plus usage stats (GET /api/v1/caches/{name}).
type CacheDetail struct {
	Cache
	Paths         int64 `json:"paths"`
	Chunks        int64 `json:"chunks"`
	LogicalBytes  int64 `json:"logicalBytes"`
	PhysicalBytes int64 `json:"physicalBytes"`
}

// CreateCacheReq creates a cache (POST /api/v1/caches).
type CreateCacheReq struct {
	Name     string `json:"name"`
	Public   bool   `json:"public"`
	Priority int    `json:"priority"` // 0 = default 40
}

// ConfigureCacheReq updates cache settings; nil fields keep current values.
type ConfigureCacheReq struct {
	Public    *bool  `json:"public,omitempty"`
	Priority  *int   `json:"priority,omitempty"`
	Retention *int64 `json:"retention,omitempty"` // seconds
	MaxBytes  *int64 `json:"maxBytes,omitempty"`
}

// Token is token metadata (never the secret).
type Token struct {
	ID      int64    `json:"id"`
	Name    string   `json:"name"`
	Caches  []string `json:"caches"`
	Perms   []string `json:"perms"`
	Revoked bool     `json:"revoked"`
	Expires int64    `json:"expires"` // unix; 0 = never
	Created int64    `json:"created"`
}

// CreateTokenReq mints a token (POST /api/v1/tokens).
type CreateTokenReq struct {
	Name    string   `json:"name"`
	Caches  []string `json:"caches"` // empty = all
	Perms   []string `json:"perms"`
	Expires int64    `json:"expires"` // unix; 0 = never
}

// CreateTokenResp returns the one-time secret plus the stored metadata.
type CreateTokenResp struct {
	Secret string `json:"secret"`
	Token  Token  `json:"token"`
}

// GCReq triggers a sweep (POST /api/v1/gc).
type GCReq struct {
	// Also evict paths not pulled within this many seconds. 0 = skip.
	EvictOlderThan int64 `json:"evictOlderThan,omitempty"`
}

// GCResp reports what a sweep reclaimed.
type GCResp struct {
	Evicted    int64 `json:"evicted"`
	Deleted    int   `json:"deleted"`
	FreedBytes int64 `json:"freedBytes"`
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
