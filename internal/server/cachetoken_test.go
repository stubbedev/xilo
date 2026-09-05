package server

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
)

// Minting from the cache page must take its scope from the page, not from the
// sidebar's viewing-context cookie. Before, switching context silently moved
// the token to another account.
func TestCacheTokenIgnoresViewingContext(t *testing.T) {
	_, db, ts := newTestServerCfg(t, nil)
	bootstrapAdmin(t, db)
	acme, err := db.EnsureAccount("acme", "org")
	if err != nil {
		t.Fatal(err)
	}
	other, err := db.EnsureAccount("other", "org")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateCache("acme", "web", false, 40); err != nil {
		t.Fatal(err)
	}
	c := adminClient(t, ts)

	// Point the viewing context at the *wrong* account.
	resp, err := c.PostForm(ts.URL+"/admin/context", url.Values{"ctx": {"other"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	resp, err = c.PostForm(ts.URL+"/admin/cache/acme/web/tokens",
		url.Values{"name": {"ci"}, "push": {"on"}, "manage": {"on"}, "permanent": {"on"}})
	if err != nil {
		t.Fatal(err)
	}
	page := body(t, resp)

	toks, err := db.ListTokens()
	if err != nil || len(toks) != 1 {
		t.Fatalf("tokens: %+v %v", toks, err)
	}
	tok := toks[0]
	if tok.AccountID != acme.ID || tok.AccountID == other.ID {
		t.Fatalf("token landed on account %d, want acme (%d)", tok.AccountID, acme.ID)
	}
	if len(tok.Caches) != 1 || tok.Caches[0] != "web" {
		t.Fatalf("scope = %v", tok.Caches)
	}
	// "manage" is one switch standing for the three enforced perms.
	for _, p := range []string{"push", "create", "configure", "destroy"} {
		if !slices.Contains(tok.Perms, p) {
			t.Fatalf("perms = %v, missing %s", tok.Perms, p)
		}
	}

	// The secret is rendered into the copy-ready snippets, not left as a
	// <token> placeholder to be filled in by hand.
	if strings.Contains(page, "<token>") || strings.Contains(page, "&lt;token&gt;") {
		t.Error("page still shows a <token> placeholder after minting")
	}
	// A private cache must show its netrc line — nix pulls send basic auth.
	if !strings.Contains(page, "machine ") || !strings.Contains(page, "login xilo password") {
		t.Error("private cache page has no netrc line")
	}
}

// A member who cannot administer the account must not be able to mint for it.
func TestCacheTokenRequiresManageRights(t *testing.T) {
	_, db, ts := newTestServerCfg(t, nil)
	bootstrapAdmin(t, db)
	acme, err := db.EnsureAccount("acme", "org")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateCache("acme", "web", true, 40); err != nil {
		t.Fatal(err)
	}
	plain, err := db.CreateUser("plain", "", passHash(t, "plainpass123"), "user")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetMember(acme.ID, plain.ID, "user"); err != nil {
		t.Fatal(err)
	}

	pc := loginAs(t, ts, "plain", "plainpass123")
	resp, err := pc.PostForm(ts.URL+"/admin/cache/acme/web/tokens", url.Values{"name": {"sneaky"}, "push": {"on"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusInternalServerError {
		t.Fatalf("unexpected 500")
	}
	if toks, _ := db.ListTokens(); len(toks) != 0 {
		t.Fatalf("plain member minted a token: %+v", toks)
	}
}
