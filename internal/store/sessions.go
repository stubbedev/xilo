package store

import (
	"database/sql"
	"time"
)

// Sessions are stored by SHA-256 hash of the cookie value, so reading the DB
// never yields a usable session id.

// CreateSession records a session hash with its expiry, pruning expired rows
// while it holds the writer anyway.
func (db *DB) CreateSession(idHash string, expires time.Time) error {
	return db.write(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM sessions WHERE expires < ?`, time.Now().Unix()); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO sessions (id, expires) VALUES (?, ?)`, idHash, expires.Unix())
		return err
	})
}

// SessionValid reports whether the session hash exists and is unexpired.
func (db *DB) SessionValid(idHash string) bool {
	var one int
	err := db.r.QueryRow(`SELECT 1 FROM sessions WHERE id=? AND expires >= ?`,
		idHash, time.Now().Unix()).Scan(&one)
	return err == nil
}

// DropSession removes a session (logout).
func (db *DB) DropSession(idHash string) error {
	return db.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM sessions WHERE id=?`, idHash)
		return err
	})
}
