// Package config defines xilo's YAML config. Every field's doc comment becomes
// the description in the generated JSON schema (see `xilo schema dump`), so the
// struct is the single source of truth for both runtime config and editor
// hinting. Keep comments user-facing.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the root of xilo.yaml.
type Config struct {
	// Address the server listens on, host:port. Empty host = all interfaces.
	Listen string `yaml:"listen" json:"listen"`
	// Public base URL clients reach this cache at, e.g.
	// "https://cache.example.com". Used to render setup snippets in the admin
	// UI. Defaults to http://localhost<listen> when empty.
	BaseURL string `yaml:"base_url" json:"base_url"`
	// Directory for the metadata database (and local storage, unless
	// storage.local.root is set). Created if missing.
	DataDir string `yaml:"data_dir" json:"data_dir"`
	// Metadata database settings. Defaults to a zero-config SQLite file in
	// data_dir; point at PostgreSQL for large multi-writer deployments.
	Database Database `yaml:"database" json:"database"`
	// Multi-tenant mode: enables self-registration (governed by the instance
	// settings in the dashboard), plans, and organization creation by users.
	// Off (default) = single-tenant: no signup surface at all, everything is
	// managed by the bootstrap admin.
	MultiTenant bool `yaml:"multi_tenant" json:"multi_tenant"`
	// Admin dashboard settings.
	Admin Admin `yaml:"admin" json:"admin"`
	// Where chunk bytes are stored (the backend named "default").
	Storage Storage `yaml:"storage" json:"storage"`
	// Additional named blob backends. Each cache is pinned to one backend at
	// creation; chunk dedup is per-backend.
	Storages map[string]Storage `yaml:"storages" json:"storages,omitempty"`
	// Which backend newly created caches use when none is specified.
	// Defaults to "default" (the storage: block above).
	DefaultStorage string `yaml:"default_storage" json:"default_storage,omitempty"`
	// Content-defined chunking parameters (server-dictated; push clients fetch
	// these so every client chunks identically and dedup stays global).
	Chunking Chunking `yaml:"chunking" json:"chunking"`
	// At-rest chunk compression.
	Compression Compression `yaml:"compression" json:"compression"`
	// Background garbage collection.
	GC GC `yaml:"gc" json:"gc"`
	// Recommended parallel uploads advertised to push clients. 0 = auto
	// (number of CPUs). Clients use this unless overridden with --jobs.
	Parallelism int `yaml:"parallelism" json:"parallelism"`
	// Upstream cache signing-key names (e.g. "cache.nixos.org-1"). Paths signed
	// by any of these are skipped on push — no point re-caching nixpkgs. Sent to
	// clients so the skip is automatic.
	UpstreamKeys []string `yaml:"upstream_keys" json:"upstream_keys"`
	// Security hardening.
	Security Security `yaml:"security" json:"security"`
	// Server-wide storage limits.
	Limits Limits `yaml:"limits" json:"limits"`
	// Request logging: "full" logs every request (default), "quiet" logs only
	// errors (status >= 400) and slow requests (> 1s). At tens of thousands of
	// requests/second the synchronous log write is measurable — flip to quiet
	// on busy production instances.
	Logging string `yaml:"logging" json:"logging" jsonschema:"enum=full,enum=quiet"`
	// Commit durability: "normal" (default; no fsync per commit — power loss
	// can drop the last few acknowledged pushes, which heal on the next run)
	// or "full" (fsync every commit; acknowledged pushes survive power loss
	// at a per-write latency cost).
	Durability string `yaml:"durability" json:"durability" jsonschema:"enum=normal,enum=full"`
}

// Database selects the metadata store.
type Database struct {
	// Connection URL. Empty (default) = SQLite at <data_dir>/xilo.db.
	// "postgres://user:pass@host/db" switches to PostgreSQL — recommended when
	// running at larger scale than a personal cache. Overridable with
	// XILO_DATABASE_URL (preferred for credentials).
	URL string `yaml:"url" json:"url"`
}

// Postgres reports whether the configured database is PostgreSQL.
func (d Database) Postgres() bool {
	return strings.HasPrefix(d.URL, "postgres://") || strings.HasPrefix(d.URL, "postgresql://")
}

// Limits caps total storage. Per-cache caps are set per cache in the dashboard.
type Limits struct {
	// Total stored (compressed) size across all caches before least-recently-
	// pulled paths are evicted. Human sizes: "500GB", "2TB", "100MB". Empty or
	// "0" = unlimited.
	Total string `yaml:"total" json:"total"`
}

// TotalBytes parses Limits.Total; 0 means unlimited or unparseable.
func (l Limits) TotalBytes() int64 {
	n, _ := ParseBytes(l.Total)
	return n
}

// ParseBytes parses "500GB"/"2TB"/"1024" (binary KiB/MiB/… units) into bytes.
func ParseBytes(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return 0, nil
	}
	units := []struct {
		suffix string
		mult   int64
	}{
		{"TIB", 1 << 40}, {"GIB", 1 << 30}, {"MIB", 1 << 20}, {"KIB", 1 << 10},
		{"TB", 1 << 40}, {"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10},
		{"T", 1 << 40}, {"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}, {"B", 1},
	}
	up := strings.ToUpper(s)
	for _, u := range units {
		if strings.HasSuffix(up, u.suffix) {
			num := strings.TrimSpace(up[:len(up)-len(u.suffix)])
			f, err := strconv.ParseFloat(num, 64)
			if err != nil {
				return 0, fmt.Errorf("bad size %q: %w", s, err)
			}
			return int64(f * float64(u.mult)), nil
		}
	}
	n, err := strconv.ParseInt(up, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("bad size %q", s)
	}
	return n, nil
}

// Security configures upload verification and bootstrap behavior.
type Security struct {
	// Skip reassembling + hashing uploaded chunks against the claimed NarHash on
	// push. Verification is proof-of-possession: without the real NAR a client
	// can't produce a chunk list that hashes correctly, so it can't claim
	// another tenant's path. Leave false unless you need the throughput.
	SkipUploadVerify bool `yaml:"skip_upload_verify" json:"skip_upload_verify"`
	// Allow anonymous push before the first token exists. Convenient for a
	// trusted single-user setup, but on a fresh public deploy it means anyone
	// can push until you mint a token. Default false: push requires a token.
	AllowOpenBootstrap bool `yaml:"allow_open_bootstrap" json:"allow_open_bootstrap"`
}

// Chunking tunes FastCDC. Smaller average size = better dedup, more overhead.
type Chunking struct {
	// Minimum chunk size in bytes.
	MinSize int `yaml:"min_size" json:"min_size"`
	// Target average chunk size in bytes.
	AvgSize int `yaml:"avg_size" json:"avg_size"`
	// Maximum chunk size in bytes.
	MaxSize int `yaml:"max_size" json:"max_size"`
	// NARs smaller than this are stored as a single chunk (skip CDC overhead).
	NarThreshold int `yaml:"nar_threshold" json:"nar_threshold"`
}

// Compression configures how chunks are compressed at rest (zstd only —
// no-dep, and high levels rival xz).
type Compression struct {
	// zstd level: "fastest", "default", "better", or "best".
	Level string `yaml:"level" json:"level" jsonschema:"enum=fastest,enum=default,enum=better,enum=best"`
}

// GC configures the background sweeper. Durations are Go strings ("12h", "30m").
type GC struct {
	// How often to sweep unreferenced chunks. Empty/"0" disables.
	Interval string `yaml:"interval" json:"interval"`
	// Evict store paths not pulled within this window before sweeping.
	// Empty/"0" disables time-based eviction. Per-cache retention overrides this.
	Retention string `yaml:"retention" json:"retention"`
	// Grace window: chunks created more recently than this are never swept, so
	// a chunk uploaded during an in-flight push is never collected before its
	// path is registered. Must exceed your longest single push. Default 1h.
	Grace string `yaml:"grace" json:"grace"`
}

// Admin configures the management dashboard.
type Admin struct {
	// Password for the admin dashboard. Overridable at runtime with
	// XILO_ADMIN_PASSWORD (preferred for secrets / Docker).
	Password string `yaml:"password" json:"password"`
}

// Storage selects and configures the chunk blob backend.
type Storage struct {
	// Backend: "local" (filesystem) or "s3" (any S3-compatible bucket).
	Backend string `yaml:"backend" json:"backend" jsonschema:"enum=local,enum=s3"`
	// Local filesystem backend settings (used when backend=local).
	Local Local `yaml:"local" json:"local"`
	// S3 backend settings (used when backend=s3).
	S3 S3 `yaml:"s3" json:"s3"`
}

// Local is the filesystem storage backend.
type Local struct {
	// Root directory for stored chunks. Defaults to <data_dir>/storage.
	Root string `yaml:"root" json:"root"`
}

// S3 is the S3-compatible storage backend. Every field is overridable from the
// environment (XILO_S3_*) so a Docker deployment needs no config file.
type S3 struct {
	// S3 endpoint host:port, e.g. "s3.amazonaws.com" or "minio.local:9000".
	// Overridable with XILO_S3_ENDPOINT.
	Endpoint string `yaml:"endpoint" json:"endpoint"`
	// Bucket name. Must already exist. Overridable with XILO_S3_BUCKET;
	// setting it via env also selects the s3 backend unless the config file
	// says otherwise.
	Bucket string `yaml:"bucket" json:"bucket"`
	// Region, e.g. "us-east-1". Optional for MinIO. Overridable with XILO_S3_REGION.
	Region string `yaml:"region" json:"region"`
	// Access key. Overridable with XILO_S3_ACCESS_KEY.
	AccessKey string `yaml:"access_key" json:"access_key"`
	// Secret key. Overridable with XILO_S3_SECRET_KEY.
	SecretKey string `yaml:"secret_key" json:"secret_key"`
	// Use plain HTTP instead of TLS (for local MinIO). Default false.
	// Overridable with XILO_S3_INSECURE=true.
	Insecure bool `yaml:"insecure" json:"insecure"`
}

// StorageMap returns every configured blob backend by name: "default" (the
// storage: block) plus the storages: entries.
func (c *Config) StorageMap() map[string]Storage {
	m := map[string]Storage{"default": c.Storage}
	for name, s := range c.Storages {
		m[name] = s
	}
	return m
}

// DBPath is the sqlite file path derived from DataDir.
func (c *Config) DBPath() string { return filepath.Join(c.DataDir, "xilo.db") }

// Load reads a YAML config file, applies defaults, then env overrides for
// secrets. A missing file is not an error — defaults + env still yield a usable
// config.
func Load(path string) (*Config, error) {
	c := &Config{}
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if err == nil {
			if err := yaml.Unmarshal(data, c); err != nil {
				return nil, fmt.Errorf("parsing %s: %w", path, err)
			}
		}
	}
	c.applyEnv()      // env first, so derived defaults build on overridden values
	c.applyDefaults() // fills anything still empty
	return c, c.validate()
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.DataDir == "" {
		c.DataDir = "./data"
	}
	if c.BaseURL == "" {
		c.BaseURL = "http://localhost" + c.Listen
	}
	if c.Storage.Backend == "" {
		c.Storage.Backend = "local"
	}
	if c.Storage.Local.Root == "" {
		c.Storage.Local.Root = filepath.Join(c.DataDir, "storage")
	}
	if c.Chunking.MinSize == 0 {
		c.Chunking.MinSize = 64 << 10
	}
	if c.Chunking.AvgSize == 0 {
		c.Chunking.AvgSize = 256 << 10
	}
	if c.Chunking.MaxSize == 0 {
		c.Chunking.MaxSize = 1 << 20
	}
	if c.Chunking.NarThreshold == 0 {
		c.Chunking.NarThreshold = c.Chunking.MinSize
	}
	if c.Compression.Level == "" {
		c.Compression.Level = "default"
	}
	if c.Parallelism <= 0 {
		c.Parallelism = runtime.NumCPU()
	}
	if c.GC.Grace == "" {
		c.GC.Grace = "1h"
	}
	if c.DefaultStorage == "" {
		c.DefaultStorage = "default"
	}
	// Named local backends need explicit roots — deriving them all from
	// data_dir would silently share one directory.
	for name, s := range c.Storages {
		if s.Backend == "" {
			s.Backend = "local"
		}
		if s.Backend == "local" && s.Local.Root == "" {
			s.Local.Root = filepath.Join(c.DataDir, "storage-"+name)
		}
		c.Storages[name] = s
	}
	if c.Logging == "" {
		c.Logging = "full"
	}
	if c.Durability == "" {
		c.Durability = "normal"
	}
}

func (c *Config) applyEnv() {
	if v := os.Getenv("XILO_ADMIN_PASSWORD"); v != "" {
		c.Admin.Password = v
	}
	if v := os.Getenv("XILO_S3_ACCESS_KEY"); v != "" {
		c.Storage.S3.AccessKey = v
	}
	if v := os.Getenv("XILO_S3_SECRET_KEY"); v != "" {
		c.Storage.S3.SecretKey = v
	}
	if v := os.Getenv("XILO_S3_ENDPOINT"); v != "" {
		c.Storage.S3.Endpoint = v
	}
	if v := os.Getenv("XILO_S3_BUCKET"); v != "" {
		c.Storage.S3.Bucket = v
		// A bucket from the environment means "use S3" — unless the config
		// file explicitly chose a backend.
		if c.Storage.Backend == "" {
			c.Storage.Backend = "s3"
		}
	}
	if v := os.Getenv("XILO_S3_REGION"); v != "" {
		c.Storage.S3.Region = v
	}
	if v := os.Getenv("XILO_S3_INSECURE"); v == "true" || v == "1" {
		c.Storage.S3.Insecure = true
	}
	if v := os.Getenv("XILO_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("XILO_BASE_URL"); v != "" {
		c.BaseURL = v
	}
	if v := os.Getenv("XILO_DATA_DIR"); v != "" {
		c.DataDir = v
	}
	if v := os.Getenv("XILO_DATABASE_URL"); v != "" {
		c.Database.URL = v
	}
}

func (c *Config) validate() error {
	switch c.Storage.Backend {
	case "local", "s3":
	default:
		return fmt.Errorf("storage.backend %q invalid (want local|s3)", c.Storage.Backend)
	}
	if c.Database.URL != "" && !c.Database.Postgres() {
		return fmt.Errorf("database.url %q invalid (want postgres://… or empty for SQLite)", c.Database.URL)
	}
	for name, s := range c.Storages {
		if name == "" || strings.ContainsAny(name, "/ ") {
			return fmt.Errorf("storages: invalid backend name %q", name)
		}
		switch s.Backend {
		case "local", "s3":
		default:
			return fmt.Errorf("storages.%s.backend %q invalid (want local|s3)", name, s.Backend)
		}
	}
	if _, ok := c.StorageMap()[c.DefaultStorage]; !ok {
		return fmt.Errorf("default_storage %q is not a configured storage", c.DefaultStorage)
	}
	switch c.Logging {
	case "full", "quiet":
	default:
		return fmt.Errorf("logging %q invalid (want full|quiet)", c.Logging)
	}
	switch c.Durability {
	case "normal", "full":
	default:
		return fmt.Errorf("durability %q invalid (want normal|full)", c.Durability)
	}
	return nil
}
