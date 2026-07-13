package store

import (
	"errors"
	"testing"
	"time"
)

func TestAuditRoundTrip(t *testing.T) {
	db := openTest(t)
	if err := db.Audit(AuditEntry{UserID: 7, Actor: "alice", Method: "POST", Path: "/admin/caches", Status: 201, IP: "10.0.0.1", UserAgent: "curl", DurationMs: 12}); err != nil {
		t.Fatal(err)
	}
	if err := db.Audit(AuditEntry{Method: "DELETE", Path: "/api/v1/caches/x/y", Status: 200, IP: "10.0.0.2"}); err != nil {
		t.Fatal(err)
	}
	es, _, err := db.SearchAudit("", 10, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 2 {
		t.Fatalf("want 2 entries, got %d", len(es))
	}
	// newest first
	if es[0].Method != "DELETE" || es[0].Path != "/api/v1/caches/x/y" {
		t.Fatalf("newest entry wrong: %+v", es[0])
	}
	if es[1].Actor != "alice" || es[1].UserID != 7 || es[1].Status != 201 || es[1].IP != "10.0.0.1" || es[1].DurationMs != 12 {
		t.Fatalf("oldest entry wrong: %+v", es[1])
	}

	// Search filters by term across actor/method/path/ip, newest first.
	m, total, err := db.SearchAudit("alice", 10, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(m) != 1 || m[0].Actor != "alice" {
		t.Fatalf("search alice: total=%d got=%+v", total, m)
	}
	if m, total, _ := db.SearchAudit("10.0.0.2", 10, 0, "", ""); total != 1 || m[0].Path != "/api/v1/caches/x/y" {
		t.Fatalf("search by ip: total=%d got=%+v", total, m)
	}
}

func TestPruneAuditBatch(t *testing.T) {
	db := openTest(t)
	// Two entries stamped "now"; cutoff in the future removes both. Batch of 1
	// drains one per call, so the second call reports the tail then zero.
	for i := 0; i < 2; i++ {
		if err := db.Audit(AuditEntry{Method: "POST", Path: "/x", Status: 200}); err != nil {
			t.Fatal(err)
		}
	}
	cutoff := time.Now().Add(time.Hour).Unix()
	if n, err := db.PruneAuditBatch(cutoff, 1); err != nil || n != 1 {
		t.Fatalf("first batch: n=%d err=%v", n, err)
	}
	if n, err := db.PruneAuditBatch(cutoff, 1); err != nil || n != 1 {
		t.Fatalf("second batch: n=%d err=%v", n, err)
	}
	if n, err := db.PruneAuditBatch(cutoff, 1); err != nil || n != 0 {
		t.Fatalf("drained batch: n=%d err=%v", n, err)
	}
	es, _, err := db.SearchAudit("", 10, 0, "", "")
	if err != nil || len(es) != 0 {
		t.Fatalf("audit_log should be empty: len=%d err=%v", len(es), err)
	}
}

// TestSoftDelete proves a deleted user/org reads as gone through the normal
// resolvers yet keeps its row (so audit-log id references still resolve), and
// that reusing a freed org slug reactivates the same row.
func TestSoftDelete(t *testing.T) {
	db := openTest(t)

	org, err := db.EnsureAccount("acme", "org")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteOrg(org.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetAccount("acme"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted org still visible: %v", err)
	}
	var n int
	if err := db.r.QueryRow(`SELECT COUNT(*) FROM accounts WHERE id=? AND status='deleted'`, org.ID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("org row should survive soft-delete: n=%d err=%v", n, err)
	}
	again, err := db.EnsureAccount("acme", "org")
	if err != nil || again.ID != org.ID {
		t.Fatalf("reusing slug should reactivate same row: %+v %v", again, err)
	}
	if _, err := db.GetAccount("acme"); err != nil {
		t.Fatalf("reactivated org should resolve: %v", err)
	}

	u, err := db.CreateUser("bob", "bob@example.com", "h", "user")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteUser(u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetUser(u.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted user still visible: %v", err)
	}
	if err := db.r.QueryRow(`SELECT COUNT(*) FROM users WHERE id=? AND status='deleted'`, u.ID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("user row should survive soft-delete: n=%d err=%v", n, err)
	}
}
