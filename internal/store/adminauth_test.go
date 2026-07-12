package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestUsersLifecycle(t *testing.T) {
	db := openTest(t)
	if db.UsersExist() {
		t.Fatal("no users should exist yet")
	}
	u, err := db.CreateUser("admin", "", "hash-1", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if !db.UsersExist() {
		t.Fatal("users should exist after create")
	}
	if _, err := db.CreateUser("admin", "", "x", "user"); err == nil {
		t.Fatal("duplicate username should fail")
	}
	got, err := db.GetUserByName("admin")
	if err != nil || got.ID != u.ID || got.PassHash != "hash-1" || got.Role != "owner" {
		t.Fatalf("GetUserByName: %+v %v", got, err)
	}
	if _, err := db.GetUser(9999); err != ErrNotFound {
		t.Fatalf("missing user: %v", err)
	}
	if err := db.SetUserPassword(u.ID, "hash-3"); err != nil {
		t.Fatal(err)
	}
	if got, _ = db.GetUser(u.ID); got.PassHash != "hash-3" {
		t.Fatalf("password not updated: %q", got.PassHash)
	}

	m, err := db.CreateUser("bob", "", "h", "user")
	if err != nil {
		t.Fatal(err)
	}
	users, err := db.ListUsers()
	if err != nil || len(users) != 2 {
		t.Fatalf("ListUsers: %v %v", users, err)
	}

	// Deleting a user takes their passkeys and sessions along.
	if err := db.AddPasskey(m.ID, "k", []byte("cred")); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteUser(m.ID); err != nil {
		t.Fatal(err)
	}
	if pks, _ := db.ListUserPasskeys(m.ID); len(pks) != 0 {
		t.Fatal("passkeys should be gone with the user")
	}
	if _, err := db.GetUser(m.ID); err != ErrNotFound {
		t.Fatalf("user should be gone: %v", err)
	}
}

func TestTOTPLifecycle(t *testing.T) {
	db := openTest(t)
	u, err := db.CreateUser("admin", "", "h", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, enabled, _ := db.UserTOTP(u.ID); enabled {
		t.Fatal("totp should start disabled")
	}
	secret := []byte("0123456789abcdef0123")
	if err := db.SetUserTOTPSecret(u.ID, secret); err != nil {
		t.Fatal(err)
	}
	got, enabled, _ := db.UserTOTP(u.ID)
	if enabled || string(got) != string(secret) {
		t.Fatalf("after enroll: enabled=%v secret match=%v", enabled, string(got) == string(secret))
	}
	db.SetUserTOTPEnabled(u.ID, true)
	if _, enabled, _ := db.UserTOTP(u.ID); !enabled {
		t.Fatal("totp should be enabled")
	}
	// disabling clears the secret
	db.SetUserTOTPEnabled(u.ID, false)
	got, enabled, _ = db.UserTOTP(u.ID)
	if enabled || len(got) != 0 {
		t.Fatalf("after disable: enabled=%v secretLen=%d", enabled, len(got))
	}
}

// openLegacyAdminDB writes a pre-users database (singleton admin table, no
// user_id columns) and reopens it through store.Open so migrate() runs.
func openLegacyAdminDB(t *testing.T) *DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legacy.db")
	raw, err := sql.Open("sqlite", "file:"+path+pragmas)
	if err != nil {
		t.Fatal(err)
	}
	legacy := []string{
		`CREATE TABLE admin (id INTEGER PRIMARY KEY CHECK (id = 1), password_hash TEXT NOT NULL,
			totp_secret BLOB, totp_enabled INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE passkeys (id INTEGER PRIMARY KEY, name TEXT NOT NULL, credential BLOB NOT NULL, created INTEGER NOT NULL)`,
		`CREATE TABLE sessions (id TEXT PRIMARY KEY, expires INTEGER NOT NULL)`,
		`INSERT INTO admin (id, password_hash, totp_secret, totp_enabled) VALUES (1, 'legacy-hash', x'aa', 1)`,
		`INSERT INTO passkeys (name, credential, created) VALUES ('old-key', x'bb', 1)`,
		`INSERT INTO sessions (id, expires) VALUES ('somehash', 9999999999)`,
	}
	for _, s := range legacy {
		if _, err := raw.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	raw.Close()
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestAdminMigration builds a pre-users database (singleton admin table) and
// checks migrate() converts it: admin row → user "admin" (role owner), passkeys claimed,
// sessions wiped, old table dropped.
func TestAdminMigration(t *testing.T) {
	db := openLegacyAdminDB(t)
	u, err := db.GetUserByName("admin")
	if err != nil || u.Role != "owner" || u.PassHash != "legacy-hash" {
		t.Fatalf("migrated admin: %+v %v", u, err)
	}
	if !u.TOTPEnabled {
		t.Fatal("totp enablement should migrate")
	}
	pks, err := db.ListUserPasskeys(u.ID)
	if err != nil || len(pks) != 1 || pks[0].Name != "old-key" {
		t.Fatalf("passkeys should belong to migrated admin: %v %v", pks, err)
	}
	if _, ok := db.SessionUser("somehash"); ok {
		t.Fatal("legacy sessions should be wiped")
	}
	// The old table is gone — a second migrate (reopen) must not resurrect it.
	if db.r.QueryRow(`SELECT 1 FROM sqlite_master WHERE name='admin'`).Scan(new(int)) == nil {
		t.Fatal("admin table should be dropped")
	}
}

// TestOwnerHierarchy pins the permission invariants: org ownership is set
// once at creation and can never be granted, changed, removed, or duplicated
// — no escalation to owner, no ownerless orgs, no lockout.
func TestOwnerHierarchy(t *testing.T) {
	db := openTest(t)
	creator, err := db.CreateUser("carol", "", "h", "user")
	if err != nil {
		t.Fatal(err)
	}
	other, err := db.CreateUser("dave", "", "h", "user")
	if err != nil {
		t.Fatal(err)
	}
	org, err := db.EnsureAccount("acme", "org")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MakeOwner(org.ID, creator.ID); err != nil {
		t.Fatal(err)
	}
	if db.MemberRole(org.ID, creator.ID) != "owner" {
		t.Fatal("creator should own the org")
	}
	if err := db.MakeOwner(org.ID, other.ID); err == nil {
		t.Fatal("second owner must be refused")
	}
	if err := db.SetMember(org.ID, other.ID, "owner"); err == nil {
		t.Fatal("granting owner must be refused")
	}
	if err := db.SetMember(org.ID, creator.ID, "user"); err == nil {
		t.Fatal("demoting the owner must be refused")
	}
	if err := db.RemoveMember(org.ID, creator.ID); err == nil {
		t.Fatal("removing the owner must be refused")
	}
	if !db.OwnsOrgs(creator.ID) || db.OwnsOrgs(other.ID) {
		t.Fatal("OwnsOrgs wrong")
	}
	// Admin and user remain grantable, changeable, removable.
	if err := db.SetMember(org.ID, other.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetMember(org.ID, other.ID, "user"); err != nil {
		t.Fatal(err)
	}
	if err := db.RemoveMember(org.ID, other.ID); err != nil {
		t.Fatal(err)
	}
	// A user's personal account is owner-held too — nobody else joins it.
	personal, err := db.GetAccount("carol")
	if err != nil {
		t.Fatal(err)
	}
	if db.MemberRole(personal.ID, creator.ID) != "owner" {
		t.Fatal("personal account should be owner-held")
	}
	if err := db.SetMember(personal.ID, other.ID, "user"); err == nil {
		t.Fatal("personal accounts must not take members")
	}
}

// TestDeleteCascades pins the destruction contract: deleting an org takes
// its memberships, tokens, caches and paths along (chunk blobs are the GC's
// job — dedup may share them with other accounts); deleting a user revokes
// their personal account's tokens; personal accounts never go through
// DeleteOrg.
func TestDeleteCascades(t *testing.T) {
	db := openTest(t)
	u, err := db.CreateUser("carol", "", "h", "user")
	if err != nil {
		t.Fatal(err)
	}
	org, err := db.EnsureAccount("acme", "org")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MakeOwner(org.ID, u.ID); err != nil {
		t.Fatal(err)
	}
	c, err := db.CreateCache("acme", "web", true, 40)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PutPath(c.ID, "h1", &Path{StorePath: "/nix/store/h1-x", NarHash: "sha256:x", NarSize: 1}); err != nil {
		t.Fatal(err)
	}
	sec, _, err := db.CreateToken(org.ID, "orgtok", []string{"web"}, []string{"pull"}, 0)
	if err != nil {
		t.Fatal(err)
	}

	// Personal accounts are refused.
	personal, err := db.GetAccount("carol")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteOrg(personal.ID); err == nil {
		t.Fatal("DeleteOrg must refuse personal accounts")
	}

	if err := db.DeleteOrg(org.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetAccount("acme"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("org should be gone: %v", err)
	}
	if _, err := db.GetCache("acme", "web"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cache should be gone: %v", err)
	}
	var n int
	if db.r.QueryRow(`SELECT COUNT(*) FROM paths`).Scan(&n); n != 0 {
		t.Fatalf("paths should cascade away, %d left", n)
	}
	if db.Authorize(sec, "acme", "web", "pull", 100) {
		t.Fatal("org token must die with the org")
	}
	if db.MemberRole(org.ID, u.ID) != "" {
		t.Fatal("membership should be gone")
	}

	// Deleting a user revokes their personal account's tokens.
	if _, err := db.CreateCache("carol", "pc", true, 40); err != nil {
		t.Fatal(err)
	}
	psec, ptok, err := db.CreateToken(personal.ID, "ptok", []string{"pc"}, []string{"pull"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !db.Authorize(psec, "carol", "pc", "pull", 100) {
		t.Fatal("personal token should work before deletion")
	}
	if err := db.DeleteUser(u.ID); err != nil {
		t.Fatal(err)
	}
	if db.Authorize(psec, "carol", "pc", "pull", 100) {
		t.Fatal("personal token must be revoked with the user")
	}
	if got, err := db.GetToken(ptok.ID); err != nil || !got.Revoked {
		t.Fatalf("token should be revoked, got %+v %v", got, err)
	}
}
