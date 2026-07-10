package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseBytes(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"", 0}, {"0", 0},
		{"1024", 1024},
		{"1KB", 1 << 10}, {"1K", 1 << 10}, {"1KiB", 1 << 10},
		{"1MB", 1 << 20}, {"500GB", 500 << 30}, {"2TB", 2 << 40},
		{"1.5GB", int64(1.5 * float64(1<<30))},
		{" 10 GB ", 10 << 30},
	}
	for _, c := range cases {
		got, err := ParseBytes(c.in)
		if err != nil {
			t.Errorf("ParseBytes(%q) error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseBytes(%q)=%d want %d", c.in, got, c.want)
		}
	}
	if _, err := ParseBytes("garbage"); err == nil {
		t.Error("expected error for garbage")
	}
	if _, err := ParseBytes("xxGB"); err == nil {
		t.Error("expected error for non-numeric size with unit suffix")
	}
}

func TestTotalBytes(t *testing.T) {
	if got := (Limits{Total: "1GB"}).TotalBytes(); got != 1<<30 {
		t.Fatalf("TotalBytes = %d", got)
	}
	if got := (Limits{Total: "junk"}).TotalBytes(); got != 0 {
		t.Fatalf("TotalBytes(junk) = %d, want 0", got)
	}
}

func TestLoadYAMLFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "xilo.yaml")
	yaml := `
listen: ":9999"
base_url: "https://cache.example.com"
data_dir: "/var/lib/xilo"
admin:
  password: "hunter2"
storage:
  backend: s3
  s3:
    endpoint: "minio.local:9000"
    bucket: "chunks"
    region: "eu-west-1"
    access_key: "ak"
    secret_key: "sk"
    insecure: true
chunking:
  min_size: 1024
  avg_size: 2048
  max_size: 4096
  nar_threshold: 512
compression:
  level: best
gc:
  interval: 12h
  retention: 720h
  grace: 2h
parallelism: 3
upstream_keys: ["cache.nixos.org-1"]
security:
  skip_upload_verify: true
  allow_open_bootstrap: true
limits:
  total: "500GB"
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":9999" || c.BaseURL != "https://cache.example.com" || c.DataDir != "/var/lib/xilo" {
		t.Fatalf("core fields: %+v", c)
	}
	if c.Admin.Password != "hunter2" || c.Storage.Backend != "s3" || !c.Storage.S3.Insecure {
		t.Fatalf("admin/storage: %+v", c)
	}
	if c.Storage.S3.Endpoint != "minio.local:9000" || c.Storage.S3.Bucket != "chunks" ||
		c.Storage.S3.AccessKey != "ak" || c.Storage.S3.SecretKey != "sk" {
		t.Fatalf("s3: %+v", c.Storage.S3)
	}
	if c.Chunking.NarThreshold != 512 || c.Compression.Level != "best" || c.Parallelism != 3 {
		t.Fatalf("tuning: %+v", c)
	}
	if c.GC.Grace != "2h" || len(c.UpstreamKeys) != 1 || !c.Security.SkipUploadVerify || !c.Security.AllowOpenBootstrap {
		t.Fatalf("gc/security: %+v", c)
	}
	if c.Limits.TotalBytes() != 500<<30 {
		t.Fatalf("limits: %d", c.Limits.TotalBytes())
	}
}

func TestLoadMalformedYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(path, []byte("listen: [unclosed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestLoadUnreadableFile(t *testing.T) {
	// A directory path fails ReadFile with a non-IsNotExist error.
	if _, err := Load(t.TempDir()); err == nil {
		t.Fatal("expected read error for directory path")
	}
}

func TestValidateBadBackend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "xilo.yaml")
	if err := os.WriteFile(path, []byte("storage:\n  backend: bogus\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected invalid backend error")
	}
}

func TestEnvOverrides(t *testing.T) {
	t.Setenv("XILO_ADMIN_PASSWORD", "envpw")
	t.Setenv("XILO_S3_ACCESS_KEY", "envak")
	t.Setenv("XILO_S3_SECRET_KEY", "envsk")
	t.Setenv("XILO_BASE_URL", "https://env.example.com")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Admin.Password != "envpw" || c.Storage.S3.AccessKey != "envak" || c.Storage.S3.SecretKey != "envsk" {
		t.Fatalf("env secrets: %+v", c)
	}
	if c.BaseURL != "https://env.example.com" {
		t.Fatalf("base url = %q", c.BaseURL)
	}
}

func TestS3InsecureEnv(t *testing.T) {
	for _, tc := range []struct {
		val  string
		want bool
	}{{"1", true}, {"true", true}, {"false", false}, {"", false}} {
		t.Setenv("XILO_S3_INSECURE", tc.val)
		c, err := Load("")
		if err != nil {
			t.Fatal(err)
		}
		if c.Storage.S3.Insecure != tc.want {
			t.Fatalf("XILO_S3_INSECURE=%q → %v, want %v", tc.val, c.Storage.S3.Insecure, tc.want)
		}
	}
}

func TestEnvBucketDoesNotOverrideExplicitBackend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "xilo.yaml")
	if err := os.WriteFile(path, []byte("storage:\n  backend: local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XILO_S3_BUCKET", "xilo")
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Storage.Backend != "local" {
		t.Fatalf("backend = %q, want local (explicit yaml wins)", c.Storage.Backend)
	}
	if c.Storage.S3.Bucket != "xilo" {
		t.Fatalf("bucket = %q", c.Storage.S3.Bucket)
	}
}

func TestDefaults(t *testing.T) {
	c, err := Load("/does/not/exist.yaml") // missing file → defaults
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8080" || c.Storage.Backend != "local" {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	if c.Storage.Local.Root != filepath.Join("./data", "storage") {
		t.Fatalf("local root = %q", c.Storage.Local.Root)
	}
	if c.BaseURL != "http://localhost:8080" {
		t.Fatalf("base url = %q", c.BaseURL)
	}
}

func TestS3FromEnv(t *testing.T) {
	t.Setenv("XILO_S3_BUCKET", "xilo")
	t.Setenv("XILO_S3_ENDPOINT", "s3.example.com")
	t.Setenv("XILO_S3_REGION", "us-east-1")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	// A bucket from env selects the s3 backend without a config file.
	if c.Storage.Backend != "s3" {
		t.Fatalf("backend = %q, want s3", c.Storage.Backend)
	}
	if c.Storage.S3.Endpoint != "s3.example.com" || c.Storage.S3.Region != "us-east-1" {
		t.Fatalf("s3 config = %+v", c.Storage.S3)
	}
}

func TestEnvBeforeDefaults(t *testing.T) {
	t.Setenv("XILO_DATA_DIR", "/srv/xilo")
	t.Setenv("XILO_LISTEN", ":9000")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	// Derived defaults must build on the env-overridden data dir + listen.
	if c.Storage.Local.Root != filepath.Join("/srv/xilo", "storage") {
		t.Fatalf("local root = %q", c.Storage.Local.Root)
	}
	if c.BaseURL != "http://localhost:9000" {
		t.Fatalf("base url = %q", c.BaseURL)
	}
	if c.DBPath() != filepath.Join("/srv/xilo", "xilo.db") {
		t.Fatalf("db path = %q", c.DBPath())
	}
}
