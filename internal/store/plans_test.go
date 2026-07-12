package store

import (
	"database/sql"
	"errors"
	"testing"
)

func TestPlanCRUD(t *testing.T) {
	db := openTest(t)

	p, err := db.CreatePlan(&Plan{
		Name: "pro", MaxCaches: 5, MaxMembers: 3, MaxStorage: 1 << 30,
		MaxRetention: 3600, OrgsAllowed: true, Public: true,
	})
	if err != nil || p.ID == 0 {
		t.Fatalf("CreatePlan: %+v %v", p, err)
	}
	// Free plan, not public.
	free, err := db.CreatePlan(&Plan{Name: "free"})
	if err != nil {
		t.Fatal(err)
	}

	got, err := db.GetPlan(p.ID)
	if err != nil || got.Name != "pro" || got.MaxCaches != 5 || !got.OrgsAllowed || !got.Public {
		t.Fatalf("GetPlan: %+v %v", got, err)
	}
	if _, err := db.GetPlan(0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetPlan(0) = %v, want ErrNotFound", err)
	}
	if _, err := db.GetPlan(99999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetPlan(missing) = %v, want ErrNotFound", err)
	}

	p.Name = "pro-plus"
	p.MaxCaches = 10
	if err := db.UpdatePlan(p); err != nil {
		t.Fatalf("UpdatePlan: %v", err)
	}
	if got, _ := db.GetPlan(p.ID); got.Name != "pro-plus" || got.MaxCaches != 10 {
		t.Fatalf("update not applied: %+v", got)
	}

	// ListPlans is name-sorted; PublicPlans filters.
	all, err := db.ListPlans()
	if err != nil || len(all) != 2 || all[0].Name != "free" || all[1].Name != "pro-plus" {
		t.Fatalf("ListPlans: %v %v", all, err)
	}
	pub, err := db.PublicPlans()
	if err != nil || len(pub) != 1 || pub[0].Name != "pro-plus" {
		t.Fatalf("PublicPlans: %v %v", pub, err)
	}

	if err := db.DeletePlan(free.ID); err != nil {
		t.Fatalf("DeletePlan: %v", err)
	}
	if _, err := db.GetPlan(free.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("plan survived delete: %v", err)
	}
}

func TestDeletePlanInUse(t *testing.T) {
	db := openTest(t)
	p, err := db.CreatePlan(&Plan{Name: "pro"})
	if err != nil {
		t.Fatal(err)
	}
	a, err := db.EnsureAccount("acme", "org")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetAccountPlan(a.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.DeletePlan(p.ID); err == nil {
		t.Fatal("DeletePlan should refuse a plan still in use")
	}
}

func TestAccountPlanResolution(t *testing.T) {
	db := openTest(t)
	a, err := db.EnsureAccount("acme", "org")
	if err != nil {
		t.Fatal(err)
	}
	// No plan → (nil, nil) = unlimited.
	if pl, err := db.AccountPlan(a); err != nil || pl != nil {
		t.Fatalf("no-plan account: %+v %v", pl, err)
	}
	if pl, err := db.AccountPlan(nil); err != nil || pl != nil {
		t.Fatalf("nil account: %+v %v", pl, err)
	}

	p, _ := db.CreatePlan(&Plan{Name: "pro", MaxCaches: 2})
	if err := db.SetAccountPlan(a.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	a, _ = db.GetAccount("acme")
	if pl, err := db.AccountPlan(a); err != nil || pl == nil || pl.MaxCaches != 2 {
		t.Fatalf("planned account: %+v %v", pl, err)
	}

	// Plan deleted out from under the account → treated as unlimited.
	if err := db.write(func(tx *sql.Tx) error { _, e := tx.Exec(`DELETE FROM plans WHERE id=?`, p.ID); return e }); err != nil {
		t.Fatal(err)
	}
	if pl, err := db.AccountPlan(a); err != nil || pl != nil {
		t.Fatalf("dangling plan should be unlimited: %+v %v", pl, err)
	}
}

func TestAccountLogicalBytesAndEgress(t *testing.T) {
	db := openTest(t)
	c, err := db.CreateCache("acme", "one", true, 40)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := db.AccountLogicalBytes(c.AccountID); err != nil || n != 0 {
		t.Fatalf("empty logical bytes: %d %v", n, err)
	}

	// Register a chunk + path so nar_size is summed.
	h := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := db.PutChunk("default", "h1", 100, 50, "k1", 1); err != nil {
		t.Fatal(err)
	}
	if err := db.PutPath(c.ID, h, &Path{
		StorePath: "/nix/store/" + h + "-n", NarHash: "sha256:x",
		NarSize: 4096, Chunks: []string{"h1"},
	}); err != nil {
		t.Fatalf("PutPath: %v", err)
	}
	if n, err := db.AccountLogicalBytes(c.AccountID); err != nil || n != 4096 {
		t.Fatalf("logical bytes: %d %v", n, err)
	}

	// Egress rollup upserts and accumulates per month.
	if v := db.AccountEgress(c.AccountID, "2026-07"); v != 0 {
		t.Fatalf("egress start: %d", v)
	}
	if err := db.AddEgress(c.AccountID, "2026-07", 1000); err != nil {
		t.Fatal(err)
	}
	if err := db.AddEgress(c.AccountID, "2026-07", 500); err != nil {
		t.Fatal(err)
	}
	if v := db.AccountEgress(c.AccountID, "2026-07"); v != 1500 {
		t.Fatalf("egress accumulate: %d want 1500", v)
	}
	if v := db.AccountEgress(c.AccountID, "2026-08"); v != 0 {
		t.Fatalf("egress other month: %d", v)
	}
}

func TestSettings(t *testing.T) {
	db := openTest(t)
	if v := db.Setting("nope"); v != "" {
		t.Fatalf("unset setting = %q", v)
	}
	if !db.SettingBool("allow_registrations", true) {
		t.Fatal("default true not honored")
	}
	if db.SettingBool("allow_registrations", false) {
		t.Fatal("default false not honored")
	}
	if err := db.SetSetting("allow_registrations", "1"); err != nil {
		t.Fatal(err)
	}
	if !db.SettingBool("allow_registrations", false) {
		t.Fatal("stored 1 should read true")
	}
	if err := db.SetSetting("allow_registrations", "0"); err != nil {
		t.Fatal(err)
	}
	if db.SettingBool("allow_registrations", true) {
		t.Fatal("stored 0 should read false")
	}
	if v := db.Setting("allow_registrations"); v != "0" {
		t.Fatalf("Setting = %q want 0", v)
	}
}

func TestUserStatusEmailAndLogin(t *testing.T) {
	db := openTest(t)
	// Pending user (self-registration awaiting approval).
	u, err := db.CreatePendingUser("alice", "alice@x.test", "hash")
	if err != nil {
		t.Fatalf("CreatePendingUser: %v", err)
	}
	if got, _ := db.GetUser(u.ID); got.Status != "pending" {
		t.Fatalf("status = %q want pending", got.Status)
	}

	// Login by username and by email both resolve.
	if got, err := db.GetUserByLogin("alice"); err != nil || got.ID != u.ID {
		t.Fatalf("login by name: %+v %v", got, err)
	}
	if got, err := db.GetUserByLogin("alice@x.test"); err != nil || got.ID != u.ID {
		t.Fatalf("login by email: %+v %v", got, err)
	}
	if _, err := db.GetUserByLogin("ghost@x.test"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing email login = %v want ErrNotFound", err)
	}

	if err := db.SetUserStatus(u.ID, "active"); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.GetUser(u.ID); got.Status != "active" {
		t.Fatalf("status not flipped: %q", got.Status)
	}

	if err := db.SetUserEmail(u.ID, "new@x.test"); err != nil {
		t.Fatal(err)
	}
	if got, err := db.GetUserByLogin("new@x.test"); err != nil || got.ID != u.ID {
		t.Fatalf("email change: %+v %v", got, err)
	}
	// Clearing the email removes the sign-in alias.
	if err := db.SetUserEmail(u.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetUserByLogin("new@x.test"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cleared email still resolves: %v", err)
	}
}

func TestAuthorizeAdminAndNS(t *testing.T) {
	db := openTest(t)
	now := int64(1000)
	org, err := db.EnsureAccount("team", "org")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateCache("team", "cache", true, 40); err != nil {
		t.Fatal(err)
	}

	// Admin-only token: passes AuthorizeAdmin and (via admin) AuthorizeNS,
	// but grants no pull/push.
	adminSec, _, err := db.CreateToken(0, "adm", nil, []string{"admin"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !db.AuthorizeAdmin(adminSec, now) {
		t.Fatal("admin token should AuthorizeAdmin")
	}
	if !db.AuthorizeNS(adminSec, "team", "cache", "configure", now) {
		t.Fatal("admin token should pass AuthorizeNS")
	}
	if db.Authorize(adminSec, "team", "cache", "pull", now) {
		t.Fatal("admin token must not grant pull")
	}

	// Scoped management token: configure on its own cache only.
	nsSec, _, err := db.CreateToken(org.ID, "cfg", []string{"cache"}, []string{"configure"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if db.AuthorizeAdmin(nsSec, now) {
		t.Fatal("scoped token must not AuthorizeAdmin")
	}
	if !db.AuthorizeNS(nsSec, "team", "cache", "configure", now) {
		t.Fatal("scoped token should manage its own cache")
	}
	if db.AuthorizeNS(nsSec, "team", "cache", "destroy", now) {
		t.Fatal("scoped token lacks destroy perm")
	}
	if db.AuthorizeNS(nsSec, "default", "cache", "configure", now) {
		t.Fatal("scoped token must not cross accounts")
	}
	if db.AuthorizeAdmin("garbage-secret", now) {
		t.Fatal("unknown secret must not AuthorizeAdmin")
	}
	if db.AuthorizeNS("garbage-secret", "team", "cache", "configure", now) {
		t.Fatal("unknown secret must not AuthorizeNS")
	}
}
