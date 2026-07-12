package store

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"
)

// TestSaltRoundTrip pins the at-rest encryption contract: with a salt set,
// signing keys and TOTP secrets round-trip through the DB (and survive a
// reopen with the same salt), token auth works, and the raw column bytes are
// actually ciphertext. A different salt must refuse to decrypt.
func TestSaltRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetSalt("s3cret")

	c, err := db.CreateCache("acme", "web", true, 40)
	if err != nil {
		t.Fatal(err)
	}
	u, err := db.CreateUser("alice", "", "h", "user")
	if err != nil {
		t.Fatal(err)
	}
	totp := []byte("totp-seed-123")
	if err := db.SetUserTOTPSecret(u.ID, totp); err != nil {
		t.Fatal(err)
	}
	sec, tok, err := db.CreateToken(c.AccountID, "ci", []string{"web"}, []string{"pull"}, 0)
	if err != nil {
		t.Fatal(err)
	}

	// Raw columns must not contain the plaintext.
	var rawPriv, rawTOTP []byte
	if err := db.r.QueryRow(`SELECT privkey FROM caches WHERE id=?`, c.ID).Scan(&rawPriv); err != nil {
		t.Fatal(err)
	}
	if err := db.r.QueryRow(`SELECT totp_secret FROM users WHERE id=?`, u.ID).Scan(&rawTOTP); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(rawPriv, c.PrivKey) || bytes.Contains(rawTOTP, totp) {
		t.Fatal("sensitive columns stored in plaintext despite salt")
	}
	db.Close()

	// Same salt: everything decrypts and the token authorizes.
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetSalt("s3cret")
	got, err := db.GetCache("acme", "web")
	if err != nil || !bytes.Equal(got.PrivKey, c.PrivKey) {
		t.Fatalf("privkey round-trip: %v", err)
	}
	if s, _, err := db.UserTOTP(u.ID); err != nil || !bytes.Equal(s, totp) {
		t.Fatalf("totp round-trip: %v", err)
	}
	if !db.Authorize(sec, "acme", "web", "pull", time.Now().Unix()) {
		t.Fatalf("token %d should authorize under the same salt", tok.ID)
	}
	db.Close()

	// Different salt: reads fail, token hash no longer matches.
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetSalt("other")
	if _, err := db.GetCache("acme", "web"); err == nil {
		t.Fatal("privkey decrypted under the wrong salt")
	}
	if _, _, err := db.UserTOTP(u.ID); err == nil {
		t.Fatal("totp decrypted under the wrong salt")
	}
	if db.Authorize(sec, "acme", "web", "pull", time.Now().Unix()) {
		t.Fatal("token authorized under the wrong salt")
	}
}
