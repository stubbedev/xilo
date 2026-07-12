package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
	"golang.org/x/crypto/bcrypt"

	"github.com/stubbedev/xilo/internal/config"
	"github.com/stubbedev/xilo/internal/store"
)

const adminPass = "hunter2boogaloo"

// bootstrapAdmin sets the admin password directly in the store.
func bootstrapAdmin(t *testing.T, db *store.DB) {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(adminPass), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateUser("admin", "", string(hash), "owner"); err != nil {
		t.Fatal(err)
	}
}

// adminID returns the bootstrapped admin account's id.
func adminID(t *testing.T, db *store.DB) int64 {
	t.Helper()
	u, err := db.GetUserByName("admin")
	if err != nil {
		t.Fatal(err)
	}
	return u.ID
}

// adminClient logs in and returns a cookie-jar client plus the final response body.
func adminClient(t *testing.T, ts *httptest.Server) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	resp, err := c.PostForm(ts.URL+"/admin/login", url.Values{"username": {"admin"}, "password": {adminPass}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Request.URL.Path != "/admin" {
		t.Fatalf("login → %d at %s", resp.StatusCode, resp.Request.URL)
	}
	return c
}

func body(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAdminLoginLogout(t *testing.T) {
	_, db, ts := newTestServerCfg(t, nil)

	// no admin yet: login POST re-renders the bootstrap login page
	resp, _ := http.PostForm(ts.URL+"/admin/login", url.Values{"username": {"admin"}, "password": {"x"}})
	if resp.StatusCode != 200 {
		t.Fatalf("pre-bootstrap login → %d", resp.StatusCode)
	}
	resp.Body.Close()

	bootstrapAdmin(t, db)

	// wrong password
	resp, _ = http.PostForm(ts.URL+"/admin/login", url.Values{"username": {"admin"}, "password": {"nope"}})
	if b := body(t, resp); !strings.Contains(b, "Invalid username or password") {
		t.Fatalf("wrong password should re-render login: %q", b)
	}

	// anonymous GET /admin shows the login page, not the dashboard
	resp, _ = http.Get(ts.URL + "/admin")
	if resp.StatusCode != 200 {
		t.Fatalf("anon /admin → %d", resp.StatusCode)
	}
	resp.Body.Close()

	c := adminClient(t, ts)

	// logged in: settings reachable
	resp, _ = c.Get(ts.URL + "/admin/settings")
	if resp.StatusCode != 200 {
		t.Fatalf("settings → %d", resp.StatusCode)
	}
	resp.Body.Close()

	// logout drops the session
	resp, _ = c.PostForm(ts.URL+"/admin/logout", nil)
	resp.Body.Close()
	nr := &http.Client{Jar: c.Jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, _ = nr.Get(ts.URL + "/admin/settings")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("post-logout settings → %d want 303", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAdminCSRF(t *testing.T) {
	_, db, ts := newTestServerCfg(t, nil)
	bootstrapAdmin(t, db)
	c := adminClient(t, ts)

	// cross-origin POST → 403
	req, _ := http.NewRequest("POST", ts.URL+"/admin/caches", strings.NewReader("name=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://evil.example")
	resp, _ := c.Do(req)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin POST → %d want 403", resp.StatusCode)
	}
	resp.Body.Close()

	// same-origin POST → cache created
	req, _ = http.NewRequest("POST", ts.URL+"/admin/caches", strings.NewReader(url.Values{"name": {"csrf-ok"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", ts.URL)
	resp, _ = c.Do(req)
	resp.Body.Close()
	if _, err := db.GetCache("default", "csrf-ok"); err != nil {
		t.Fatalf("same-origin create failed: %v", err)
	}
}

func TestAdminCacheCRUD(t *testing.T) {
	_, db, ts := newTestServerCfg(t, nil)
	bootstrapAdmin(t, db)
	c := adminClient(t, ts)

	// create via form (private, clamped priority)
	resp, _ := c.PostForm(ts.URL+"/admin/caches", url.Values{"name": {"web"}, "priority": {"500"}, "private": {"on"}})
	resp.Body.Close()
	cc, err := db.GetCache("default", "web")
	if err != nil {
		t.Fatal(err)
	}
	if cc.Public || cc.Priority != 100 {
		t.Fatalf("created cache: public=%v priority=%d", cc.Public, cc.Priority)
	}

	// duplicate name → flash + redirect back to the dashboard (PRG), and the
	// landing page carries the error message.
	resp, _ = c.PostForm(ts.URL+"/admin/caches", url.Values{"name": {"web"}})
	if b := body(t, resp); resp.StatusCode != 200 || resp.Request.URL.Path != "/admin" || !contains(b, "Could not create cache") {
		t.Errorf("duplicate cache name → %d at %s", resp.StatusCode, resp.Request.URL.Path)
	}

	// detail page renders (with paths, search, sort, past-the-end page)
	pushFake(t, ts, "web", h32, []byte("some path data"), "")
	for _, q := range []string{"", "?q=pkg&sort=size&dir=asc", "?page[number]=99&page[size]=5", "?q=zzznomatch"} {
		resp, _ := c.Get(ts.URL + "/admin/cache/default/web" + q)
		if resp.StatusCode != 200 {
			t.Errorf("cache detail %q → %d", q, resp.StatusCode)
		}
		resp.Body.Close()
	}
	resp, _ = c.Get(ts.URL + "/admin/cache/default/ghost")
	if resp.StatusCode != 404 {
		t.Errorf("unknown cache detail → %d want 404", resp.StatusCode)
	}
	resp.Body.Close()

	// configure: retention 1 day, cap 1 MiB, public again, priority 7
	resp, _ = c.PostForm(ts.URL+"/admin/cache/default/web/configure", url.Values{
		"priority":        {"7"},
		"retention_value": {"1"}, "retention_unit": {"d"},
		"max_value": {"1"}, "max_unit": {"MiB"},
	})
	resp.Body.Close()
	cc, _ = db.GetCache("default", "web")
	if !cc.Public || cc.Priority != 7 || cc.Retention != 86400 || cc.MaxBytes != 1<<20 {
		t.Fatalf("configured cache: %+v", cc)
	}
	resp, _ = c.PostForm(ts.URL+"/admin/cache/default/ghost/configure", nil)
	if resp.StatusCode != 404 {
		t.Errorf("configure unknown cache → %d want 404", resp.StatusCode)
	}
	resp.Body.Close()

	// rotate key
	oldKey := cc.PubKey
	resp, _ = c.PostForm(ts.URL+"/admin/cache/default/web/rotate", nil)
	if b := body(t, resp); !strings.Contains(b, "rotated") {
		t.Fatalf("rotate response: %q", b)
	}
	cc, _ = db.GetCache("default", "web")
	if cc.PubKey == oldKey {
		t.Fatal("pubkey did not rotate")
	}

	// delete
	resp, _ = c.PostForm(ts.URL+"/admin/cache/default/web/delete", nil)
	resp.Body.Close()
	if _, err := db.GetCache("default", "web"); err == nil {
		t.Fatal("cache still exists after delete")
	}

	// unauthenticated mutation redirects to login
	nr := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, _ = nr.PostForm(ts.URL+"/admin/caches", url.Values{"name": {"anon"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("anon create cache → %d want 303", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAdminTokenCRUD(t *testing.T) {
	_, db, ts := newTestServerCfg(t, nil)
	bootstrapAdmin(t, db)
	// Tokens always belong to the acting user's account (viewing context or
	// personal account) — here the admin's personal "admin" account.
	db.CreateCache("admin", "c", true, 40)
	c := adminClient(t, ts)

	resp, _ := c.PostForm(ts.URL+"/admin/tokens", url.Values{
		"name": {"ci"}, "cache": {"admin/c"}, "push": {"on"}, "pull": {"on"},
		"ttl": {"604800"},
	})
	if b := body(t, resp); !strings.Contains(b, "created") {
		t.Fatalf("token create response: %q", b)
	}
	toks, _ := db.ListTokens()
	if len(toks) != 1 || toks[0].Name != "ci" || toks[0].Expires == 0 {
		t.Fatalf("tokens after create: %+v", toks)
	}
	id := toks[0].ID

	// edit: rename, pull only, permanent
	resp, _ = c.PostForm(ts.URL+"/admin/tokens/"+strconv.FormatInt(id, 10)+"/edit", url.Values{
		"name": {"ci2"}, "cache": {"admin/c"}, "pull": {"on"}, "permanent": {"on"},
	})
	resp.Body.Close()
	tok, _ := db.GetToken(id)
	if tok.Name != "ci2" || tok.Expires != 0 || len(tok.Perms) != 1 || tok.Perms[0] != "pull" {
		t.Fatalf("edited token: %+v", tok)
	}

	// edit unknown id → 404
	resp, _ = c.PostForm(ts.URL+"/admin/tokens/99999/edit", nil)
	if resp.StatusCode != 404 {
		t.Errorf("edit missing token → %d want 404", resp.StatusCode)
	}
	resp.Body.Close()

	// revoke
	resp, _ = c.PostForm(ts.URL+"/admin/tokens/"+strconv.FormatInt(id, 10)+"/revoke", nil)
	resp.Body.Close()
	tok, _ = db.GetToken(id)
	if !tok.Revoked {
		t.Fatal("token not revoked")
	}

	// dashboard with search/sort/pagination params still renders
	resp, _ = c.Get(ts.URL + "/admin?caches[q]=c&tokens[q]=ci&tokens[sort]=expires&tokens[dir]=asc&tokens[number]=1&tokens[size]=5")
	if resp.StatusCode != 200 {
		t.Fatalf("filtered dashboard → %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAdminGC(t *testing.T) {
	_, db, ts := newTestServerCfg(t, func(cfg *config.Config) {
		cfg.GC.Grace = "-1s" // everything unreferenced is instantly sweepable
		cfg.GC.Retention = "1h"
		cfg.Limits.Total = "10GB"
	})
	bootstrapAdmin(t, db)
	cache, _ := db.CreateCache("default", "c", true, 40)
	pushFake(t, ts, "c", h32, []byte("cap-evicted path data"), "")
	// 1-byte cap: the pushed path is over it and gets evicted, its chunk swept.
	// The 2h per-cache retention overrides the 1h global (nothing is that old).
	if err := db.UpdateCache(cache.ID, true, 40, 7200, 1); err != nil {
		t.Fatal(err)
	}
	c := adminClient(t, ts)
	resp, _ := c.PostForm(ts.URL+"/admin/gc", nil)
	if b := body(t, resp); !strings.Contains(b, "GC done") {
		t.Fatalf("gc response: %q", b)
	}
	if resp, _ := http.Get(ts.URL + "/c/default/c/" + h32 + ".narinfo"); resp.StatusCode != 404 {
		t.Fatalf("path survived GC: %d", resp.StatusCode)
	}
	g, _ := db.GlobalStats()
	if g.StoredBytes != 0 {
		t.Fatalf("chunks survived GC: %d bytes", g.StoredBytes)
	}
}

func TestChangePassword(t *testing.T) {
	_, db, ts := newTestServerCfg(t, nil)
	bootstrapAdmin(t, db)
	c := adminClient(t, ts)

	post := func(cur, next, confirm string) string {
		resp, _ := c.PostForm(ts.URL+"/admin/account/password", url.Values{
			"current": {cur}, "new": {next}, "confirm": {confirm},
		})
		return body(t, resp)
	}
	if b := post("wrong", "NewPass123456", "NewPass123456"); !strings.Contains(b, "incorrect") {
		t.Errorf("wrong current: %q", b)
	}
	if b := post(adminPass, "short", "short"); !strings.Contains(b, "at least 8") {
		t.Errorf("short new: %q", b)
	}
	if b := post(adminPass, "NewPass123456", "different123"); !strings.Contains(b, "do not match") {
		t.Errorf("mismatch: %q", b)
	}
	if b := post(adminPass, "NewPass123456", "NewPass123456"); !strings.Contains(b, "Password changed") {
		t.Errorf("good change: %q", b)
	}
	// new password logs in
	jar, _ := cookiejar.New(nil)
	c2 := &http.Client{Jar: jar}
	resp, _ := c2.PostForm(ts.URL+"/admin/login", url.Values{"username": {"admin"}, "password": {"NewPass123456"}})
	if resp.StatusCode != 200 || resp.Request.URL.Path != "/admin" {
		t.Fatalf("login with new password → %d", resp.StatusCode)
	}
	resp.Body.Close()

	// live hint endpoint: anon 401, logged-in 200
	resp, _ = http.PostForm(ts.URL+"/admin/account/password/check", url.Values{"new": {"x"}})
	if resp.StatusCode != 401 {
		t.Errorf("anon password check → %d want 401", resp.StatusCode)
	}
	resp.Body.Close()
	resp, _ = c2.PostForm(ts.URL+"/admin/account/password/check", url.Values{"new": {"NewPass123456"}, "confirm": {""}})
	if resp.StatusCode != 200 {
		t.Errorf("password check → %d", resp.StatusCode)
	}
	resp.Body.Close()
}

var pendingRe = regexp.MustCompile(`name="pending" value="([^"]+)"`)

// wrongCode returns a 6-digit code that is NOT valid for secret around now.
func wrongCode(secret []byte) string {
	valid := map[string]bool{}
	for _, skew := range []int{-1, 0, 1} {
		valid[totpCode(secret, time.Now().Add(time.Duration(skew)*30*time.Second))] = true
	}
	for _, c := range []string{"000000", "111111", "222222", "333333"} {
		if !valid[c] {
			return c
		}
	}
	return "999999"
}

func TestTOTPEnrollAndTwoStepLogin(t *testing.T) {
	_, db, ts := newTestServerCfg(t, nil)
	bootstrapAdmin(t, db)
	c := adminClient(t, ts)

	// enroll: stores a secret and renders the QR
	resp, _ := c.PostForm(ts.URL+"/admin/account/totp/enroll", nil)
	if b := body(t, resp); !strings.Contains(b, "data:image/png") {
		t.Fatalf("enroll page missing QR: %.100q", b)
	}
	secret, enabled, err := db.UserTOTP(adminID(t, db))
	if err != nil || enabled || len(secret) == 0 {
		t.Fatalf("post-enroll totp state: enabled=%v err=%v", enabled, err)
	}

	// enable with a wrong code re-renders the enroll form
	resp, _ = c.PostForm(ts.URL+"/admin/account/totp/enable", url.Values{"code": {wrongCode(secret)}})
	if b := body(t, resp); !strings.Contains(b, "t match") { // "didn't" arrives HTML-escaped
		t.Fatalf("wrong enable code: %.200q", b)
	}
	// correct code enables
	resp, _ = c.PostForm(ts.URL+"/admin/account/totp/enable", url.Values{"code": {totpCode(secret, time.Now())}})
	if b := body(t, resp); !strings.Contains(b, "enabled") {
		t.Fatalf("enable: %.200q", b)
	}

	// fresh login now requires the second step
	jar, _ := cookiejar.New(nil)
	c2 := &http.Client{Jar: jar}
	resp, _ = c2.PostForm(ts.URL+"/admin/login", url.Values{"username": {"admin"}, "password": {adminPass}})
	b := body(t, resp)
	m := pendingRe.FindStringSubmatch(b)
	if m == nil {
		t.Fatalf("no pending ticket in login response: %.300q", b)
	}
	pending := m[1]

	// wrong 2FA code → retry page (ticket stays valid)
	resp, _ = c2.PostForm(ts.URL+"/admin/login/code", url.Values{"pending": {pending}, "code": {wrongCode(secret)}})
	if b := body(t, resp); !strings.Contains(b, "Invalid 2FA code") {
		t.Fatalf("wrong 2fa code: %.200q", b)
	}
	// bogus ticket → back to password step
	resp, _ = c2.PostForm(ts.URL+"/admin/login/code", url.Values{"pending": {"bogus"}, "code": {"123456"}})
	if b := body(t, resp); !strings.Contains(b, "expired") {
		t.Fatalf("bogus pending: %.200q", b)
	}
	// correct code → session granted
	resp, _ = c2.PostForm(ts.URL+"/admin/login/code", url.Values{"pending": {pending}, "code": {totpCode(secret, time.Now())}})
	if resp.StatusCode != 200 || resp.Request.URL.Path != "/admin" {
		t.Fatalf("2fa login → %d at %s", resp.StatusCode, resp.Request.URL)
	}
	resp.Body.Close()
	resp, _ = c2.Get(ts.URL + "/admin/settings")
	if resp.StatusCode != 200 {
		t.Fatalf("post-2fa settings → %d", resp.StatusCode)
	}
	resp.Body.Close()

	// disable
	resp, _ = c2.PostForm(ts.URL+"/admin/account/totp/disable", nil)
	if b := body(t, resp); !strings.Contains(b, "disabled") {
		t.Fatalf("disable: %.200q", b)
	}
	if _, on, _ := db.UserTOTP(adminID(t, db)); on {
		t.Fatal("totp still enabled")
	}
}

func TestLoginCodeAfterTOTPDisabledMidFlight(t *testing.T) {
	_, db, ts := newTestServerCfg(t, nil)
	bootstrapAdmin(t, db)
	c := adminClient(t, ts)
	resp, _ := c.PostForm(ts.URL+"/admin/account/totp/enroll", nil)
	resp.Body.Close()
	secret, _, _ := db.UserTOTP(adminID(t, db))
	resp, _ = c.PostForm(ts.URL+"/admin/account/totp/enable", url.Values{"code": {totpCode(secret, time.Now())}})
	resp.Body.Close()

	jar, _ := cookiejar.New(nil)
	c2 := &http.Client{Jar: jar}
	resp, _ = c2.PostForm(ts.URL+"/admin/login", url.Values{"username": {"admin"}, "password": {adminPass}})
	m := pendingRe.FindStringSubmatch(body(t, resp))
	if m == nil {
		t.Fatal("no pending ticket")
	}
	// admin disables 2FA while the second step is pending
	db.SetUserTOTPEnabled(adminID(t, db), false)
	resp, _ = c2.PostForm(ts.URL+"/admin/login/code", url.Values{"pending": {m[1]}, "code": {"whatever"}})
	if resp.StatusCode != 200 || resp.Request.URL.Path != "/admin" {
		t.Fatalf("mid-flight disabled login → %d at %s", resp.StatusCode, resp.Request.URL)
	}
	resp.Body.Close()
}

func TestPasskeyEndpoints(t *testing.T) {
	// webauthn requires a real-looking RP domain; the default test BaseURL
	// ("http://example", no dot) fails relying-party validation.
	_, db, ts := newTestServerCfg(t, func(cfg *config.Config) { cfg.BaseURL = "http://localhost:8080" })
	bootstrapAdmin(t, db)

	// no passkeys: login begin refuses
	resp, _ := http.Post(ts.URL+"/admin/login/passkey/begin", "", nil)
	if resp.StatusCode != 400 {
		t.Fatalf("login begin sans passkeys → %d want 400", resp.StatusCode)
	}
	resp.Body.Close()

	c := adminClient(t, ts)

	// register begin returns creation options
	resp, _ = c.PostForm(ts.URL+"/admin/passkeys/register/begin", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("register begin → %d", resp.StatusCode)
	}
	var opts map[string]any
	json.NewDecoder(resp.Body).Decode(&opts)
	resp.Body.Close()
	if opts["publicKey"] == nil {
		t.Fatalf("register begin options: %v", opts)
	}

	// finish with garbage body → 400; second finish → ceremony gone → 400
	resp, _ = c.Post(ts.URL+"/admin/passkeys/register/finish", "application/json", strings.NewReader("{}"))
	if resp.StatusCode != 400 {
		t.Fatalf("garbage register finish → %d want 400", resp.StatusCode)
	}
	resp.Body.Close()
	resp, _ = c.Post(ts.URL+"/admin/passkeys/register/finish", "application/json", strings.NewReader("{}"))
	if b := body(t, resp); resp.StatusCode != 400 || !strings.Contains(b, "expired") {
		t.Fatalf("stale register finish → %d %q", resp.StatusCode, b)
	}

	// seed one readable and one unreadable credential row
	blob, _ := json.Marshal(webauthn.Credential{ID: []byte("cred-id")})
	db.AddPasskey(adminID(t, db), "good", blob)
	db.AddPasskey(adminID(t, db), "bad", []byte("not-json"))

	resp, _ = http.Post(ts.URL+"/admin/login/passkey/begin", "", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("login begin with passkey → %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp, _ = http.Post(ts.URL+"/admin/login/passkey/finish", "application/json", strings.NewReader("{}"))
	if resp.StatusCode != 400 {
		t.Fatalf("garbage login finish → %d want 400 (unparsable body)", resp.StatusCode)
	}
	resp.Body.Close()
	resp, _ = http.Post(ts.URL+"/admin/login/passkey/finish", "application/json", strings.NewReader("{}"))
	if resp.StatusCode != 400 {
		t.Fatalf("stale login finish → %d want 400", resp.StatusCode)
	}
	resp.Body.Close()

	// delete
	pks, _ := db.ListPasskeys()
	resp, _ = c.PostForm(ts.URL+"/admin/passkeys/"+strconv.FormatInt(pks[0].ID, 10)+"/delete", nil)
	resp.Body.Close()
	after, _ := db.ListPasskeys()
	if len(after) != len(pks)-1 {
		t.Fatalf("passkey delete: %d → %d", len(pks), len(after))
	}
}

func TestStatusPagesAndData(t *testing.T) {
	s, db, ts := newTestServerCfg(t, nil)
	bootstrapAdmin(t, db)
	db.CreateCache("default", "c", true, 40)
	pushFake(t, ts, "c", h32, []byte("status traffic"), "")
	getNar(t, ts, "/c/default/c/nar/"+h32+".nar", "")
	s.sampleStatus()
	s.sampleStatus()

	c := adminClient(t, ts)
	for _, q := range []string{"", "?window=30&rate=2", "?window=720", "?from=2026-01-01&to=2026-01-02", "?window=junk&rate=junk"} {
		resp, _ := c.Get(ts.URL + "/admin/status" + q)
		if resp.StatusCode != 200 {
			t.Errorf("status %q → %d", q, resp.StatusCode)
		}
		resp.Body.Close()
	}

	// data endpoint: anon 401, session 200 with 4 charts
	resp, _ := http.Get(ts.URL + "/admin/status/data")
	if resp.StatusCode != 401 {
		t.Fatalf("anon status data → %d want 401", resp.StatusCode)
	}
	resp.Body.Close()
	resp, _ = c.Get(ts.URL + "/admin/status/data?window=10")
	var sj struct {
		Healthy bool                       `json:"healthy"`
		Charts  map[string]json.RawMessage `json:"charts"`
	}
	json.NewDecoder(resp.Body).Decode(&sj)
	resp.Body.Close()
	if !sj.Healthy || len(sj.Charts) != 4 {
		t.Fatalf("status json: healthy=%v charts=%d", sj.Healthy, len(sj.Charts))
	}
}
