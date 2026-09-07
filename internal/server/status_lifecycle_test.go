package server

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/stubbedev/xilo/internal/store"
)

// TestAccountLifecycle pins what each state does on the wire: read-only takes
// nothing new but keeps serving, suspended serves nothing at all, and restore
// undoes both. The binary-cache path is the one that matters — stopping a
// workspace has to stop the bytes, not just grey out a button.
func TestAccountLifecycle(t *testing.T) {
	_, db, ts := newTestServerCfg(t, nil)
	bootstrapAdmin(t, db)
	acct := mustAccount(t, db, "admin")
	if _, err := db.CreateCache("admin", "pub", true, 40); err != nil {
		t.Fatal(err)
	}
	c := adminClient(t, ts)

	get := func(path string) int {
		t.Helper()
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	setStatus := func(status string) {
		t.Helper()
		resp, err := c.PostForm(ts.URL+"/admin/org/admin/status", url.Values{"status": {status}})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	// Active: a public cache answers.
	if code := get("/c/admin/pub/nix-cache-info"); code != http.StatusOK {
		t.Fatalf("active cache-info → %d", code)
	}

	// Read-only: still serving. That is the whole point of the state —
	// freezing a workspace should not break somebody's build mid-afternoon.
	setStatus(store.StatusReadOnly)
	if got := db.AccountStatus(acct.ID); got != store.StatusReadOnly {
		t.Fatalf("status = %q", got)
	}
	if code := get("/c/admin/pub/nix-cache-info"); code != http.StatusOK {
		t.Errorf("read-only cache-info → %d want 200", code)
	}
	// ...but it takes nothing new.
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/c/admin/pub/api/path", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("push to read-only account → %d want 403", resp.StatusCode)
	}

	// Suspended: nothing answers, public or not.
	setStatus(store.StatusSuspended)
	if code := get("/c/admin/pub/nix-cache-info"); code != http.StatusForbidden {
		t.Errorf("suspended cache-info → %d want 403", code)
	}

	// Restored.
	setStatus(store.StatusActive)
	if code := get("/c/admin/pub/nix-cache-info"); code != http.StatusOK {
		t.Errorf("restored cache-info → %d want 200", code)
	}

	// A state this instance does not define is refused rather than stored.
	setStatus("frozen")
	if got := db.AccountStatus(acct.ID); got != store.StatusActive {
		t.Errorf("unknown status stuck: %q", got)
	}
}
