package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseAndReplaceManagedBlock(t *testing.T) {
	// empty content -> well-formed block with trailing newline
	out := replaceManagedBlock("", []string{"s1"}, []string{"k1"})
	if !strings.HasSuffix(out, "\n") {
		t.Fatalf("expected trailing newline, got %q", out)
	}
	if !strings.Contains(out, blockStart) || !strings.Contains(out, blockEnd) {
		t.Fatalf("block markers missing: %q", out)
	}
	if !strings.Contains(out, "extra-substituters = s1") ||
		!strings.Contains(out, "extra-trusted-public-keys = k1") {
		t.Fatalf("block body wrong: %q", out)
	}

	// idempotency: applying twice with same args -> byte-identical
	out2 := replaceManagedBlock(out, []string{"s1"}, []string{"k1"})
	if out != out2 {
		t.Fatalf("not idempotent:\n%q\nvs\n%q", out, out2)
	}

	// accumulation: parse a block already containing sub1 -> [sub1]
	subs, keys := parseManagedBlock(out)
	if !reflect.DeepEqual(subs, []string{"s1"}) {
		t.Fatalf("parse subs = %v, want [s1]", subs)
	}
	if !reflect.DeepEqual(keys, []string{"k1"}) {
		t.Fatalf("parse keys = %v, want [k1]", keys)
	}

	// user's own non-block lines preserved across a replace
	withUser := "# my comment\nsandbox = true\n" + out
	replaced := replaceManagedBlock(withUser, []string{"s1", "s2"}, []string{"k1"})
	if !strings.Contains(replaced, "# my comment") || !strings.Contains(replaced, "sandbox = true") {
		t.Fatalf("user lines dropped: %q", replaced)
	}
	if !strings.Contains(replaced, "extra-substituters = s1 s2") {
		t.Fatalf("accumulation failed: %q", replaced)
	}
}

func TestParseManagedBlockMissingSubstituters(t *testing.T) {
	// block missing the extra-substituters line -> empty slices, no panic
	content := blockStart + "\nextra-trusted-public-keys = k1\n" + blockEnd + "\n"
	subs, keys := parseManagedBlock(content)
	if len(subs) != 0 {
		t.Fatalf("subs = %v, want empty", subs)
	}
	if !reflect.DeepEqual(keys, []string{"k1"}) {
		t.Fatalf("keys = %v, want [k1]", keys)
	}

	// entirely empty content -> empty slices, no panic
	if s, k := parseManagedBlock(""); len(s) != 0 || len(k) != 0 {
		t.Fatalf("empty content parse = %v %v", s, k)
	}
}

func TestFields(t *testing.T) {
	if got := fields("extra-substituters = a b c"); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("fields = %v, want [a b c]", got)
	}
	if got := fields("x ="); len(got) != 0 {
		t.Fatalf("fields(x =) = %v, want empty", got)
	}
	if got := fields("no equals here"); got != nil {
		t.Fatalf("fields(no =) = %v, want nil", got)
	}
}

func TestHostOf(t *testing.T) {
	cases := map[string]string{
		"https://c.example.com/": "c.example.com",
		"http://h:8080":          "h:8080",
		"h:9000":                 "h:9000",
	}
	for in, want := range cases {
		if got := hostOf(in); got != want {
			t.Errorf("hostOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUpdateNetrc(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".netrc")

	if err := updateNetrc("host", "tok"); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(body), "machine host login xilo password tok") != 1 {
		t.Fatalf("expected one entry, got %q", body)
	}

	// mode 0600
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
	}

	// same host again -> no dup
	if err := updateNetrc("host", "tok"); err != nil {
		t.Fatal(err)
	}
	body, _ = os.ReadFile(path)
	if strings.Count(string(body), "machine host") != 1 {
		t.Fatalf("duplicate host entry: %q", body)
	}

	// different host -> appended
	if err := updateNetrc("other", "tok2"); err != nil {
		t.Fatal(err)
	}
	body, _ = os.ReadFile(path)
	if !strings.Contains(string(body), "machine other login xilo password tok2") {
		t.Fatalf("second host not appended: %q", body)
	}
}

func TestResolvePaths(t *testing.T) {
	// normal args passthrough
	got, err := resolvePaths([]string{"/a", "/b"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"/a", "/b"}) {
		t.Fatalf("passthrough = %v", got)
	}

	// len != 1 with "-" -> literal (not stdin)
	got, err = resolvePaths([]string{"-", "x"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"-", "x"}) {
		t.Fatalf("literal = %v, want [- x]", got)
	}

	// stdin case: swap os.Stdin with a temp file, restore with defer
	f, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("/nix/store/a\n\n  /nix/store/b  \n"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	orig := os.Stdin
	os.Stdin = f
	defer func() { os.Stdin = orig }()

	got, err = resolvePaths([]string{"-"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"/nix/store/a", "/nix/store/b"}) {
		t.Fatalf("stdin = %v, want trimmed pair", got)
	}
}

func TestResolveServer(t *testing.T) {
	// point HOME + XDG at a temp dir so no real saved config leaks in
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))

	// default URL when nothing set
	t.Setenv("XILO_URL", "")
	t.Setenv("XILO_TOKEN", "")
	url, token := resolveServer("", "")
	if url != "http://localhost:8080" || token != "" {
		t.Fatalf("default: url=%q token=%q", url, token)
	}

	// env beats default
	t.Setenv("XILO_URL", "http://env")
	t.Setenv("XILO_TOKEN", "envtok")
	url, token = resolveServer("", "")
	if url != "http://env" || token != "envtok" {
		t.Fatalf("env: url=%q token=%q", url, token)
	}

	// flag beats env
	url, token = resolveServer("http://flag", "flagtok")
	if url != "http://flag" || token != "flagtok" {
		t.Fatalf("flag: url=%q token=%q", url, token)
	}
}
