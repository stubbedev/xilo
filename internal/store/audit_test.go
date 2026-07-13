package store

import (
	"errors"
	"testing"
)

func TestAuditRoundTrip(t *testing.T) {
	db := openTest(t)
	if err := db.Audit(7, "alice", "POST", "/admin/caches", 201); err != nil {
		t.Fatal(err)
	}
	if err := db.Audit(0, "", "DELETE", "/api/v1/caches/x/y", 200); err != nil {
		t.Fatal(err)
	}
	es, err := db.ListAudit(10)
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
	if es[1].Actor != "alice" || es[1].UserID != 7 || es[1].Status != 201 {
		t.Fatalf("oldest entry wrong: %+v", es[1])
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
