// Package views_test smoke-renders every exported templ component: each must
// render without error and key pages must contain an expected marker string.
package views_test

import (
	"bytes"
	"context"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"
	"github.com/stubbedev/xilo/internal/server/views"
	"github.com/stubbedev/xilo/internal/store"
)

func render(t *testing.T, name string, c templ.Component) string {
	t.Helper()
	var buf bytes.Buffer
	if err := c.Render(context.Background(), &buf); err != nil {
		t.Fatalf("%s: render error: %v", name, err)
	}
	return buf.String()
}

var bytesFn = func(n int64) string { return strconv.FormatInt(n, 10) + " B" }

func sortCtx() views.SortCtx {
	return views.SortCtx{
		Path:      "/admin",
		Query:     url.Values{"q": {"x"}, "page": {"2"}},
		SortParam: "sort", DirParam: "dir", PageParam: "page",
		Key: "name", Dir: "asc", Target: "#tbl",
	}
}

func demoCache() store.Cache {
	return store.Cache{ID: 1, AccountID: 1, Account: "default", Name: "demo", Public: true, Priority: 40,
		PubKey: "demo:AAAA", MaxBytes: 100, Retention: 7 * 86400, Created: 1}
}

func demoTokens() []store.Token {
	now := time.Now().Unix()
	return []store.Token{
		{ID: 1, Name: "all-perms", Caches: []string{"*"}, Perms: []string{"push", "pull"}, Created: now},
		{ID: 2, Name: "revoked", Caches: []string{"demo"}, Perms: []string{"pull"}, Revoked: true, Expires: now + 3600, Created: now},
		{ID: 3, Name: "expired", Caches: []string{"demo"}, Perms: []string{"push"}, Expires: 1, Created: 1},
	}
}

func TestSmokeAllComponents(t *testing.T) {
	sc := sortCtx()
	pager := views.Pager{Prev: "/a?p=1", Next: "/a?p=3", Page: 2, Pages: 3, Target: "#x"}
	cases := []struct {
		name   string
		c      templ.Component
		marker string // "" = only assert render succeeds
	}{
		{"Layout", views.Layout("Overview", "caches", views.Nav{LoggedIn: true, UserName: "admin", IsAdmin: true}, views.Flash{}), "Overview"},
		{"Layout-flash-code", views.Layout("t", "", views.Nav{}, views.Flash{Msg: "created", Code: "secret123"}), "secret123"},
		{"Login", views.Login(false, false, false, views.Flash{}), "assword"},
		{"Login-nopw-passkeys", views.Login(true, true, true, views.Flash{Msg: "bad login"}), "passkey"},
		{"LoginCode", views.LoginCode("pending-id", views.Flash{Msg: "wrong code"}), "code"},
		{"Account", views.Account(views.AccountData{Nav: views.Nav{LoggedIn: true, UserName: "admin"}, User: &store.User{Name: "admin", Role: "admin"}, Passkeys: []store.Passkey{{ID: 1, Name: "yubikey", Created: 1}}}), "yubikey"},
		{"Instance", views.Instance(views.InstanceData{Nav: views.Nav{LoggedIn: true, UserName: "admin", IsAdmin: true}, Users: []store.User{{ID: 1, Name: "admin", Role: "admin"}}, Orgs: []views.OrgInfo{{Account: store.Account{ID: 1, Slug: "acme", Kind: "org"}, Members: []store.AccountMember{{UserID: 1, UserName: "admin", Role: "admin"}}}}, MultiTenant: true, AllowRegs: true, Plans: []store.Plan{{ID: 1, Name: "free", MaxCaches: 3, Public: true}}, Flash: views.Flash{Msg: "saved"}}), "saved"},
		{"PwHint-empty", views.PwHint(""), ""},
		{"PwHint-short", views.PwHint("short"), "short"},
		{"PwHint-weak", views.PwHint("weak"), "Weak"},
		{"PwHint-strong", views.PwHint("strong"), "Strong"},
		{"PwHint-mismatch", views.PwHint("mismatch"), "match"},
		{"TOTPEnroll", views.TOTPEnroll(views.Nav{LoggedIn: true}, "data:image/png;base64,x", "SECRET"), "SECRET"},
		{"TOTPEnrollErr", views.TOTPEnrollErr(views.Nav{LoggedIn: true}, "data:image/png;base64,x", "SECRET", "bad code"), "bad code"},
		{"SectionHeader", views.SectionHeader("Head"), "Head"},
		{"SearchInput", views.SearchInput("/admin", "q", "val", "find...", "#t"), "find..."},
		{"StatTile", views.StatTile("box", "42", "things", "kpi1", ""), "42"},
		{"StatTile-tone", views.StatTile("box", "9", "over", "kpi2", "text-destructive"), "over"},
		{"EmptyState", views.EmptyState("inbox", "nothing here"), "nothing here"},
		{"StatusBadge", views.StatusBadge("active"), "active"},
		{"VisBadge-public", views.VisBadge(true), "Public"},
		{"VisBadge-private", views.VisBadge(false), "Private"},
		{"PermBadges", views.PermBadges([]string{"push", "pull"}), "push"},
		{"CopyField", views.CopyField("copy-me"), "copy-me"},
		{"ConfirmForm", views.ConfirmForm(views.Confirm{ID: "c1", Action: "/admin/gc", Title: "Sure?", Message: "Really?", ConfirmLabel: "Yes", ConfirmIcon: "check", TriggerLabel: "Delete", TriggerIcon: "trash-2"}), "Delete"},
		{"NotFound-in", views.NotFound(views.Nav{LoggedIn: true}), "not found"},
		{"NotFound-out", views.NotFound(views.Nav{}), "not found"},
		{"Icon", views.Icon("check"), "svg"},
		{"IconClass", views.IconClass("sun", 20, "spin"), "svg"},
		{"PagerNav", views.PagerNav(pager), "Next"},
		{"PagerNav-single", views.PagerNav(views.Pager{Page: 1, Pages: 1}), ""},
		{"SortHead-active", views.SortHead(sc, "name", "Name"), "Name"},
		{"SortHead-desc", views.SortHead(views.SortCtx{Path: "/a", SortParam: "sort", DirParam: "dir", PageParam: "page", Key: "name", Dir: "desc"}, "name", "Name"), "descending"},
	}
	for _, tc := range cases {
		out := render(t, tc.name, tc.c)
		if tc.marker != "" && !strings.Contains(strings.ToLower(out), strings.ToLower(tc.marker)) {
			t.Errorf("%s: output missing %q:\n%s", tc.name, tc.marker, out)
		}
	}
}

func TestSmokeDashboard(t *testing.T) {
	d := views.DashboardData{
		Global: store.Global{Caches: 2, Paths: 3, Chunks: 4, StoredBytes: 100, LogicalBytes: 200},
		Caches: []views.CacheUsage{
			{Cache: demoCache(), Bytes: 96, Paths: 2},                                         // capped, near cap
			{Cache: store.Cache{ID: 2, Name: "open", PubKey: "open:BB"}, Bytes: 10, Paths: 1}, // uncapped, public=false
		},
		Tokens:     demoTokens(),
		Flash:      views.Flash{Msg: "token created", Code: "tok-secret"},
		ServerCap:  1000,
		Bytes:      bytesFn,
		CachePager: views.Pager{Prev: "/a?p=1", Next: "/a?p=3", Page: 2, Pages: 3},
		TokenPager: views.Pager{Page: 1, Pages: 1},
		TokenSort:  sortCtx(),
		CacheQuery: "de",
		TokenQuery: "to",
	}
	out := render(t, "Dashboard", views.Dashboard(d))
	for _, want := range []string{"demo", "open", "all-perms", "revoked", "tok-secret"} {
		if !strings.Contains(out, want) {
			t.Errorf("Dashboard missing %q", want)
		}
	}
	// Empty-state branches.
	render(t, "Dashboard-empty", views.Dashboard(views.DashboardData{Bytes: bytesFn, TokenSort: sortCtx()}))
	// Uncapped cache with zero global storage → barMax's min-1 floor.
	render(t, "Dashboard-zero-global", views.Dashboard(views.DashboardData{
		Caches:    []views.CacheUsage{{Cache: store.Cache{ID: 3, Name: "fresh", PubKey: "fresh:CC"}}},
		Bytes:     bytesFn,
		TokenSort: sortCtx(),
	}))
}

func TestPageOfClampLow(t *testing.T) {
	// page < 1 clamps to 1 (rest of PageOf is covered in data_test.go).
	if _, p, _ := views.PageOf(make([]int, 5), 0, 2); p != 1 {
		t.Fatalf("PageOf page=0 clamped to %d, want 1", p)
	}
}

func TestSmokeCacheView(t *testing.T) {
	now := time.Now().Unix()
	d := views.CacheData{
		Cache:   demoCache(),
		Stats:   store.Stats{Paths: 2, Chunks: 3, LogicalBytes: 200, PhysicalBytes: 100},
		Dedup:   "2.00",
		BaseURL: "http://localhost:8080",
		Host:    "localhost:8080",
		Bytes:   bytesFn,
		Paths: []store.PathInfo{
			{StorePath: "/nix/store/8kvxvr3pmsypxiypq4g8zy13glnfr7nx-glibc-2.42", NarSize: 100, Accessed: now},
			{StorePath: "/nix/store/nodash", NarSize: 50, Accessed: 0},
		},
		PathQuery: "gl",
		PathTotal: 2,
		PathPager: views.Pager{Page: 1, Pages: 2, Next: "/c?p=2"},
		PathSort:  sortCtx(),
	}
	out := render(t, "CacheView", views.CacheView(d))
	for _, want := range []string{"demo", "glibc-2.42", "demo:AAAA", "http://localhost:8080/c/default/demo"} {
		if !strings.Contains(out, want) {
			t.Errorf("CacheView missing %q", want)
		}
	}
	// Private cache, no paths, no query → empty state + private note.
	priv := d
	priv.Cache.Public = false
	priv.Cache.MaxBytes = 0
	priv.Paths = nil
	priv.PathQuery = ""
	priv.PathTotal = 0
	priv.PathPager = views.Pager{Page: 1, Pages: 1}
	render(t, "CacheView-private-empty", views.CacheView(priv))
	// Query with no matches → nomatch branch.
	nomatch := priv
	nomatch.PathQuery = "zzz"
	render(t, "CacheView-nomatch", views.CacheView(nomatch))
}

func TestSmokeStatusPage(t *testing.T) {
	d := views.StatusData{
		Healthy: true, Uptime: "1h2m", HitPct: "50%", Updated: "12:00:00",
		Rate: 5, Window: 60,
		Global:    store.Global{Caches: 1, Paths: 2, Chunks: 3, StoredBytes: 100, LogicalBytes: 200},
		AuthFails: 1, NarServed: 2, Requests: 3,
		Bytes:  bytesFn,
		Charts: []views.ChartData{{ID: "req", Label: "Requests / second", Cur: "1.0", Peak: "2.0"}},
	}
	out := render(t, "StatusPage", views.StatusPage(d))
	for _, want := range []string{"1h2m", "50%", "Requests / second"} {
		if !strings.Contains(out, want) {
			t.Errorf("StatusPage missing %q", want)
		}
	}
	// Custom range + degraded → rateURL custom branch and unhealthy badge.
	custom := d
	custom.Healthy = false
	custom.Window = 0
	custom.From, custom.To = "2026-07-01", "2026-07-09"
	render(t, "StatusPage-custom", views.StatusPage(custom))
}

func TestAsset(t *testing.T) {
	if got := views.Asset("app.css"); got != "/static/app.css" {
		t.Fatalf("Asset unversioned = %q", got)
	}
	views.SetAssetVersions(map[string]string{"app.css": "abc123"})
	defer views.SetAssetVersions(map[string]string{})
	if got := views.Asset("app.css"); got != "/static/app.css?v=abc123" {
		t.Fatalf("Asset versioned = %q", got)
	}
	if got := views.Asset("other.js"); got != "/static/other.js" {
		t.Fatalf("Asset unknown = %q", got)
	}
}

func TestTokenHelpers(t *testing.T) {
	toks := demoTokens()
	active, revoked, expired := toks[0], toks[1], toks[2]
	if views.TokenStatus(active) != "active" || views.TokenStatus(revoked) != "revoked" || views.TokenStatus(expired) != "expired" {
		t.Fatalf("TokenStatus: %s/%s/%s", views.TokenStatus(active), views.TokenStatus(revoked), views.TokenStatus(expired))
	}
	if !views.TokenActive(active) || views.TokenActive(revoked) || views.TokenActive(expired) {
		t.Fatal("TokenActive misclassifies")
	}
	if views.TokenExpiry(active) != "never" {
		t.Fatalf("TokenExpiry(never) = %q", views.TokenExpiry(active))
	}
	if got := views.TokenExpiry(store.Token{Expires: 86400}); got != time.Unix(86400, 0).Format("2006-01-02") {
		t.Fatalf("TokenExpiry(date) = %q", got)
	}
}

func TestRemaining(t *testing.T) {
	now := time.Now().Unix()
	if views.Remaining(0) != 0 {
		t.Fatal("Remaining(0) != 0")
	}
	if views.Remaining(now-10) != 0 {
		t.Fatal("Remaining(past) != 0")
	}
	// >48h rounds up to whole days: 3 days out → exactly 3 days.
	if got := views.Remaining(now + 3*86400); got != 3*86400 {
		t.Fatalf("Remaining(3d) = %d", got)
	}
	// <=48h rounds up to whole hours: 25h out → exactly 25h.
	if got := views.Remaining(now + 25*3600); got != 25*3600 {
		t.Fatalf("Remaining(25h) = %d", got)
	}
}

func TestAgo(t *testing.T) {
	now := time.Now().Unix()
	cases := []struct {
		ts   int64
		want string
	}{
		{0, "never"},
		{now, "just now"},
		{now - 120, "2m ago"},
		{now - 2*3600, "2h ago"},
		{now - 3*86400, "3d ago"},
		{now - 60*86400, time.Unix(now-60*86400, 0).Format("2006-01-02")},
	}
	for _, c := range cases {
		if got := views.Ago(c.ts); got != c.want {
			t.Errorf("Ago(%d) = %q, want %q", c.ts, got, c.want)
		}
	}
}

func TestT(t *testing.T) {
	if views.T("tok.never") != "never" {
		t.Fatalf("T(tok.never) = %q", views.T("tok.never"))
	}
	if views.T("no.such.key") != "no.such.key" {
		t.Fatal("unknown key must echo the id")
	}
}

func TestSortCtxURL(t *testing.T) {
	sc := sortCtx() // active: name asc, page param present
	u := sc.URL("name")
	if !strings.Contains(u, "dir=desc") || !strings.Contains(u, "sort=name") {
		t.Fatalf("active column should flip to desc: %q", u)
	}
	if strings.Contains(u, "page=") {
		t.Fatalf("page param must reset on sort change: %q", u)
	}
	if u2 := sc.URL("size"); !strings.Contains(u2, "dir=asc") || !strings.Contains(u2, "q=x") {
		t.Fatalf("other column: %q", u2)
	}
}

func TestCacheUsage(t *testing.T) {
	capped := views.CacheUsage{Cache: store.Cache{MaxBytes: 100}, Bytes: 50}
	if capped.Pct() != 50 || capped.Over() {
		t.Fatalf("capped: pct=%d over=%v", capped.Pct(), capped.Over())
	}
	over := views.CacheUsage{Cache: store.Cache{MaxBytes: 100}, Bytes: 150}
	if over.Pct() != 100 || !over.Over() {
		t.Fatalf("over: pct=%d over=%v", over.Pct(), over.Over())
	}
	uncapped := views.CacheUsage{Bytes: 50}
	if uncapped.Pct() != 0 || uncapped.Over() {
		t.Fatalf("uncapped: pct=%d over=%v", uncapped.Pct(), uncapped.Over())
	}
}
