package server

import (
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"testing"

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
	if b := body(t, resp); resp.StatusCode != http.StatusForbidden || !contains(b, "at most 1 caches") {
		t.Fatalf("quota: %d %.120q", resp.StatusCode, b)
	}

	// Member quota: plan allows 2 members (carol + one more).
	resp, _ = ac.PostForm(ts.URL+"/admin/users", url.Values{"username": {"dave"}, "password": {"davepass123"}})
	resp.Body.Close()
	resp, _ = ac.PostForm(ts.URL+"/admin/orgs/carols-org/members", url.Values{"username": {"dave"}, "role": {"member"}})
	resp.Body.Close()
	resp, _ = ac.PostForm(ts.URL+"/admin/users", url.Values{"username": {"erin"}, "password": {"erinpass123"}})
	resp.Body.Close()
	resp, _ = ac.PostForm(ts.URL+"/admin/orgs/carols-org/members", url.Values{"username": {"erin"}, "role": {"member"}})
	if b := body(t, resp); !contains(b, "at most 2 members") {
		t.Fatalf("member quota: %.200q", b)
	}

	// Duplicate slug across users and orgs is refused.
	resp, _ = http.PostForm(ts.URL+"/register", url.Values{
		"username": {"carols-org"}, "password": {"whatever123"}, "plan": {strconv.FormatInt(plan.ID, 10)},
	})
	if b := body(t, resp); !contains(b, "taken") {
		t.Fatalf("slug collision: %.120q", b)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
