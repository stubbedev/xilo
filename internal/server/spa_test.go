package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// The admin's short private cache is what the hover-preload spends: the page
// fetched on hover has to still be in the browser's cache a click later. It is
// only safe because Vary: Cookie makes the flash cookie an invalidator.
func TestAdminPagesAreBrieflyCacheable(t *testing.T) {
	srv, db, _ := newTestServerCfg(t, nil)
	bootstrapAdmin(t, db)
	// The headers live in the middleware, which Serve wraps around the mux —
	// newTestServerCfg mounts the bare handler.
	ts := httptest.NewServer(srv.middleware(srv.Handler()))
	defer ts.Close()
	c := adminClient(t, ts)

	resp, err := c.Get(ts.URL + "/admin")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "private") || !strings.Contains(cc, "max-age=5") {
		t.Fatalf("Cache-Control = %q, want private max-age=5", cc)
	}
	if v := resp.Header.Get("Vary"); !strings.Contains(v, "Cookie") {
		t.Fatalf("Vary = %q, want Cookie", v)
	}
}

// A boosted request swaps the body and nothing else, so the palette and the
// language — both attributes of <html> — have to come back as a reload. The
// flash still travels on the cookie.
func TestAppearanceReloadsBoostedClients(t *testing.T) {
	_, db, ts := newTestServerCfg(t, nil)
	bootstrapAdmin(t, db)
	c := adminClient(t, ts)

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/admin/account/appearance",
		strings.NewReader(url.Values{"theme": {"gruvbox"}, "locale": {"de"}}.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	req.Header.Set("HX-Boosted", "true")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("boosted appearance → %d, want 204", resp.StatusCode)
	}
	if resp.Header.Get("HX-Refresh") != "true" {
		t.Fatalf("boosted appearance did not ask for a reload: %v", resp.Header)
	}
	if !hasFlashCookie(resp.Cookies()) {
		t.Fatal("boosted appearance dropped the flash")
	}

	// A fragment request (no HX-Boosted) keeps the client-side redirect, and a
	// plain form post keeps the 303: neither may turn into a 204.
	resp, err = c.PostForm(ts.URL+"/admin/account/appearance", url.Values{"theme": {""}, "locale": {""}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Request.URL.Path != "/admin/account" {
		t.Fatalf("plain appearance post → %d at %s", resp.StatusCode, resp.Request.URL.Path)
	}
}

func hasFlashCookie(cs []*http.Cookie) bool {
	for _, ck := range cs {
		if ck.Name == flashCookie && ck.Value != "" {
			return true
		}
	}
	return false
}
