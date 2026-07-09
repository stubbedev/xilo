package store

import (
	"database/sql"
	"errors"
)

// AdminExists reports whether the singleton admin row is present.
func (db *DB) AdminExists() bool {
	var one int
	return db.r.QueryRow(`SELECT 1 FROM admin WHERE id=1`).Scan(&one) == nil
}

// BootstrapAdmin inserts the admin row with the given bcrypt hash if none
// exists yet. No-op once an admin exists.
func (db *DB) BootstrapAdmin(passwordHash string) error {
	return db.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT OR IGNORE INTO admin (id, password_hash) VALUES (1, ?)`, passwordHash)
		return err
	})
}

func (db *DB) AdminPasswordHash() (string, error) {
	var h string
	err := db.r.QueryRow(`SELECT password_hash FROM admin WHERE id=1`).Scan(&h)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return h, err
}

func (db *DB) SetAdminPassword(passwordHash string) error {
	return db.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE admin SET password_hash=? WHERE id=1`, passwordHash)
		return err
	})
}

// TOTP returns the enrolled secret (nil if none) and whether 2FA is enabled.
func (db *DB) TOTP() (secret []byte, enabled bool, err error) {
	var e int
	err = db.r.QueryRow(`SELECT totp_secret, totp_enabled FROM admin WHERE id=1`).Scan(&secret, &e)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, ErrNotFound
	}
	return secret, e != 0, err
}

// SetTOTPSecret stores a freshly-generated secret (enrollment); enabling stays
// separate so the admin must confirm a code first.
func (db *DB) SetTOTPSecret(secret []byte) error {
	return db.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE admin SET totp_secret=?, totp_enabled=0 WHERE id=1`, secret)
		return err
	})
}

// SetTOTPEnabled toggles 2FA. Disabling also clears the secret.
func (db *DB) SetTOTPEnabled(enabled bool) error {
	return db.write(func(tx *sql.Tx) error {
		if !enabled {
			_, err := tx.Exec(`UPDATE admin SET totp_enabled=0, totp_secret=NULL WHERE id=1`)
			return err
		}
		_, err := tx.Exec(`UPDATE admin SET totp_enabled=1 WHERE id=1`)
		return err
	})
}
