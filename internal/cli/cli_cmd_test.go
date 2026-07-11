package cli

// Command-level tests: run the real cobra tree against a temp-dir config/DB,
// capturing stdout (commands print with fmt.Printf).

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stubbedev/xilo/internal/api"
	"github.com/stubbedev/xilo/internal/store"
)

// isolateEnv points HOME/XDG at temp dirs and clears xilo env vars so tests
// never touch the real ~ or a real server config.
func isolateEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	for _, v := range []string{"XILO_CONFIG", "XILO_URL", "XILO_TOKEN", "XILO_DATA_DIR", "XILO_LISTEN", "XILO_BASE_URL", "XILO_ADMIN_PASSWORD"} {
		t.Setenv(v, "")
		os.Unsetenv(v)
	}
	return home
}

func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	runErr := fn()
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	return string(out), runErr
}

func runRoot(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := Root()
	root.SetArgs(args)
	return captureStdout(t, root.Execute)
}

// writeConfig writes a minimal xilo.yaml with data_dir in a temp dir and
// returns its path.
func writeConfig(t *testing.T, extra string) string {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "xilo.yaml")
	body := "data_dir: " + filepath.Join(dir, "data") + "\n" + extra
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestCacheLifecycle(t *testing.T) {
	isolateEnv(t)
	cfg := writeConfig(t, "")

	out, err := runRoot(t, "--config", cfg, "cache", "create", "foo")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `created cache "default/foo"`) || !strings.Contains(out, "trusted-public-keys = foo:") {
		t.Fatalf("create output: %q", out)
	}
	pubkey := ""
	for _, line := range strings.Split(out, "\n") {
		if i := strings.Index(line, "trusted-public-keys = "); i >= 0 {
			pubkey = strings.TrimSpace(line[i+len("trusted-public-keys = "):])
		}
	}

	out, err = runRoot(t, "--config", cfg, "cache", "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "foo") || !strings.Contains(out, "public") || !strings.Contains(out, "priority=40") {
		t.Fatalf("list output: %q", out)
	}

	out, err = runRoot(t, "--config", cfg, "cache", "info", "foo")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"name:        default/foo", "visibility:  public", "retention:   global default", "max size:    unlimited", "paths:       0"} {
		if !strings.Contains(out, want) {
			t.Fatalf("info output missing %q: %q", want, out)
		}
	}

	out, err = runRoot(t, "--config", cfg, "cache", "configure", "foo",
		"--private", "--priority", "10", "--retention", "24h", "--max-size", "1KB")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "private") || !strings.Contains(out, "priority=10") || !strings.Contains(out, "retention=24h") || !strings.Contains(out, "1024 bytes") {
		t.Fatalf("configure output: %q", out)
	}

	out, err = runRoot(t, "--config", cfg, "cache", "rotate", "foo")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "rotated key for default/foo") {
		t.Fatalf("rotate output: %q", out)
	}
	if pubkey != "" && strings.Contains(out, pubkey) {
		t.Fatal("rotate did not change the public key")
	}

	// destroy refuses without --yes
	if _, err := runRoot(t, "--config", cfg, "cache", "destroy", "foo"); err == nil ||
		!strings.Contains(err.Error(), "--yes") {
		t.Fatalf("destroy without --yes: err = %v", err)
	}
	if _, err := runRoot(t, "--config", cfg, "cache", "destroy", "foo", "--yes"); err != nil {
		t.Fatal(err)
	}
	if _, err := runRoot(t, "--config", cfg, "cache", "info", "foo"); err == nil {
		t.Fatal("info on destroyed cache should error")
	}
}

func TestTokenLifecycle(t *testing.T) {
	isolateEnv(t)
	cfg := writeConfig(t, "")

	// no perms -> error
	if _, err := runRoot(t, "--config", cfg, "token", "create", "t0"); err == nil ||
		!strings.Contains(err.Error(), "--push / --pull") {
		t.Fatalf("no-perm create: err = %v", err)
	}

	out, err := runRoot(t, "--config", cfg, "token", "create", "t1", "--push", "--pull", "--cache", "foo", "--ttl", "720h")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `token "t1" (id=1) perms=push,pull scope=default/foo`) || !strings.Contains(out, "Store it now") {
		t.Fatalf("create output: %q", out)
	}

	out, err = runRoot(t, "--config", cfg, "token", "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "t1") || !strings.Contains(out, "active") || !strings.Contains(out, "scope=default/foo") {
		t.Fatalf("list output: %q", out)
	}

	if _, err := runRoot(t, "--config", cfg, "token", "revoke", "notanumber"); err == nil {
		t.Fatal("revoke with bad id should error")
	}
	out, err = runRoot(t, "--config", cfg, "token", "revoke", "1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "revoked token 1") {
		t.Fatalf("revoke output: %q", out)
	}
	out, _ = runRoot(t, "--config", cfg, "token", "list")
	if !strings.Contains(out, "REVOKED") {
		t.Fatalf("list after revoke: %q", out)
	}
}

func TestGCCommand(t *testing.T) {
	isolateEnv(t)
	cfg := writeConfig(t, "")

	out, err := runRoot(t, "--config", cfg, "gc", "--older-than", "1h")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "evicted 0 paths older than 1h0m0s") || !strings.Contains(out, "removed 0 chunks, freed 0 bytes") {
		t.Fatalf("gc output: %q", out)
	}
}

func TestGCBadGraceFallsBack(t *testing.T) {
	isolateEnv(t)
	cfg := writeConfig(t, "gc:\n  grace: banana\n")

	out, err := runRoot(t, "--config", cfg, "gc")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `bad gc.grace "banana", using 1h`) {
		t.Fatalf("gc output: %q", out)
	}
}

func TestSchemaDump(t *testing.T) {
	isolateEnv(t)
	outFile := filepath.Join(t.TempDir(), "schema.json")
	if _, err := runRoot(t, "schema", "dump", "--out", outFile); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("schema not valid JSON: %v", err)
	}
	if _, ok := v["properties"]; !ok {
		t.Fatalf("schema has no properties: %v", v)
	}

	// stdout mode
	out, err := runRoot(t, "schema", "dump")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("stdout schema not valid JSON: %v", err)
	}
}

func TestLoginSavesClientConfig(t *testing.T) {
	isolateEnv(t)

	out, err := runRoot(t, "login", "http://srv:8080/", "--token", "tok123")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "saved http://srv:8080 to ") {
		t.Fatalf("login output: %q", out)
	}
	cc := loadClientConfig()
	if cc.URL != "http://srv:8080" || cc.Token != "tok123" {
		t.Fatalf("saved config = %+v", cc)
	}
	fi, err := os.Stat(clientConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %v, want 0600", fi.Mode().Perm())
	}

	// resolveServer picks the saved values up
	url, token := resolveServer("", "")
	if url != "http://srv:8080" || token != "tok123" {
		t.Fatalf("resolveServer from saved config: %q %q", url, token)
	}
}

func TestLoginTokenFromEnv(t *testing.T) {
	isolateEnv(t)
	t.Setenv("XILO_TOKEN", "envtok")
	if _, err := runRoot(t, "login", "http://srv"); err != nil {
		t.Fatal(err)
	}
	if cc := loadClientConfig(); cc.Token != "envtok" {
		t.Fatalf("token = %q, want envtok", cc.Token)
	}
}

func cacheConfigServer(t *testing.T, public bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) != 4 || parts[2] != "api" || parts[3] != "config" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(api.ConfigResp{
			PublicKey: parts[1] + ":KEYDATA",
			Public:    public,
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestUseAddAndRemove(t *testing.T) {
	home := isolateEnv(t)
	srv := cacheConfigServer(t, true)
	nixConf := filepath.Join(home, ".config", "nix", "nix.conf")

	out, err := runRoot(t, "use", "c1", "--url", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "added "+srv.URL+"/default/c1 to nix.conf") {
		t.Fatalf("use output: %q", out)
	}
	body, err := os.ReadFile(nixConf)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "extra-substituters = "+srv.URL+"/default/c1") ||
		!strings.Contains(string(body), "extra-trusted-public-keys = c1:KEYDATA") {
		t.Fatalf("nix.conf: %q", body)
	}

	// second cache accumulates
	if _, err := runRoot(t, "use", "c2", "--url", srv.URL); err != nil {
		t.Fatal(err)
	}
	body, _ = os.ReadFile(nixConf)
	if !strings.Contains(string(body), srv.URL+"/default/c1 "+srv.URL+"/default/c2") ||
		!strings.Contains(string(body), "c1:KEYDATA c2:KEYDATA") {
		t.Fatalf("nix.conf after second use: %q", body)
	}

	// remove one of two
	out, err = runRoot(t, "use", "c1", "--remove", "--url", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "removed "+srv.URL+"/default/c1 from nix.conf") {
		t.Fatalf("remove output: %q", out)
	}
	body, _ = os.ReadFile(nixConf)
	s := string(body)
	if strings.Contains(s, srv.URL+"/default/c1") || strings.Contains(s, "c1:KEYDATA") {
		t.Fatalf("c1 not removed: %q", s)
	}
	if !strings.Contains(s, srv.URL+"/default/c2") || !strings.Contains(s, "c2:KEYDATA") {
		t.Fatalf("c2 lost during remove: %q", s)
	}
}

func TestUsePrivateCacheWritesNetrc(t *testing.T) {
	home := isolateEnv(t)
	srv := cacheConfigServer(t, false)

	out, err := runRoot(t, "use", "priv", "--url", srv.URL, "--token", "pulltok")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "added pull token to ~/.netrc") {
		t.Fatalf("use output: %q", out)
	}
	body, err := os.ReadFile(filepath.Join(home, ".netrc"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "machine "+hostOf(srv.URL)+" login xilo password pulltok") {
		t.Fatalf("netrc: %q", body)
	}
}

func TestUsePrivateCacheNoTokenNote(t *testing.T) {
	home := isolateEnv(t)
	srv := cacheConfigServer(t, false)

	out, err := runRoot(t, "use", "priv", "--url", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "note: private cache") {
		t.Fatalf("expected login note, got %q", out)
	}
	if _, err := os.Stat(filepath.Join(home, ".netrc")); !os.IsNotExist(err) {
		t.Fatal("netrc written without a token")
	}
}

func TestUseFetchConfigError(t *testing.T) {
	isolateEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no such cache", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	_, err := runRoot(t, "use", "ghost", "--url", srv.URL)
	if err == nil || !strings.Contains(err.Error(), "fetch cache config") {
		t.Fatalf("err = %v, want fetch cache config", err)
	}
}

func TestUseRemoveNoNixConf(t *testing.T) {
	isolateEnv(t)
	// no nix.conf exists -> remove is a silent no-op
	out, err := runRoot(t, "use", "c1", "--remove", "--url", "http://x")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "removed") {
		t.Fatalf("out = %q", out)
	}
}

func TestPushCmdEmptyStdin(t *testing.T) {
	isolateEnv(t)
	f, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdin
	os.Stdin = f
	defer func() { os.Stdin = orig }()

	// "-" with empty stdin resolves to zero paths -> nil without any network
	if _, err := runRoot(t, "push", "somecache", "-"); err != nil {
		t.Fatal(err)
	}
}

func TestServeBadConfig(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	cfg := filepath.Join(dir, "xilo.yaml")
	if err := os.WriteFile(cfg, []byte("listen: ["), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runRoot(t, "--config", cfg, "serve"); err == nil {
		t.Fatal("serve with malformed yaml should error")
	}
}

func TestOpenDBBadConfig(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	cfg := filepath.Join(dir, "xilo.yaml")
	if err := os.WriteFile(cfg, []byte("storage:\n  backend: bogus\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runRoot(t, "--config", cfg, "cache", "list"); err == nil ||
		!strings.Contains(err.Error(), "storage.backend") {
		t.Fatalf("err = %v, want storage.backend validation error", err)
	}
}

func TestBootstrapAdmin(t *testing.T) {
	isolateEnv(t)
	db, err := store.Open(filepath.Join(t.TempDir(), "xilo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// empty password -> no-op
	if err := bootstrapAdmin(db, ""); err != nil {
		t.Fatal(err)
	}
	if db.UsersExist() {
		t.Fatal("admin created from empty password")
	}

	if err := bootstrapAdmin(db, "hunter2"); err != nil {
		t.Fatal(err)
	}
	if !db.UsersExist() {
		t.Fatal("admin not created")
	}
	u1, err := db.GetUserByName("admin")
	if err != nil {
		t.Fatal(err)
	}
	hash1 := u1.PassHash

	// second bootstrap with a different password must not overwrite
	if err := bootstrapAdmin(db, "other"); err != nil {
		t.Fatal(err)
	}
	u2, _ := db.GetUserByName("admin")
	hash2 := u2.PassHash
	if hash1 != hash2 {
		t.Fatal("existing admin credential overwritten")
	}
}

func TestDefaultConfig(t *testing.T) {
	isolateEnv(t)

	// 1. explicit env wins
	t.Setenv("XILO_CONFIG", "/some/where/xilo.yaml")
	if got := defaultConfig(); got != "/some/where/xilo.yaml" {
		t.Fatalf("env: %q", got)
	}
	t.Setenv("XILO_CONFIG", "")
	os.Unsetenv("XILO_CONFIG")

	// 2. xilo.yaml in cwd
	cwd := t.TempDir()
	t.Chdir(cwd)
	if err := os.WriteFile(filepath.Join(cwd, "xilo.yaml"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := defaultConfig(); got != "xilo.yaml" {
		t.Fatalf("cwd: %q", got)
	}

	// 3. XDG user config
	if err := os.Remove(filepath.Join(cwd, "xilo.yaml")); err != nil {
		t.Fatal(err)
	}
	xdgPath := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "xilo", "xilo.yaml")
	if err := os.MkdirAll(filepath.Dir(xdgPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(xdgPath, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := defaultConfig(); got != xdgPath {
		t.Fatalf("xdg: %q, want %q", got, xdgPath)
	}

	// 4. final fallback (only assertable when /etc/xilo/xilo.yaml is absent)
	if err := os.Remove(xdgPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat("/etc/xilo/xilo.yaml"); os.IsNotExist(err) {
		if got := defaultConfig(); got != "xilo.yaml" {
			t.Fatalf("fallback: %q", got)
		}
	}
}

func TestRootHasSubcommands(t *testing.T) {
	isolateEnv(t)
	root := Root()
	want := []string{"serve", "push", "watch", "login", "use", "cache", "token", "gc", "schema"}
	for _, name := range want {
		found := false
		for _, c := range root.Commands() {
			if c.Name() == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing subcommand %q", name)
		}
	}
}

func TestConfigDirFallback(t *testing.T) {
	// UserConfigDir errors when both XDG_CONFIG_HOME and HOME are unset.
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")
	os.Unsetenv("XDG_CONFIG_HOME")
	os.Unsetenv("HOME")
	if got := configDir(); got != ".config" {
		t.Fatalf("configDir fallback = %q", got)
	}
	if got := clientConfigPath(); got != filepath.Join(".config", "xilo", "config.yaml") {
		t.Fatalf("clientConfigPath fallback = %q", got)
	}
}

func TestPushCmdServerError(t *testing.T) {
	isolateEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	_, err := runRoot(t, "push", "c", "/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-x", "--url", srv.URL, "--quiet")
	if err == nil || !strings.Contains(err.Error(), "fetch server config") {
		t.Fatalf("err = %v, want fetch server config", err)
	}
}

func TestUseFetchConfigBadJSON(t *testing.T) {
	isolateEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("{oops"))
	}))
	t.Cleanup(srv.Close)

	if _, err := runRoot(t, "use", "c", "--url", srv.URL); err == nil {
		t.Fatal("expected decode error")
	}
}

func TestServeDBOpenError(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	// xilo.db as a *directory* makes store.Open fail after config loads fine
	if err := os.MkdirAll(filepath.Join(dataDir, "xilo.db"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "xilo.yaml")
	if err := os.WriteFile(cfg, []byte("data_dir: "+dataDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runRoot(t, "--config", cfg, "serve"); err == nil {
		t.Fatal("serve with unopenable db should error")
	}
}

func TestOpenDBMkdirError(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	// data_dir is an existing *file* -> MkdirAll fails
	dataDir := filepath.Join(dir, "data")
	if err := os.WriteFile(dataDir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "xilo.yaml")
	if err := os.WriteFile(cfg, []byte("data_dir: "+dataDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runRoot(t, "--config", cfg, "cache", "list"); err == nil {
		t.Fatal("openDB with file as data_dir should error")
	}
}

func TestGCStorageError(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	// storage root is an existing *file* -> storage.New fails after openDB works
	rootFile := filepath.Join(dir, "storageroot")
	if err := os.WriteFile(rootFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "xilo.yaml")
	body := "data_dir: " + filepath.Join(dir, "data") + "\nstorage:\n  local:\n    root: " + rootFile + "\n"
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runRoot(t, "--config", cfg, "gc"); err == nil {
		t.Fatal("gc with bad storage root should error")
	}
}

func TestCacheCommandsOnMissingCache(t *testing.T) {
	isolateEnv(t)
	cfg := writeConfig(t, "")
	for _, args := range [][]string{
		{"cache", "info", "ghost"},
		{"cache", "configure", "ghost", "--public"},
		{"cache", "rotate", "ghost"},
		{"cache", "destroy", "ghost", "--yes"},
	} {
		if _, err := runRoot(t, append([]string{"--config", cfg}, args...)...); err == nil {
			t.Errorf("%v on missing cache should error", args)
		}
	}
}

func TestLoginSaveError(t *testing.T) {
	home := isolateEnv(t)
	// XDG_CONFIG_HOME pointing at a *file* makes MkdirAll fail in save
	blocker := filepath.Join(home, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", blocker)
	if _, err := runRoot(t, "login", "http://srv", "--token", "t"); err == nil {
		t.Fatal("login with unwritable config dir should error")
	}
}
