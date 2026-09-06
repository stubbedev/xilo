package server

import (
	"net/http"
	"net/url"
	"strconv"
	"testing"
)

// TestAccountWallet covers the multi-account flow end to end: two people
// signed in at once in one browser, switching between them, and signing one
// out landing on the other rather than at the login page.
func TestAccountWallet(t *testing.T) {
	_, db, ts := newTestServerCfg(t, nil)
	bootstrapAdmin(t, db)
	bea, _ := db.CreateUser("bea", "", passHash(t, "beapass1234"), "user")

	c := loginAs(t, ts, "admin", adminPass)
	who := func() string {
		t.Helper()
		resp, err := c.Get(ts.URL + "/admin/account")
		if err != nil {
			t.Fatal(err)
		}
		b := body(t, resp)
		switch {
		case contains(b, "Signed in as admin."):
			return "admin"
		case contains(b, "Signed in as bea."):
			return "bea"
		}
		return "signed out"
	}

	// The sign-in page is reachable while signed in — that is how a second
	// account gets added without dropping the first.
	resp, err := c.Get(ts.URL + "/admin/signin")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("signin page while signed in → %v %v", resp.StatusCode, err)
	}
	resp.Body.Close()

	resp, err = c.PostForm(ts.URL+"/admin/login", url.Values{"username": {"bea"}, "password": {"beapass1234"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := who(); got != "bea" {
		t.Fatalf("after adding bea, active = %s", got)
	}

	// Switching goes back to the first account without re-authenticating.
	adminID := adminID(t, db)
	resp, err = c.PostForm(ts.URL+"/admin/session/switch", url.Values{"user": {strconv.FormatInt(adminID, 10)}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := who(); got != "admin" {
		t.Fatalf("after switch, active = %s", got)
	}

	// A user this browser never signed in as cannot be assumed: the wallet is
	// the proof, and there is no entry for them.
	carl, _ := db.CreateUser("carl", "", passHash(t, "carlpass1234"), "user")
	resp, err = c.PostForm(ts.URL+"/admin/session/switch", url.Values{"user": {strconv.FormatInt(carl.ID, 10)}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := who(); got != "admin" {
		t.Fatalf("switch to a stranger changed the session to %s", got)
	}

	// Signing out drops this account only and lands on the one still held.
	resp, err = c.PostForm(ts.URL+"/admin/logout", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := who(); got != "bea" {
		t.Fatalf("after logging admin out, active = %s want bea", got)
	}
	if _, err := db.GetUser(bea.ID); err != nil {
		t.Fatalf("bea gone: %v", err)
	}

	// The last one out is signed out for real.
	resp, err = c.PostForm(ts.URL+"/admin/logout", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := who(); got != "signed out" {
		t.Fatalf("after the last logout, active = %s", got)
	}
}
