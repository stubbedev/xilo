package config

import (
	"os"
	"path/filepath"
	"testing"
)

// A config file is the one input an operator hands the server by hand, so a
// malformed one must produce a diagnosable error at boot rather than a panic
// or, worse, a Config that silently means something other than what it says.

// FuzzParseBytes covers the size grammar behind every limit in the file
// (max-size, limits.total, a plan's storage cap). It is a hand-rolled
// suffix-and-float parser, which is exactly the shape that panics on a
// pathological string, and a size that parses to the wrong number is a quota
// that does not hold.
func FuzzParseBytes(f *testing.F) {
	f.Add("10GB")
	f.Add("1.5 TiB")
	f.Add("0")
	f.Add("")
	f.Add("B")
	f.Add("-1K")
	f.Add("1e309T")
	f.Add("NaNMB")
	f.Add("InfGB")

	f.Fuzz(func(t *testing.T, s string) {
		n, err := ParseBytes(s)
		if err != nil {
			return
		}
		// A byte count is a size. Negative is meaningless, and a NaN or
		// overflowed float landing in an int64 is how a cap becomes a
		// nonsense number that compares wrong against real usage.
		if n < 0 {
			t.Fatalf("ParseBytes(%q) = %d, a negative size", s, n)
		}
	})
}

// FuzzLoad runs the real loader over arbitrary YAML. Boot must survive
// anything here, because the alternative is a server that will not start and
// cannot say why.
func FuzzLoad(f *testing.F) {
	f.Add("listen: \":8080\"\ndata_dir: /data\n")
	f.Add("multi_tenant: true\n")
	f.Add("limits:\n  total: 10GB\n")
	f.Add("")
	f.Add("[")
	f.Add("listen: [1, 2]\n")
	f.Add("storage:\n  backend: s3\n")

	dir := f.TempDir()
	f.Fuzz(func(t *testing.T, body string) {
		path := filepath.Join(dir, "xilo.yaml")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
		cfg, err := Load(path)
		if err != nil {
			return
		}
		if cfg == nil {
			t.Fatalf("Load(%q) returned no config and no error", body)
		}
		// Defaults are the loader's other job: a config that parses must come
		// back usable, or the server boots into a state no field explains.
		if cfg.Listen == "" {
			t.Fatalf("Load(%q) left Listen empty", body)
		}
		if cfg.DataDir == "" {
			t.Fatalf("Load(%q) left DataDir empty", body)
		}
	})
}
