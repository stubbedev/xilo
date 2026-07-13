package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
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

	hit(ok, "POST", "/admin/caches")                 // recorded
	hit(ok, "DELETE", "/api/v1/caches/acme/web")     // recorded
	hit(ok, "GET", "/admin/caches")                  // skip: read
	hit(ok, "POST", "/admin/account/password/check") // skip: /check handshake
	hit(ok, "POST", "/admin/login/passkey/begin")    // skip: /begin handshake
	hit(ok, "PUT", "/c/default/web/api/path")        // skip: cache traffic
	hit(forbidden, "POST", "/admin/orgs")            // skip: failed (403)

	es, _, err := db.SearchAudit("", 20, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 2 {
		t.Fatalf("want 2 recorded actions, got %d: %+v", len(es), es)
	}
	if es[0].Method != "DELETE" || es[0].Path != "/api/v1/caches/acme/web" {
		t.Fatalf("newest entry wrong: %+v", es[0])
	}
	if es[1].Method != "POST" || es[1].Path != "/admin/caches" || es[1].Status != 200 {
		t.Fatalf("oldest entry wrong: %+v", es[1])
	}
}
