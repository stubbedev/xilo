package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stubbedev/xilo/internal/api"
)

// saveProfiles writes a client config with def as the default profile, and
// resets --profile so the default is what resolves.
func saveProfiles(t *testing.T, def string, servers map[string]serverProfile) {
	t.Helper()
	if err := saveClientConfig(clientConfig{Default: def, Servers: servers}); err != nil {
		t.Fatal(err)
	}
	profileFlag = ""
	t.Cleanup(func() { profileFlag = "" })
}

// authRecorder serves a private cache's config and remembers every
// Authorization header it was sent.
func authRecorder(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if a := r.Header.Get("Authorization"); a != "" {
			seen = append(seen, a)
		}
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/api/config") {
			json.NewEncoder(w).Encode(api.ConfigResp{PublicKey: "priv:KEYDATA", Public: false})
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(seen)
	}
}

func TestSameServer(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"https://home.example", "https://home.example", true},
		{"https://home.example", "https://home.example/", true},
		{"https://home.example", "https://HOME.example", true},
		{"https://home.example", "HTTPS://home.example", true},
		{"https://home.example", "https://home.example:443", true},
		{"http://home.example", "http://home.example:80/", true},
		{"https://home.example/xilo", "https://home.example/xilo/", true},

		{"https://home.example", "http://home.example", false},
		{"https://home.example", "https://home.example:80", false},
		{"https://home.example", "https://home.example:8443", false},
		{"https://home.example", "https://home.example.evil", false},
		{"https://home.example", "https://evil.example", false},
		{"https://home.example", "https://home.example/xilo", false},
		{"https://home.example", "home.example", false},
		{"https://home.example", "http://[bad", false},
		{"http://[bad", "http://[bad", false},
		{"", "", false},
	} {
		if got := sameServer(tc.a, tc.b); got != tc.want {
			t.Errorf("sameServer(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
		if got := sameServer(tc.b, tc.a); got != tc.want {
			t.Errorf("sameServer(%q, %q) = %v, want %v", tc.b, tc.a, got, tc.want)
		}
	}
}

// A saved token is a credential for its profile's server: a URL from --url or
// XILO_URL naming any other server must not be paired with it, and a profile
// without a token must not borrow one from another profile.
func TestResolveServerBindsTokenToItsServer(t *testing.T) {
	isolateEnv(t)
	saveProfiles(t, "home", map[string]serverProfile{
		"home":  {URL: "https://home.example", Token: "hometok"},
		"pull":  {URL: "https://admin.example"},
		"admin": {URL: "https://admin.example", Token: "admintok"},
	})

	check := func(name, urlFlag, tokenFlag, wantToken string) {
		t.Helper()
		if _, token := resolveServer(urlFlag, tokenFlag); token != wantToken {
			t.Errorf("%s: token=%q, want %q", name, token, wantToken)
		}
	}

	check("profile", "", "", "hometok")
	check("same server, other spelling", "https://HOME.example/", "", "hometok")
	check("foreign --url", "https://evil.example", "", "")

	profileFlag = "pull"
	check("token-less profile", "", "", "")
	profileFlag = ""

	t.Setenv("XILO_URL", "https://evil.example")
	check("foreign XILO_URL", "", "", "")

	// An explicit token is the caller's choice and still goes where it is told.
	check("--token", "", "flagtok", "flagtok")
	t.Setenv("XILO_TOKEN", "envtok")
	check("XILO_TOKEN", "", "", "envtok")
}

// `xilo use --url <other server>` must not send the saved token to that
// server, nor write it into ~/.netrc for its host.
func TestUseForeignServerGetsNoSavedToken(t *testing.T) {
	home := isolateEnv(t)
	saveProfiles(t, "home", map[string]serverProfile{
		"home": {URL: "https://home.example", Token: "hometok"},
	})
	foreign, seen := authRecorder(t)

	out, err := runRoot(t, "use", "acct/priv", "--url", foreign.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "note: private cache") {
		t.Fatalf("use output: %q", out)
	}
	if got := seen(); len(got) != 0 {
		t.Fatalf("foreign server was sent %q", got)
	}
	if _, err := os.Stat(filepath.Join(home, ".netrc")); !os.IsNotExist(err) {
		t.Fatal("netrc written for a foreign server")
	}
}

// The admin commands resolve their token in adminTarget, against --server.
func TestAdminForeignServerGetsNoSavedToken(t *testing.T) {
	isolateEnv(t)
	t.Chdir(t.TempDir())
	saveProfiles(t, "home", map[string]serverProfile{
		"home": {URL: "https://home.example", Token: "hometok"},
	})
	foreign, seen := authRecorder(t)

	_, err := runRoot(t, "cache", "list", "--server", foreign.URL)
	if err == nil || !strings.Contains(err.Error(), "admin token required") {
		t.Fatalf("cache list --server: err = %v, want admin-token-required", err)
	}
	if got := seen(); len(got) != 0 {
		t.Fatalf("foreign server was sent %q", got)
	}
}

// Re-pointing a profile at another server without a new token drops the old
// one; saving the same server again keeps it.
func TestLoginNewServerDropsSavedToken(t *testing.T) {
	isolateEnv(t)
	token := func() string {
		cc := loadClientConfig()
		return cc.Servers[cc.Default].Token
	}

	if _, err := runRoot(t, "login", "https://home.example", "--token", "hometok"); err != nil {
		t.Fatal(err)
	}
	if _, err := runRoot(t, "login", "https://HOME.example/"); err != nil {
		t.Fatal(err)
	}
	if got := token(); got != "hometok" {
		t.Fatalf("same server re-saved: token = %q, want hometok", got)
	}

	if _, err := runRoot(t, "login", "https://other.example"); err != nil {
		t.Fatal(err)
	}
	if got := token(); got != "" {
		t.Fatalf("after re-pointing: token = %q, want none", got)
	}
}
