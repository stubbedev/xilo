package server

import (
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/stubbedev/xilo/internal/config"
	"github.com/stubbedev/xilo/internal/store"
)

// TestRegistrationFlow covers the multi-tenant signup surface: gated routes,
// plan selection, org creation at signup, approval, and create-time quotas.
func TestRegistrationFlow(t *testing.T) {
	_, db, ts := newTestServerCfg(t, func(c *config.Config) { c.MultiTenant = true })
	bootstrapAdmin(t, db)

	// Closed by default.
	resp, _ := http.Get(ts.URL + "/register")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("register while closed → %d want 404", resp.StatusCode)
	}
	if err := db.SetSetting("allow_registrations", "1"); err != nil {
		t.Fatal(err)
	}

	plan, err := db.CreatePlan(&store.Plan{Name: "team", MaxCaches: 1, MaxMembers: 2, OrgsAllowed: true, Public: true})
	if err != nil {
		t.Fatal(err)
	}

	resp, _ = http.Get(ts.URL + "/register")
	if resp.StatusCode != 200 {
		t.Fatalf("register form → %d", resp.StatusCode)
	}

	// Email is required in multi-tenant mode.
	resp, _ = http.PostForm(ts.URL+"/register", url.Values{
		"username": {"noemail"}, "password": {"password12"}, "plan": {strconv.FormatInt(plan.ID, 10)},
	})
	if b := body(t, resp); !contains(b, "valid email address is required") {
		t.Fatalf("missing email should be rejected: %.120q", b)
	}
	if _, err := db.GetUserByName("noemail"); err == nil {
		t.Fatal("user created without required email")
	}

	// Register with org; approval required by default → pending, no session.
	form := url.Values{
		"username": {"carol"}, "email": {"carol@example.com"}, "password": {"carolpass1"},
		"plan": {strconv.FormatInt(plan.ID, 10)}, "org": {"carols-org"},
	}
	resp, _ = http.PostForm(ts.URL+"/register", form)
	if b := body(t, resp); resp.StatusCode != 200 || !contains(b, "approve") {
		t.Fatalf("register: %d %.120q", resp.StatusCode, b)
	}
	u, err := db.GetUserByName("carol")
	if err != nil || u.Status != "pending" {
		t.Fatalf("carol: %+v %v", u, err)
	}
	org, err := db.GetAccount("carols-org")
	if err != nil || org.Kind != "org" || org.PlanID != plan.ID {
		t.Fatalf("org: %+v %v", org, err)
	}
	if db.MemberRole(org.ID, u.ID) != "admin" {
		t.Fatal("carol should administer her org")
	}

	// Pending users cannot sign in.
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	resp, _ = c.PostForm(ts.URL+"/admin/login", url.Values{"username": {"carol"}, "password": {"carolpass1"}})
	if b := body(t, resp); !contains(b, "awaiting approval") {
		t.Fatalf("pending login: %.120q", b)
	}

	// Approve (as admin), then carol signs in.
	ac := adminClient(t, ts)
	resp, _ = ac.PostForm(ts.URL+"/admin/users/"+strconv.FormatInt(u.ID, 10)+"/approve", nil)
	resp.Body.Close()
	resp, _ = c.PostForm(ts.URL+"/admin/login", url.Values{"username": {"carol"}, "password": {"carolpass1"}})
	if resp.StatusCode != 200 || resp.Request.URL.Path != "/admin" {
		t.Fatalf("approved login → %d at %s", resp.StatusCode, resp.Request.URL)
	}

	// Plan quota: max 1 cache in the org.
	resp, _ = c.PostForm(ts.URL+"/admin/caches", url.Values{"namespace": {"carols-org"}, "name": {"one"}})
	if resp.StatusCode != 200 && resp.StatusCode != 303 {
		t.Fatalf("first cache → %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp, _ = c.PostForm(ts.URL+"/admin/caches", url.Values{"namespace": {"carols-org"}, "name": {"two"}})
	if b := body(t, resp); resp.StatusCode != 200 || resp.Request.URL.Path != "/admin" || !contains(b, "at most 1 caches") {
		t.Fatalf("quota: %d at %s %.120q", resp.StatusCode, resp.Request.URL.Path, b)
	}

	// Member quota: plan allows 2 members (carol + one more). Members are
	// added by user id (picker), not free text.
	dave, err := db.CreateUser("dave", "", "h", "user")
	if err != nil {
		t.Fatal(err)
	}
	erin, err := db.CreateUser("erin", "", "h", "user")
	if err != nil {
		t.Fatal(err)
	}
	daveID := strconv.FormatInt(dave.ID, 10)
	erinID := strconv.FormatInt(erin.ID, 10)
	resp, _ = ac.PostForm(ts.URL+"/admin/org/carols-org/members", url.Values{"user_id": {daveID}, "role": {"user"}})
	resp.Body.Close()
	resp, _ = ac.PostForm(ts.URL+"/admin/org/carols-org/members", url.Values{"user_id": {erinID}, "role": {"user"}})
	if b := body(t, resp); !contains(b, "at most 2 members") {
		t.Fatalf("member quota: %.200q", b)
	}

	// Duplicate slug across users and orgs is refused.
	resp, _ = http.PostForm(ts.URL+"/register", url.Values{
		"username": {"carols-org"}, "email": {"z@example.com"}, "password": {"whatever123"}, "plan": {strconv.FormatInt(plan.ID, 10)},
	})
	if b := body(t, resp); !contains(b, "taken") {
		t.Fatalf("slug collision: %.120q", b)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// TestOrgManagementAuthz pins the corrected member flows: personal accounts
// reject members, org admins manage their own org without instance rights,
// and outsiders get 404.
func TestOrgManagementAuthz(t *testing.T) {
	s, db, ts := newTestServerCfg(t, nil)
	bootstrapAdmin(t, db)
	_ = s

	// alice: org admin of "acme"; bob: unrelated user; carol: to be added.
	alice, _ := db.CreateUser("alice", "", passHash(t, "alicepass1"), "user")
	db.CreateUser("bob", "", passHash(t, "bobpass1234"), "user")
	carol, _ := db.CreateUser("carol", "", passHash(t, "carolpass1"), "user")
	acme, _ := db.EnsureAccount("acme", "org")
	db.SetMember(acme.ID, alice.ID, "admin")

	al := loginAs(t, ts, "alice", "alicepass1")

	// Org admin adds carol by id — no instance-admin rights needed.
	resp, _ := al.PostForm(ts.URL+"/admin/org/acme/members",
		url.Values{"user_id": {itoa(carol.ID)}, "role": {"user"}})
	resp.Body.Close()
	if db.MemberRole(acme.ID, carol.ID) != "user" {
		t.Fatal("org admin could not add a member")
	}

	// bob (not a member) cannot even see the org page → 404.
	bob, _ := db.GetUserByName("bob")
	bo := loginAs(t, ts, "bob", "bobpass1234")
	bo.userID = bob.ID
	resp, _ = bo.Get(ts.URL + "/admin/org/acme")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("outsider org page → %d want 404", resp.StatusCode)
	}
	resp.Body.Close()
	// …and cannot add themselves.
	resp, _ = bo.PostForm(ts.URL+"/admin/org/acme/members",
		url.Values{"user_id": {itoa(bo.userID)}, "role": {"admin"}})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("outsider member add → %d want 404", resp.StatusCode)
	}
	resp.Body.Close()

	// Personal accounts refuse members at the store layer.
	if err := db.SetMember(mustAccount(t, db, "alice").ID, carol.ID, "user"); err == nil {
		t.Fatal("personal account accepted an extra member")
	}
}

// userClient is a logged-in cookie-jar client that remembers its user id.
type userClient struct {
	*http.Client
	userID int64
}

func loginAs(t *testing.T, ts *httptest.Server, user, pass string) *userClient {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	resp, err := c.PostForm(ts.URL+"/admin/login", url.Values{"username": {user}, "password": {pass}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Request.URL.Path != "/admin" {
		t.Fatalf("login %s landed at %s", user, resp.Request.URL.Path)
	}
	return &userClient{Client: c}
}

func passHash(t *testing.T, pw string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	return string(h)
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func mustAccount(t *testing.T, db *store.DB, slug string) *store.Account {
	t.Helper()
	a, err := db.GetAccount(slug)
	if err != nil {
		t.Fatalf("account %s: %v", slug, err)
	}
	return a
}
