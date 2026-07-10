package config

import (
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
