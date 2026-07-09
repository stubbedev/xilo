package store

import "testing"

func TestAdminBootstrapAndPassword(t *testing.T) {
	db := openTest(t)
	if db.AdminExists() {
		t.Fatal("no admin should exist yet")
	}
	if err := db.BootstrapAdmin("hash-1"); err != nil {
		t.Fatal(err)
	}
	if !db.AdminExists() {
		t.Fatal("admin should exist after bootstrap")
	}
	// bootstrap again is a no-op (INSERT OR IGNORE) — must not overwrite.
	if err := db.BootstrapAdmin("hash-2"); err != nil {
		t.Fatal(err)
	}
	h, err := db.AdminPasswordHash()
	if err != nil || h != "hash-1" {
		t.Fatalf("hash=%q err=%v, want hash-1", h, err)
	}
	if err := db.SetAdminPassword("hash-3"); err != nil {
		t.Fatal(err)
	}
	if h, _ := db.AdminPasswordHash(); h != "hash-3" {
		t.Fatalf("password not updated: %q", h)
	}
}

func TestTOTPLifecycle(t *testing.T) {
	db := openTest(t)
	db.BootstrapAdmin("h")
	if _, enabled, _ := db.TOTP(); enabled {
		t.Fatal("totp should start disabled")
	}
	secret := []byte("0123456789abcdef0123")
	if err := db.SetTOTPSecret(secret); err != nil {
		t.Fatal(err)
	}
	got, enabled, _ := db.TOTP()
	if enabled || string(got) != string(secret) {
		t.Fatalf("after enroll: enabled=%v secret match=%v", enabled, string(got) == string(secret))
	}
	db.SetTOTPEnabled(true)
	if _, enabled, _ := db.TOTP(); !enabled {
		t.Fatal("totp should be enabled")
	}
	// disabling clears the secret
	db.SetTOTPEnabled(false)
	got, enabled, _ = db.TOTP()
	if enabled || len(got) != 0 {
		t.Fatalf("after disable: enabled=%v secretLen=%d", enabled, len(got))
	}
}
