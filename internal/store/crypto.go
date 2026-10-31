package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

var errSealed = errors.New("cannot decrypt: database.salt differs from the salt that encrypted this database")

// SetSalt installs the instance salt: sensitive columns (cache signing keys,
// TOTP secrets) are encrypted at rest with a key derived from it, and token
// hashes are keyed with it. Call right after Open, before any reads or
// writes. The salt must stay stable for the lifetime of the database — a
// different salt cannot decrypt existing rows and invalidates every issued
// token. Empty salt = plaintext columns and unsalted token hashes.
func (db *DB) SetSalt(salt string) error {
	if salt == "" {
		return nil
	}
	k := sha256.Sum256([]byte("xilo-db-key:" + salt))
	block, err := aes.NewCipher(k[:])
	if err != nil {
		return fmt.Errorf("database salt cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return fmt.Errorf("database salt cipher: %w", err)
	}
	db.key, db.aead = k[:], aead
	return nil
}

// seal encrypts b with AES-256-GCM (random nonce prefixed). Pass-through when
// no salt is set; nil stays nil so NULL columns survive.
func (db *DB) seal(b []byte) ([]byte, error) {
	if db.aead == nil || b == nil {
		return b, nil
	}
	nonce := make([]byte, db.aead.NonceSize(), db.aead.NonceSize()+len(b)+db.aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return db.aead.Seal(nonce, nonce, b, nil), nil
}

// unseal reverses seal. Fails when the salt differs from the one that sealed.
func (db *DB) unseal(b []byte) ([]byte, error) {
	if db.aead == nil || b == nil {
		return b, nil
	}
	n := db.aead.NonceSize()
	if len(b) < n {
		return nil, errSealed
	}
	out, err := db.aead.Open(nil, b[:n], b[n:], nil)
	if err != nil {
		return nil, errSealed
	}
	return out, nil
}

// HashToken is the stored form of a secret token: HMAC-SHA256 under the
// instance salt, plain SHA-256 without one.
func (db *DB) HashToken(secret string) string {
	if db.key == nil {
		sum := sha256.Sum256([]byte(secret))
		return hex.EncodeToString(sum[:])
	}
	mac := hmac.New(sha256.New, db.key)
	mac.Write([]byte(secret))
	return hex.EncodeToString(mac.Sum(nil))
}
