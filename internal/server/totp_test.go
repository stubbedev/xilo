package server

import (
	"testing"
	"time"
)

func TestTOTPVerify(t *testing.T) {
	secret, err := newTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0)
	code := totpCode(secret, now)

	if !totpVerify(secret, code, now) {
		t.Fatal("current code should verify")
	}
	// within skew (±1 step of 30s)
	if !totpVerify(secret, code, now.Add(25*time.Second)) {
		t.Error("code should verify within skew window")
	}
	// far outside → reject
	if totpVerify(secret, code, now.Add(5*time.Minute)) {
		t.Error("stale code should not verify")
	}
	// wrong code → reject
	if totpVerify(secret, "000000", now) && code != "000000" {
		t.Error("wrong code should not verify")
	}
	// malformed
	if totpVerify(secret, "12", now) {
		t.Error("short code should not verify")
	}
}

func TestTOTPURIAndSecret(t *testing.T) {
	secret := []byte("12345678901234567890")
	uri := totpURI(secret, "xilo", "cache.example.com")
	if uri == "" || secretB32(secret) == "" {
		t.Fatal("empty totp uri/secret")
	}
	if _, err := totpQRDataURI(uri); err != nil {
		t.Fatalf("qr encode: %v", err)
	}
}
