package store

import (
	"errors"
	"testing"
)

func TestAccountLifecycle(t *testing.T) {
	db := openTest(t)

	a, err := db.EnsureAccount("acme", "org")
	if err != nil || a.ID == 0 || a.Slug != "acme" || a.Kind != "org" {
		t.Fatalf("EnsureAccount: %+v %v", a, err)
	}
	// Idempotent.
	if again, err := db.EnsureAccount("acme", "org"); err != nil || again.ID != a.ID {
		t.Fatalf("EnsureAccount idempotence: %+v %v", again, err)
	}

	got, err := db.GetAccountByID(a.ID)
	if err != nil || got.Slug != "acme" {
		t.Fatalf("GetAccountByID: %+v %v", got, err)
	}
	if _, err := db.GetAccountByID(99999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetAccountByID missing = %v, want ErrNotFound", err)
	}
	if _, err := db.GetAccount("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetAccount missing = %v, want ErrNotFound", err)
	}

	if err := db.SetAccountPlan(a.ID, 7); err != nil {
		t.Fatalf("SetAccountPlan: %v", err)
	}
	if got, _ := db.GetAccount("acme"); got.PlanID != 7 {
		t.Fatalf("plan not applied: %+v", got)
	}

	db.EnsureAccount("beta", "org")
	// A "default" account exists from DB open; ListAccounts is slug-sorted.
	list, err := db.ListAccounts()
	if err != nil || len(list) != 3 ||
		list[0].Slug != "acme" || list[1].Slug != "beta" || list[2].Slug != "default" {
		t.Fatalf("ListAccounts: %v %v", list, err)
	}
}

func TestUserAccountsAndMembers(t *testing.T) {
	db := openTest(t)
	org, err := db.EnsureAccount("acme", "org")
	if err != nil {
		t.Fatal(err)
	}
	// CreateUser also creates the personal account with the user as owner.
	u, err := db.CreateUser("alice", "a@x.test", "hash", "user")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	accs, err := db.UserAccounts(u.ID)
	if err != nil || len(accs) != 1 || accs[0].Slug != "alice" || accs[0].Kind != "user" {
		t.Fatalf("UserAccounts personal: %v %v", accs, err)
	}

	if err := db.SetMember(org.ID, u.ID, "admin"); err != nil {
		t.Fatalf("SetMember: %v", err)
	}
	accs, err = db.UserAccounts(u.ID)
	if err != nil || len(accs) != 2 || accs[0].Slug != "acme" || accs[1].Slug != "alice" {
		t.Fatalf("UserAccounts after join: %v %v", accs, err)
	}

	ms, err := db.ListMembers(org.ID)
	if err != nil || len(ms) != 1 || ms[0].UserName != "alice" || ms[0].Role != "admin" ||
		ms[0].AccountID != org.ID || ms[0].UserID != u.ID {
		t.Fatalf("ListMembers: %+v %v", ms, err)
	}
	if ms, err := db.ListMembers(99999); err != nil || len(ms) != 0 {
		t.Fatalf("ListMembers empty: %v %v", ms, err)
	}
}

func TestListAccountCachesAndStorage(t *testing.T) {
	db := openTest(t)
	c1, err := db.CreateCache("acme", "one", true, 40)
	if err != nil {
		t.Fatal(err)
	}
	db.CreateCache("acme", "two", false, 40)
	db.CreateCache("other", "elsewhere", true, 40)

	if c1.Ref() != "acme/one" {
		t.Fatalf("Ref = %q", c1.Ref())
	}

	list, err := db.ListAccountCaches(c1.AccountID)
	if err != nil || len(list) != 2 || list[0].Name != "one" || list[1].Name != "two" {
		t.Fatalf("ListAccountCaches: %v %v", list, err)
	}
	if list, err := db.ListAccountCaches(99999); err != nil || len(list) != 0 {
		t.Fatalf("ListAccountCaches empty: %v %v", list, err)
	}

	if err := db.SetCacheStorage(c1.ID, "bulk"); err != nil {
		t.Fatalf("SetCacheStorage: %v", err)
	}
	got, err := db.GetCacheByID(c1.ID)
	if err != nil || got.Storage != "bulk" {
		t.Fatalf("storage not applied: %+v %v", got, err)
	}
}

func TestPresentSet(t *testing.T) {
	db := openTest(t)
	db.PutChunk("default", "h1", 1, 1, "k1", 1)
	db.PutChunk("default", "h2", 1, 1, "k2", 1)

	got, err := db.presentSet("chunks", "hash", []string{"h1", "h2", "hx"})
	if err != nil || len(got) != 2 || !got["h1"] || !got["h2"] || got["hx"] {
		t.Fatalf("presentSet: %v %v", got, err)
	}
	if got, err := db.presentSet("chunks", "hash", nil); err != nil || len(got) != 0 {
		t.Fatalf("presentSet empty: %v %v", got, err)
	}
	// Bad table name surfaces the query error.
	if _, err := db.presentSet("no_such_table", "hash", []string{"h1"}); err == nil {
		t.Fatal("presentSet on missing table should error")
	}
}
