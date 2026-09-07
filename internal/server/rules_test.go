package server

import (
	"net/url"
	"testing"
	"time"
)

// TestInstanceRules pins the two rules with teeth — a token cannot outlive the
// instance maximum, and a new cache inherits the default retention — plus the
// contract that an empty field means no rule at all.
func TestInstanceRules(t *testing.T) {
	srv, db, ts := newTestServerCfg(t, nil)
	bootstrapAdmin(t, db)
	c := adminClient(t, ts)

	// No rules yet: a token asked to never expire never expires.
	if got := srv.capTokenExpiry(0); got != 0 {
		t.Fatalf("uncapped permanent token got expiry %d", got)
	}

	// A sysadmin sets a ceiling of 30 days on token life and 7 days of
	// retention for new caches.
	_, b := postFlash(t, c, ts.URL+"/admin/settings/rules", url.Values{
		"max_token_ttl_value":     {"30"},
		"max_token_ttl_unit":      {"d"},
		"default_retention_value": {"7"},
		"default_retention_unit":  {"d"},
		"instance_cap_value":      {"5"},
		"instance_cap_unit":       {"GiB"},
	})
	if !contains(b, "aved") {
		t.Fatalf("saving rules: %.120q", b)
	}

	// A permanent token is now capped, and a longer one is pulled back.
	now := time.Now().Unix()
	capped := srv.capTokenExpiry(0)
	if capped <= now || capped > now+31*86400 {
		t.Errorf("permanent token expiry %d not inside the 30d ceiling", capped-now)
	}
	if got := srv.capTokenExpiry(now + 365*86400); got > now+31*86400 {
		t.Errorf("year-long token survived the ceiling: %d days", (got-now)/86400)
	}
	// A shorter one is left alone — the rule is a ceiling, not a default.
	shorter := now + 3600
	if got := srv.capTokenExpiry(shorter); got != shorter {
		t.Errorf("short token rewritten: %d want %d", got, shorter)
	}

	if got := srv.defaultRetention(); got != 7*86400 {
		t.Errorf("default retention = %d want %d", got, 7*86400)
	}
	if got := srv.instanceCap(); got != 5<<30 {
		t.Errorf("instance cap = %d want %d", got, int64(5)<<30)
	}

	// A cache created with no retention of its own inherits the rule.
	resp, err := c.PostForm(ts.URL+"/admin/caches", url.Values{
		"namespace": {"admin"}, "name": {"ruled"}, "priority": {"40"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	cache, err := db.GetCache("admin", "ruled")
	if err != nil {
		t.Fatalf("GetCache: %v", err)
	}
	if cache.Retention != 7*86400 {
		t.Errorf("new cache retention = %d want %d", cache.Retention, 7*86400)
	}

	// Clearing the fields removes the rules.
	postFlash(t, c, ts.URL+"/admin/settings/rules", url.Values{})
	if got := srv.capTokenExpiry(0); got != 0 {
		t.Errorf("cleared ceiling still caps: %d", got)
	}
	if got := srv.defaultRetention(); got != 0 {
		t.Errorf("cleared retention = %d", got)
	}
}
