package server

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/stubbedev/xilo/internal/api"
)

// A token must be able to describe itself: that is the read behind
// `xilo status`, and the thing that turns an opaque 401 into a diagnosis.
func TestWhoami(t *testing.T) {
	_, db, ts := newTestServerCfg(t, nil)
	acct, err := db.EnsureAccount("acme", "org")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateCache("acme", "web", true, 40); err != nil {
		t.Fatal(err)
	}
	scoped, _, err := db.CreateToken(acct.ID, "ci", []string{"web"}, []string{"push", "manage"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	root, _, err := db.CreateToken(0, "root", nil, []string{"admin"}, 0)
	if err != nil {
		t.Fatal(err)
	}

	get := func(tok string) (int, api.WhoamiResp) {
		resp, body := apiReq(t, ts, http.MethodGet, "/api/v1/whoami", tok, nil)
		var w api.WhoamiResp
		json.Unmarshal(body, &w)
		return resp.StatusCode, w
	}

	code, w := get(scoped)
	if code != http.StatusOK {
		t.Fatalf("scoped token → %d", code)
	}
	if w.Token != "ci" || w.Account != "acme" || w.Cache != "acme/web" || w.Admin {
		t.Fatalf("scoped whoami = %+v", w)
	}
	// "manage" is stored as the three perms it stands for.
	for _, p := range []string{"create", "configure", "destroy"} {
		if !slices.Contains(w.Perms, p) {
			t.Fatalf("manage did not expand: %+v", w.Perms)
		}
	}

	// An admin token names no cache — reporting one would be a lie, and
	// reporting "all caches" would be a worse one.
	code, w = get(root)
	if code != http.StatusOK || w.Cache != "" || !w.Admin || w.Account != "" {
		t.Fatalf("admin whoami = %d %+v", code, w)
	}

	// Unknown, revoked and absent secrets are all just 401.
	if code, _ = get("garbage"); code != http.StatusUnauthorized {
		t.Fatalf("garbage token → %d", code)
	}
	if code, _ = get(""); code != http.StatusUnauthorized {
		t.Fatalf("no token → %d", code)
	}
}

// The API must not invent an account when none is named: that is what created
// the phantom "default" organization on a first CLI command.
func TestCreateCacheNeedsAnAccount(t *testing.T) {
	_, db, ts := newTestServerCfg(t, nil)
	acct, err := db.EnsureAccount("acme", "org")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateCache("acme", "seed", true, 40); err != nil {
		t.Fatal(err)
	}
	admin, _, err := db.CreateToken(0, "root", nil, []string{"admin"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	creator, _, err := db.CreateToken(acct.ID, "creator", []string{"new"}, []string{"manage"}, 0)
	if err != nil {
		t.Fatal(err)
	}

	// An instance-wide token names no account, so it must say which one.
	resp, _ := apiReq(t, ts, http.MethodPost, "/api/v1/caches", admin, api.CreateCacheReq{Name: "orphan"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("accountless create → %d, want 400", resp.StatusCode)
	}
	if _, err := db.GetAccount("default"); err == nil {
		t.Fatal(`a "default" account was conjured`)
	}

	// A scoped token has an account of its own, so a bare name resolves.
	resp, body := apiReq(t, ts, http.MethodPost, "/api/v1/caches", creator, api.CreateCacheReq{Name: "new"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("scoped create → %d %s", resp.StatusCode, body)
	}
	var c api.Cache
	json.Unmarshal(body, &c)
	if c.Account != "acme" {
		t.Fatalf("created in %q, want acme", c.Account)
	}
}
