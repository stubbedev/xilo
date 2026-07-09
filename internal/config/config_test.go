package config

import (
	"path/filepath"
	"testing"
)

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
