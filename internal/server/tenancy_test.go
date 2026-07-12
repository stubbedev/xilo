package server

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stubbedev/xilo/internal/api"
	"github.com/stubbedev/xilo/internal/config"
	"github.com/stubbedev/xilo/internal/store"
)

// mtServer is newTestServerCfg with multi-tenant mode on.
func mtServer(t *testing.T) (*Server, *store.DB, *httptest.Server) {
	t.Helper()
	return newTestServerCfg(t, func(c *config.Config) { c.MultiTenant = true })
}

// postFlash POSTs a form and returns the landing path + body after the PRG
// redirect (303 + one-shot flash cookie, popped by the GET renderer).
func postFlash(t *testing.T, c *http.Client, u string, form url.Values) (path, b string) {
	t.Helper()
	resp, err := c.PostForm(u, form)
	if err != nil {
		t.Fatal(err)
	}
	return resp.Request.URL.Path, body(t, resp)
}

func TestAdminUserLifecycle(t *testing.T) {
	_, db, ts := newTestServerCfg(t, nil)
	bootstrapAdmin(t, db)
	c := adminClient(t, ts)

	// missing username → PRG back to settings with the error flash
	path, b := postFlash(t, c, ts.URL+"/admin/users", url.Values{"password": {"longenough1"}})
	if path != "/admin/settings" || !contains(b, "Username is required") {
		t.Errorf("no username → %s %.120q", path, b)
	}
	// short password
	_, b = postFlash(t, c, ts.URL+"/admin/users", url.Values{"username": {"walter"}, "password": {"short"}})
	if !contains(b, "at least 8") {
		t.Errorf("short password: %.120q", b)
	}
	// happy path
	path, b = postFlash(t, c, ts.URL+"/admin/users", url.Values{"username": {"walter"}, "password": {"walterpass1"}})
	if path != "/admin/settings" || !contains(b, "created") {
		t.Errorf("create user → %s %.120q", path, b)
	}
	walter, err := db.GetUserByName("walter")
	if err != nil || walter.Role != "user" {
		t.Fatalf("walter: %+v %v", walter, err)
	}
	// duplicate name
	_, b = postFlash(t, c, ts.URL+"/admin/users", url.Values{"username": {"walter"}, "password": {"walterpass1"}})
	if !contains(b, "Could not create user") {
		t.Errorf("duplicate user: %.120q", b)
	}

	// reset: short password refused, good one takes effect
	_, b = postFlash(t, c, ts.URL+"/admin/users/"+itoa(walter.ID)+"/reset", url.Values{"password": {"tiny"}})
	if !contains(b, "at least 8") {
		t.Errorf("short reset: %.120q", b)
	}
	path, b = postFlash(t, c, ts.URL+"/admin/users/"+itoa(walter.ID)+"/reset", url.Values{"password": {"resetpass99"}})
	if path != "/admin/settings" || !contains(b, "Password reset") {
		t.Errorf("reset → %s %.120q", path, b)
	}
	loginAs(t, ts, "walter", "resetpass99") // fails the test if the login bounces
	// unknown id → 404
	resp, _ := c.PostForm(ts.URL+"/admin/users/99999/reset", url.Values{"password": {"resetpass99"}})
	if resp.StatusCode != 404 {
		t.Errorf("reset missing user → %d want 404", resp.StatusCode)
	}
	resp.Body.Close()

	// delete guards: self, owners, org owners
	_, b = postFlash(t, c, ts.URL+"/admin/users/"+itoa(adminID(t, db))+"/delete", nil)
	if !contains(b, "cannot delete your own account") {
		t.Errorf("self delete: %.120q", b)
	}
	boss2, _ := db.CreateUser("boss2", "", passHash(t, "bosspass123"), "owner")
	_, b = postFlash(t, c, ts.URL+"/admin/users/"+itoa(boss2.ID)+"/delete", nil)
	if !contains(b, "owner cannot be deleted") {
		t.Errorf("owner delete: %.120q", b)
	}
	founder, _ := db.CreateUser("founder", "", passHash(t, "founderpass1"), "user")
	forg, _ := db.EnsureAccount("f-org", "org")
	db.MakeOwner(forg.ID, founder.ID)
	_, b = postFlash(t, c, ts.URL+"/admin/users/"+itoa(founder.ID)+"/delete", nil)
	if !contains(b, "own organizations") {
		t.Errorf("org-owner delete: %.120q", b)
	}
	// happy delete
	path, b = postFlash(t, c, ts.URL+"/admin/users/"+itoa(walter.ID)+"/delete", nil)
	if path != "/admin/settings" || !contains(b, "deleted") {
		t.Errorf("delete → %s %.120q", path, b)
	}
	if _, err := db.GetUserByName("walter"); err == nil {
		t.Error("walter still exists after delete")
	}
	resp, _ = c.PostForm(ts.URL+"/admin/users/99999/delete", nil)
	if resp.StatusCode != 404 {
		t.Errorf("delete missing user → %d want 404", resp.StatusCode)
	}
	resp.Body.Close()

	// non-owner is refused everywhere
	fo := loginAs(t, ts, "founder", "founderpass1")
	for _, p := range []string{
		"/admin/users", "/admin/users/" + itoa(boss2.ID) + "/reset",
		"/admin/users/" + itoa(boss2.ID) + "/delete", "/admin/orgs", "/admin/gc",
	} {
		resp, _ := fo.PostForm(ts.URL+p, url.Values{"username": {"x"}, "password": {"longenough1"}, "name": {"x"}})
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("non-owner POST %s → %d want 403", p, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func TestAdminUserCreateRequiresEmailMT(t *testing.T) {
	_, db, ts := mtServer(t)
	bootstrapAdmin(t, db)
	c := adminClient(t, ts)

	_, b := postFlash(t, c, ts.URL+"/admin/users", url.Values{"username": {"noemail"}, "password": {"longenough1"}})
	if !contains(b, "valid email address is required") {
		t.Errorf("MT create without email: %.120q", b)
	}
	_, b = postFlash(t, c, ts.URL+"/admin/users", url.Values{
		"username": {"mia"}, "email": {"mia@example.com"}, "password": {"longenough1"},
	})
	if !contains(b, "created") {
		t.Errorf("MT create with email: %.120q", b)
	}
	if u, err := db.GetUserByName("mia"); err != nil || u.Email != "mia@example.com" {
		t.Errorf("mia: %+v %v", u, err)
	}
}

func TestAdminOrgAndMembers(t *testing.T) {
	_, db, ts := newTestServerCfg(t, nil)
	bootstrapAdmin(t, db)
	c := adminClient(t, ts)

	// invalid name
	_, b := postFlash(t, c, ts.URL+"/admin/orgs", url.Values{"name": {"bad name"}})
	if !contains(b, "Invalid organization name") {
		t.Errorf("bad org name: %.120q", b)
	}
	// create → creator becomes owner
	path, b := postFlash(t, c, ts.URL+"/admin/orgs", url.Values{"name": {"acme"}})
	if path != "/admin/settings" || !contains(b, "ready") {
		t.Errorf("create org → %s %.120q", path, b)
	}
	acme := mustAccount(t, db, "acme")
	if acme.Kind != "org" || db.MemberRole(acme.ID, adminID(t, db)) != "owner" {
		t.Fatalf("acme: %+v role=%q", acme, db.MemberRole(acme.ID, adminID(t, db)))
	}
	// duplicate slug is idempotent (EnsureAccount) — no second account
	postFlash(t, c, ts.URL+"/admin/orgs", url.Values{"name": {"acme"}})
	if a2 := mustAccount(t, db, "acme"); a2.ID != acme.ID {
		t.Errorf("duplicate create replaced the org: %d vs %d", a2.ID, acme.ID)
	}

	// members: add with role, edit role, unknown user, remove, owner guard
	carl, _ := db.CreateUser("carl", "", passHash(t, "carlpass123"), "user")
	path, b = postFlash(t, c, ts.URL+"/admin/org/acme/members", url.Values{"user_id": {itoa(carl.ID)}, "role": {"admin"}})
	if path != "/admin/org/acme" || !contains(b, "is now a") || db.MemberRole(acme.ID, carl.ID) != "admin" {
		t.Errorf("add member → %s role=%q %.120q", path, db.MemberRole(acme.ID, carl.ID), b)
	}
	postFlash(t, c, ts.URL+"/admin/org/acme/members", url.Values{"user_id": {itoa(carl.ID)}, "role": {"user"}})
	if db.MemberRole(acme.ID, carl.ID) != "user" {
		t.Errorf("edit role: %q", db.MemberRole(acme.ID, carl.ID))
	}
	_, b = postFlash(t, c, ts.URL+"/admin/org/acme/members", url.Values{"user_id": {"99999"}, "role": {"user"}})
	if !contains(b, "No such user") {
		t.Errorf("unknown member: %.120q", b)
	}
	_, b = postFlash(t, c, ts.URL+"/admin/org/acme/members/"+itoa(adminID(t, db))+"/remove", nil)
	if !contains(b, "owner cannot be removed") || db.MemberRole(acme.ID, adminID(t, db)) != "owner" {
		t.Errorf("remove owner: %.120q", b)
	}
	path, b = postFlash(t, c, ts.URL+"/admin/org/acme/members/"+itoa(carl.ID)+"/remove", nil)
	if path != "/admin/org/acme" || !contains(b, "Member removed") || db.MemberRole(acme.ID, carl.ID) != "" {
		t.Errorf("remove member → %s %.120q", path, b)
	}

	// org admins may manage but not delete
	dana, _ := db.CreateUser("dana", "", passHash(t, "danapass123"), "user")
	db.SetMember(acme.ID, dana.ID, "admin")
	da := loginAs(t, ts, "dana", "danapass123")
	path, b = postFlash(t, da.Client, ts.URL+"/admin/org/acme/delete", nil)
	if path != "/admin/org/acme" || !contains(b, "Only the owner") {
		t.Errorf("admin delete org → %s %.120q", path, b)
	}
	// instance admin deletes → settings flash, org gone
	path, b = postFlash(t, c, ts.URL+"/admin/org/acme/delete", nil)
	if path != "/admin/settings" || !contains(b, "deleted") {
		t.Errorf("owner delete org → %s %.120q", path, b)
	}
	if _, err := db.GetAccount("acme"); err == nil {
		t.Error("acme survived delete")
	}

	// a non-instance org owner deletes their own org and lands on the dashboard
	zed, _ := db.CreateUser("zed", "", passHash(t, "zedpass1234"), "user")
	zorg, _ := db.EnsureAccount("zorg", "org")
	db.MakeOwner(zorg.ID, zed.ID)
	ze := loginAs(t, ts, "zed", "zedpass1234")
	path, b = postFlash(t, ze.Client, ts.URL+"/admin/org/zorg/delete", nil)
	if path != "/admin" || !contains(b, "deleted") {
		t.Errorf("org owner delete → %s %.120q", path, b)
	}
	if _, err := db.GetAccount("zorg"); err == nil {
		t.Error("zorg survived delete")
	}
}

func TestRefererPath(t *testing.T) {
	cases := []struct{ ref, want string }{
		{"", "/admin"},
		{"http://other.example/admin/settings", "/admin"}, // cross-host
		{"http://example/somewhere", "/admin"},            // same host, not /admin
		{"http://example/admin/settings", "/admin/settings"},
		{"http://example/admin/org/acme", "/admin/org/acme"},
	}
	for _, tc := range cases {
		r := httptest.NewRequest("POST", "http://example/admin/context", nil)
		if tc.ref != "" {
			r.Header.Set("Referer", tc.ref)
		}
		if got := refererPath(r); got != tc.want {
			t.Errorf("refererPath(%q) = %q want %q", tc.ref, got, tc.want)
		}
	}
}

func TestContextSwitcher(t *testing.T) {
	_, db, ts := newTestServerCfg(t, nil)
	bootstrapAdmin(t, db)
	acme, _ := db.EnsureAccount("acme", "org")
	db.CreateUser("uma", "", passHash(t, "umapass1234"), "user")

	// anonymous → bounced to login
	nr := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, _ := nr.PostForm(ts.URL+"/admin/context", url.Values{"ctx": {"acme"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("anon context → %d want 303", resp.StatusCode)
	}
	resp.Body.Close()

	ctxCookieOf := func(resp *http.Response) (string, bool) {
		for _, ck := range resp.Cookies() {
			if ck.Name == ctxCookie {
				return ck.Value, true
			}
		}
		return "", false
	}
	post := func(c *http.Client, val, referer string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest("POST", ts.URL+"/admin/context", strings.NewReader(url.Values{"ctx": {val}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if referer != "" {
			req.Header.Set("Referer", referer)
		}
		stop := &http.Client{Jar: c.Jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := stop.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}

	// instance admin can pick any account; redirect returns to the referring page
	ac := adminClient(t, ts)
	resp = post(ac, "acme", ts.URL+"/admin/settings")
	if v, ok := ctxCookieOf(resp); !ok || v != "acme" {
		t.Errorf("admin ctx cookie = %q ok=%v", v, ok)
	}
	if loc := resp.Header.Get("Location"); resp.StatusCode != 303 || loc != "/admin/settings" {
		t.Errorf("context redirect → %d %q", resp.StatusCode, loc)
	}
	// dashboard renders with the active context (activeContext resolves it)
	resp2, _ := ac.Get(ts.URL + "/admin")
	if resp2.StatusCode != 200 {
		t.Errorf("dashboard with context → %d", resp2.StatusCode)
	}
	resp2.Body.Close()

	// non-member picking an org they don't belong to → cleared
	um := loginAs(t, ts, "uma", "umapass1234")
	resp = post(um.Client, "acme", "")
	if v, ok := ctxCookieOf(resp); !ok || v != "" {
		t.Errorf("non-member ctx cookie = %q ok=%v (want cleared)", v, ok)
	}
	// member picking their org → kept
	uma, _ := db.GetUserByName("uma")
	db.SetMember(acme.ID, uma.ID, "user")
	resp = post(um.Client, "acme", "")
	if v, _ := ctxCookieOf(resp); v != "acme" {
		t.Errorf("member ctx cookie = %q want acme", v)
	}
	// unknown account → cleared
	resp = post(ac, "ghost", "")
	if v, _ := ctxCookieOf(resp); v != "" {
		t.Errorf("ghost ctx cookie = %q want cleared", v)
	}
}

func TestAccountEmail(t *testing.T) {
	_, db, ts := newTestServerCfg(t, nil)
	bootstrapAdmin(t, db)
	db.CreateUser("eve", "eve@example.com", passHash(t, "evepass1234"), "user")
	c := adminClient(t, ts)

	path, b := postFlash(t, c, ts.URL+"/admin/account/email", url.Values{"email": {"admin@example.com"}})
	if path != "/admin/account" || !contains(b, "Email saved") {
		t.Errorf("set email → %s %.120q", path, b)
	}
	u, _ := db.GetUserByName("admin")
	if u.Email != "admin@example.com" {
		t.Errorf("email = %q", u.Email)
	}
	// duplicate email (unique index) surfaces as a flash, not a 500
	_, b = postFlash(t, c, ts.URL+"/admin/account/email", url.Values{"email": {"eve@example.com"}})
	if !contains(b, "Could not save email") {
		t.Errorf("dup email: %.120q", b)
	}
	// clearing is allowed in single-tenant mode
	_, b = postFlash(t, c, ts.URL+"/admin/account/email", url.Values{"email": {""}})
	if !contains(b, "Email cleared") {
		t.Errorf("clear email: %.120q", b)
	}
	if u, _ = db.GetUserByName("admin"); u.Email != "" {
		t.Errorf("email not cleared: %q", u.Email)
	}
}

func TestAccountEmailRequiredMT(t *testing.T) {
	_, db, ts := mtServer(t)
	bootstrapAdmin(t, db)
	c := adminClient(t, ts)
	for _, bad := range []string{"", "not-an-email"} {
		_, b := postFlash(t, c, ts.URL+"/admin/account/email", url.Values{"email": {bad}})
		if !contains(b, "valid email address is required") {
			t.Errorf("MT email %q: %.120q", bad, b)
		}
	}
}

func TestInstanceSettingsToggle(t *testing.T) {
	_, db, ts := mtServer(t)
	bootstrapAdmin(t, db)
	db.CreateUser("norm", "", passHash(t, "normpass1234"), "user")

	// non-owner refused
	no := loginAs(t, ts, "norm", "normpass1234")
	resp, _ := no.PostForm(ts.URL+"/admin/settings/instance", url.Values{"allow_registrations": {"on"}})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-owner instance settings → %d want 403", resp.StatusCode)
	}
	resp.Body.Close()

	c := adminClient(t, ts)
	path, b := postFlash(t, c, ts.URL+"/admin/settings/instance", url.Values{"allow_registrations": {"on"}})
	if path != "/admin/settings" || !contains(b, "Instance settings saved") {
		t.Fatalf("instance settings → %s %.120q", path, b)
	}
	if !db.SettingBool("allow_registrations", false) || db.SettingBool("require_approval", true) {
		t.Errorf("settings: allow=%v require=%v", db.SettingBool("allow_registrations", false), db.SettingBool("require_approval", true))
	}
	// unchecked boxes turn both off
	postFlash(t, c, ts.URL+"/admin/settings/instance", nil)
	if db.SettingBool("allow_registrations", false) {
		t.Error("allow_registrations still on")
	}
}

func TestTenancyRoutesAbsentSingleTenant(t *testing.T) {
	s, db, ts := newTestServerCfg(t, nil)
	bootstrapAdmin(t, db)
	c := adminClient(t, ts)
	// registerTenancy is skipped in single-tenant mode, so no POST handler is
	// registered for these paths. The `GET /` catch-all still path-matches
	// every route, so the mux answers 405 (method not allowed), never 404 —
	// the point is only that no real handler runs.
	for _, p := range []string{"/admin/settings/instance", "/admin/plans", "/admin/plans/1/edit", "/admin/plans/1/delete", "/admin/neworg"} {
		resp, _ := c.PostForm(ts.URL+p, url.Values{"name": {"x"}})
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("single-tenant POST %s → %d want 405", p, resp.StatusCode)
		}
		resp.Body.Close()
	}
	resp, _ := http.Get(ts.URL + "/register")
	if resp.StatusCode != 404 {
		t.Errorf("single-tenant /register → %d want 404", resp.StatusCode)
	}
	resp.Body.Close()

	// userCanCreateOrg: owner yes, plain user no (single-tenant), nil no
	admin, _ := db.GetUserByName("admin")
	norm, _ := db.CreateUser("norm", "", "h", "user")
	if !s.userCanCreateOrg(admin) || s.userCanCreateOrg(norm) || s.userCanCreateOrg(nil) {
		t.Errorf("userCanCreateOrg single-tenant: owner=%v user=%v nil=%v",
			s.userCanCreateOrg(admin), s.userCanCreateOrg(norm), s.userCanCreateOrg(nil))
	}
}

func TestPlanCRUD(t *testing.T) {
	_, db, ts := mtServer(t)
	bootstrapAdmin(t, db)
	db.CreateUser("norm", "", passHash(t, "normpass1234"), "user")
	no := loginAs(t, ts, "norm", "normpass1234")
	resp, _ := no.PostForm(ts.URL+"/admin/plans", url.Values{"name": {"sneaky"}})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-owner create plan → %d want 403", resp.StatusCode)
	}
	resp.Body.Close()

	c := adminClient(t, ts)
	_, b := postFlash(t, c, ts.URL+"/admin/plans", nil)
	if !contains(b, "Plan name is required") {
		t.Errorf("nameless plan: %.120q", b)
	}
	// full form: negative limits clamp to 0, storage/retention units parse
	path, b := postFlash(t, c, ts.URL+"/admin/plans", url.Values{
		"name": {"pro"}, "max_caches": {"5"}, "max_members": {"-3"},
		"plan_storage_value": {"2"}, "plan_storage_unit": {"MiB"},
		"plan_retention_value": {"1"}, "plan_retention_unit": {"d"},
		"orgs_allowed": {"on"}, "public": {"on"},
	})
	if path != "/admin/settings" || !contains(b, "created") {
		t.Fatalf("create plan → %s %.120q", path, b)
	}
	plans, _ := db.ListPlans()
	if len(plans) != 1 {
		t.Fatalf("plans: %+v", plans)
	}
	p := plans[0]
	if p.Name != "pro" || p.MaxCaches != 5 || p.MaxMembers != 0 ||
		p.MaxStorage != 2<<20 || p.MaxRetention != 86400 || !p.OrgsAllowed || !p.Public {
		t.Fatalf("created plan: %+v", p)
	}

	// edit: blank name keeps the old, checkboxes off turn features off
	_, b = postFlash(t, c, ts.URL+"/admin/plans/"+itoa(p.ID)+"/edit", url.Values{
		"name": {""}, "max_caches": {"7"},
	})
	if !contains(b, "updated") {
		t.Errorf("edit plan: %.120q", b)
	}
	got, _ := db.GetPlan(p.ID)
	if got.Name != "pro" || got.MaxCaches != 7 || got.OrgsAllowed || got.Public {
		t.Fatalf("edited plan: %+v", got)
	}
	resp, _ = c.PostForm(ts.URL+"/admin/plans/99999/edit", url.Values{"name": {"x"}})
	if resp.StatusCode != 404 {
		t.Errorf("edit missing plan → %d want 404", resp.StatusCode)
	}
	resp.Body.Close()

	// delete refused while an account uses the plan
	norm := mustAccount(t, db, "norm")
	db.SetAccountPlan(norm.ID, p.ID)
	_, b = postFlash(t, c, ts.URL+"/admin/plans/"+itoa(p.ID)+"/delete", nil)
	if !contains(b, "in use") {
		t.Errorf("delete plan in use: %.120q", b)
	}
	if _, err := db.GetPlan(p.ID); err != nil {
		t.Error("plan in use was deleted")
	}
	db.SetAccountPlan(norm.ID, 0)
	_, b = postFlash(t, c, ts.URL+"/admin/plans/"+itoa(p.ID)+"/delete", nil)
	if !contains(b, "Plan deleted") {
		t.Errorf("delete plan: %.120q", b)
	}
	if _, err := db.GetPlan(p.ID); err == nil {
		t.Error("plan survived delete")
	}
}

func TestUserCreateOrg(t *testing.T) {
	s, db, ts := mtServer(t)
	bootstrapAdmin(t, db)
	noOrgs, _ := db.CreatePlan(&store.Plan{Name: "solo", OrgsAllowed: false})
	orgsOK, _ := db.CreatePlan(&store.Plan{Name: "team", OrgsAllowed: true})

	mkUser := func(name string, planID int64) *userClient {
		t.Helper()
		if _, err := db.CreateUser(name, "", passHash(t, name+"password"), "user"); err != nil {
			t.Fatal(err)
		}
		if planID != 0 {
			db.SetAccountPlan(mustAccount(t, db, name).ID, planID)
		}
		return loginAs(t, ts, name, name+"password")
	}

	// plan without orgs → 403
	pat := mkUser("pat", noOrgs.ID)
	resp, _ := pat.PostForm(ts.URL+"/admin/neworg", url.Values{"name": {"patorg"}})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("orgless plan create org → %d want 403", resp.StatusCode)
	}
	resp.Body.Close()

	// plan with orgs: bad and taken names flash back to the account page
	quinn := mkUser("quinn", orgsOK.ID)
	_, b := postFlash(t, quinn.Client, ts.URL+"/admin/neworg", url.Values{"name": {"Bad Name"}})
	if !contains(b, "Invalid organization name") {
		t.Errorf("bad org name: %.120q", b)
	}
	_, b = postFlash(t, quinn.Client, ts.URL+"/admin/neworg", url.Values{"name": {"quinn"}})
	if !contains(b, "taken") {
		t.Errorf("taken org name: %.120q", b)
	}
	// happy path: org inherits the plan, creator becomes owner
	path, b := postFlash(t, quinn.Client, ts.URL+"/admin/neworg", url.Values{"name": {"qorg"}})
	if path != "/admin/account" || !contains(b, "created") {
		t.Fatalf("create org → %s %.120q", path, b)
	}
	qorg := mustAccount(t, db, "qorg")
	quinnU, _ := db.GetUserByName("quinn")
	if qorg.Kind != "org" || qorg.PlanID != orgsOK.ID || db.MemberRole(qorg.ID, quinnU.ID) != "owner" {
		t.Fatalf("qorg: %+v role=%q", qorg, db.MemberRole(qorg.ID, quinnU.ID))
	}

	// no plan = unlimited → allowed, org keeps no plan
	ray := mkUser("ray", 0)
	postFlash(t, ray.Client, ts.URL+"/admin/neworg", url.Values{"name": {"rayorg"}})
	rorg := mustAccount(t, db, "rayorg")
	if rorg.PlanID != 0 {
		t.Errorf("planless org inherited plan %d", rorg.PlanID)
	}

	// instance owner always allowed, regardless of plans
	admin, _ := db.GetUserByName("admin")
	if !s.userCanCreateOrg(admin) {
		t.Error("owner should always create orgs")
	}
	ac := adminClient(t, ts)
	postFlash(t, ac, ts.URL+"/admin/neworg", url.Values{"name": {"adminorg"}})
	if a := mustAccount(t, db, "adminorg"); a.Kind != "org" {
		t.Errorf("adminorg: %+v", a)
	}
}

func TestAPINamespaces(t *testing.T) {
	_, db, ts := newTestServer(t, false)
	adminTok, _, err := db.CreateToken(0, "boss", nil, []string{"admin"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	pushTok, _, _ := db.CreateToken(0, "pleb", []string{"default/c"}, []string{"push"}, 0)

	// non-admin tokens are refused on every verb
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/namespaces"},
		{http.MethodPost, "/api/v1/namespaces"},
		{http.MethodDelete, "/api/v1/accounts/default"},
	} {
		resp, _ := apiReq(t, ts, tc.method, tc.path, pushTok, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("push token %s %s → %d want 401", tc.method, tc.path, resp.StatusCode)
		}
	}

	// list includes the built-in default namespace
	resp, b := apiReq(t, ts, http.MethodGet, "/api/v1/namespaces", adminTok, nil)
	var nss []api.AccountResp
	jsonUnmarshal(t, b, &nss)
	if resp.StatusCode != 200 || len(nss) == 0 || nss[0].Name != "default" {
		t.Fatalf("list namespaces: %d %s", resp.StatusCode, b)
	}

	// create: invalid names 400, valid 201, duplicate is idempotent
	for _, bad := range []string{"", "has space", "has/slash"} {
		resp, _ := apiReq(t, ts, http.MethodPost, "/api/v1/namespaces", adminTok, api.CreateAccountReq{Name: bad})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("create namespace %q → %d want 400", bad, resp.StatusCode)
		}
	}
	resp, b = apiReq(t, ts, http.MethodPost, "/api/v1/namespaces", adminTok, api.CreateAccountReq{Name: "team"})
	var ns api.AccountResp
	jsonUnmarshal(t, b, &ns)
	if resp.StatusCode != http.StatusCreated || ns.Name != "team" {
		t.Fatalf("create namespace: %d %s", resp.StatusCode, b)
	}
	resp, _ = apiReq(t, ts, http.MethodPost, "/api/v1/namespaces", adminTok, api.CreateAccountReq{Name: "team"})
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("duplicate namespace → %d want 201 (idempotent)", resp.StatusCode)
	}

	// tokens scoped to a namespace: unknown account 400, known 201
	resp, _ = apiReq(t, ts, http.MethodPost, "/api/v1/tokens", adminTok,
		api.CreateTokenReq{Name: "t", Account: "ghost", Perms: []string{"pull"}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("token for unknown namespace → %d want 400", resp.StatusCode)
	}
	// A pull token must name exactly one cache (bare name within its account).
	resp, _ = apiReq(t, ts, http.MethodPost, "/api/v1/tokens", adminTok,
		api.CreateTokenReq{Name: "t", Account: "team", Caches: []string{"team-cache"}, Perms: []string{"pull"}})
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("token for namespace → %d want 201", resp.StatusCode)
	}
	// ...and one without a cache scope is rejected.
	resp, _ = apiReq(t, ts, http.MethodPost, "/api/v1/tokens", adminTok,
		api.CreateTokenReq{Name: "t2", Account: "team", Perms: []string{"pull"}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("pull token without cache scope → %d want 400", resp.StatusCode)
	}
	resp, _ = apiReq(t, ts, http.MethodPost, "/api/v1/tokens/abc/revoke", adminTok, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("revoke non-numeric id → %d want 400", resp.StatusCode)
	}

	// delete: org 204, personal 409, missing 404
	resp, _ = apiReq(t, ts, http.MethodDelete, "/api/v1/accounts/team", adminTok, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete namespace → %d want 204", resp.StatusCode)
	}
	if _, err := db.GetAccount("team"); err == nil {
		t.Error("team survived delete")
	}
	if _, err := db.CreateUser("pers", "", "h", "user"); err != nil {
		t.Fatal(err)
	}
	resp, _ = apiReq(t, ts, http.MethodDelete, "/api/v1/accounts/pers", adminTok, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("delete personal account → %d want 409", resp.StatusCode)
	}
	resp, _ = apiReq(t, ts, http.MethodDelete, "/api/v1/accounts/ghost", adminTok, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("delete missing namespace → %d want 404", resp.StatusCode)
	}
}

func jsonUnmarshal(t *testing.T, b []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("unmarshal %q: %v", b, err)
	}
}

func TestRunListenErrorAndGracefulShutdown(t *testing.T) {
	// Run() surfaces a listen failure (port already taken).
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	s, _, _ := newTestServerCfg(t, func(cfg *config.Config) { cfg.Listen = busy.Addr().String() })
	if err := s.Run(); err == nil {
		t.Fatal("Run on an occupied port should fail")
	}

	// Graceful shutdown: serve on a free port, hit /healthz, cancel the context.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	probe.Close() // ponytail: freed-port race is fine in a test
	s2, _, _ := newTestServerCfg(t, func(cfg *config.Config) { cfg.Listen = addr })
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- s2.RunContext(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err == nil && resp.StatusCode == 200 {
			resp.Body.Close()
			break
		}
		if err == nil {
			resp.Body.Close()
		}
		if time.Now().After(deadline) {
			t.Fatal("server never became healthy")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("graceful shutdown returned %v", err)
		}
	case <-time.After(25 * time.Second):
		t.Fatal("RunContext never returned after cancel")
	}
}
