package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

var errSealed = errors.New("cannot decrypt: database.salt differs from the salt that encrypted this database")

// SetSalt installs the instance salt: sensitive columns (cache signing keys,
// TOTP secrets) are encrypted at rest with a key derived from it, and token
// hashes are keyed with it. Call right after Open, before any reads or
// writes. The salt must stay stable for the lifetime of the database — a
// different salt cannot decrypt existing rows and invalidates every issued
// token. Empty salt = plaintext columns and unsalted token hashes.
func (db *DB) SetSalt(salt string) {
	if salt == "" {
		return
	}
	k := sha256.Sum256([]byte("xilo-db-key:" + salt))
	db.key = k[:]
}

// seal encrypts b with AES-256-GCM (random nonce prefixed). Pass-through when
// no salt is set; nil stays nil so NULL columns survive.
func (db *DB) seal(b []byte) ([]byte, error) {
	if db.key == nil || b == nil {
		return b, nil
	}
	block, err := aes.NewCipher(db.key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, b, nil), nil
}

// unseal reverses seal. Fails when the salt differs from the one that sealed.
func (db *DB) unseal(b []byte) ([]byte, error) {
	if db.key == nil || b == nil {
		return b, nil
	}
	block, err := aes.NewCipher(db.key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(b) < gcm.NonceSize() {
		return nil, errSealed
	}
	out, err := gcm.Open(nil, b[:gcm.NonceSize()], b[gcm.NonceSize():], nil)
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
