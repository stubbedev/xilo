package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stubbedev/xilo/internal/store"
)

// TestAuditMiddleware checks which requests get recorded as activities: only
// successful mutating admin/API calls — not reads, auth handshakes, cache
// traffic, or failed attempts.
func TestAuditMiddleware(t *testing.T) {
	s, db, _ := newTestServerCfg(t, nil)

	ok := s.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	forbidden := s.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	hit := func(h http.Handler, method, path string) {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, path, nil))
	}

	secret, _, err := db.CreateToken(0, "robot", nil, []string{"admin"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/caches", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	ok.ServeHTTP(httptest.NewRecorder(), req) // recorded, actor = token name

	hit(ok, "POST", "/admin/caches")                 // recorded
	hit(ok, "DELETE", "/api/v1/caches/acme/web")     // recorded
	hit(ok, "GET", "/admin/caches")                  // skip: read
	hit(ok, "POST", "/admin/account/password/check") // skip: /check handshake
	hit(ok, "POST", "/admin/login/passkey/begin")    // skip: /begin handshake
	hit(ok, "PUT", "/c/default/web/api/path")        // skip: cache traffic
	hit(forbidden, "POST", "/admin/orgs")            // skip: failed (403)

	es, _, err := db.SearchAudit("", "", "", 20, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 3 {
		t.Fatalf("want 3 recorded actions, got %d: %+v", len(es), es)
	}
	if tok := es[2]; tok.Actor != "robot" || tok.UserID != 0 {
		t.Fatalf("token call should be attributed to the token: %+v", tok)
	}
	if es[0].Method != http.MethodDelete || es[0].Path != "/api/v1/caches/acme/web" {
		t.Fatalf("newest entry wrong: %+v", es[0])
	}
	if es[1].Method != http.MethodPost || es[1].Path != "/admin/caches" || es[1].Status != 200 {
		t.Fatalf("oldest entry wrong: %+v", es[1])
	}
}

// TestAuditPageFilters: the activities page accepts method/status chips
// composed with search, sort and paging, and ignores values it never offers.
func TestAuditPageFilters(t *testing.T) {
	_, db, ts := newTestServerCfg(t, nil)
	bootstrapAdmin(t, db)
	c := adminClient(t, ts)
	if err := db.Audit(store.AuditEntry{Actor: "admin", Method: "POST", Path: "/admin/caches", Status: 303, DurationMs: 3}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"", "?method=POST&status=3xx&q=cach&sort=status&dir=asc", "?method=BREW&status=9xx", "?method=DELETE&page[number]=7"} {
		resp, err := c.Get(ts.URL + "/admin/audit" + q)
		if err != nil {
			t.Fatal(err)
		}
		if b := body(t, resp); resp.StatusCode != http.StatusOK || !contains(b, "Actions recorded") {
			t.Errorf("GET /admin/audit%s → %d", q, resp.StatusCode)
		}
	}
	resp, _ := c.Get(ts.URL + "/admin/audit?method=POST")
	if b := body(t, resp); !contains(b, "/admin/caches") {
		t.Error("POST filter dropped the matching entry")
	}
	resp, _ = c.Get(ts.URL + "/admin/audit?method=DELETE")
	if b := body(t, resp); contains(b, ">/admin/caches<") {
		t.Error("DELETE filter kept a POST entry")
	}
}
