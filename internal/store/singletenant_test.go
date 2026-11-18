package store

import (
	"errors"
	"testing"
)

func ownerOf(t *testing.T, db *DB, slug string) int64 {
	t.Helper()
	acc, err := db.GetAccount(slug)
	if err != nil {
		t.Fatalf("account %s: %v", slug, err)
	}
	members, err := db.ListMembers(acc.ID)
	if err != nil {
		t.Fatalf("members of %s: %v", slug, err)
	}
	for _, m := range members {
		if m.Role == "owner" {
			return m.UserID
		}
	}
	return 0
}

func superadmin(t *testing.T, db *DB) *User {
	t.Helper()
	u, err := db.CreateUser("admin", "", "hash", RoleSuperadmin)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestSingleTenantCacheLandsInAWorkspaceTheSuperadminOwns(t *testing.T) {
	db := openTest(t)
	db.SetSingleTenant(true)
	admin := superadmin(t, db)
	if _, err := db.CreateCache("stubbedev", "main", true, 40); err != nil {
		t.Fatal(err)
	}
	if got := ownerOf(t, db, "stubbedev"); got != admin.ID {
		t.Fatalf("owner of the workspace a cache created = user %d, want the superadmin %d", got, admin.ID)
	}
	accs, err := db.UserAccounts(admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(accs) != 1 || accs[0].Slug != "stubbedev" {
		t.Fatalf("superadmin's workspaces = %+v, want only stubbedev", accs)
	}
}

func TestMultiTenantAccountsStayUnowned(t *testing.T) {
	db := openTest(t)
	superadmin(t, db)
	if _, err := db.CreateCache("acme", "web", true, 40); err != nil {
		t.Fatal(err)
	}
	if got := ownerOf(t, db, "acme"); got != 0 {
		t.Fatalf("a tenant's workspace was given to user %d", got)
	}
}

func TestClaimUnownedAccountsRepairsOnlyWhatNobodyIsIn(t *testing.T) {
	db := openTest(t)
	admin := superadmin(t, db)
	alice, err := db.CreateUser("alice", "", "hash", "user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateCache("stubbedev", "main", true, 40); err != nil {
		t.Fatal(err)
	}
	gone, err := db.EnsureAccount("gone", "org")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteOrg(gone.ID); err != nil {
		t.Fatal(err)
	}
	claimed, err := db.ClaimUnownedAccounts()
	if err != nil {
		t.Fatal(err)
	}
	if claimed != 1 {
		t.Fatalf("claimed %d accounts, want 1", claimed)
	}
	if got := ownerOf(t, db, "stubbedev"); got != admin.ID {
		t.Fatalf("owner of stubbedev = user %d, want the superadmin %d", got, admin.ID)
	}
	if got := ownerOf(t, db, "alice"); got != alice.ID {
		t.Fatalf("owner of alice's own workspace = user %d, want alice %d", got, alice.ID)
	}
	if again, err := db.ClaimUnownedAccounts(); err != nil || again != 0 {
		t.Fatalf("second repair claimed %d accounts (err %v), want 0", again, err)
	}
}

func TestClaimUnownedAccountsWithoutASuperadminClaimsNothing(t *testing.T) {
	db := openTest(t)
	if _, err := db.CreateCache("stubbedev", "main", true, 40); err != nil {
		t.Fatal(err)
	}
	if claimed, err := db.ClaimUnownedAccounts(); err != nil || claimed != 0 {
		t.Fatalf("claimed %d accounts (err %v) with no superadmin to give them to", claimed, err)
	}
}

func TestMakeOwnerIsIdempotentForTheSameOwner(t *testing.T) {
	db := openTest(t)
	admin := superadmin(t, db)
	alice, err := db.CreateUser("alice", "", "hash", "user")
	if err != nil {
		t.Fatal(err)
	}
	acc, err := db.EnsureAccount("acme", "org")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := db.MakeOwner(acc.ID, admin.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.MakeOwner(acc.ID, alice.ID); !errors.Is(err, ErrHasOwner) {
		t.Fatalf("second owner: err = %v, want ErrHasOwner", err)
	}
}
