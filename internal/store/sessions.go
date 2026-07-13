package store

import (
	"database/sql"
	"time"
)

// Sessions are stored by SHA-256 hash of the cookie value, so reading the DB
// never yields a usable session id.

// CreateSession records a session hash for a user with its expiry, pruning
// expired rows while it holds the writer anyway.
func (db *DB) CreateSession(idHash string, userID int64, expires time.Time) error {
	return db.write(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM sessions WHERE expires < ?`, time.Now().Unix()); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO sessions (id, user_id, expires) VALUES (?, ?, ?)`, idHash, userID, expires.Unix())
		return err
	})
}

// SessionUser returns the owning user id of a live session, or ok=false.
func (db *DB) SessionUser(idHash string) (userID int64, ok bool) {
	err := db.r.QueryRow(`SELECT user_id FROM sessions WHERE id=? AND expires >= ?`,
		idHash, time.Now().Unix()).Scan(&userID)
	return userID, err == nil
}

// DropSession removes a session (logout).
func (db *DB) DropSession(idHash string) error {
	return db.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM sessions WHERE id=?`, idHash)
		return err
	})
}

// DropUserSessions invalidates every session for a user. Called after a
// credential change (password reset, TOTP disable) so a stolen cookie can't
// outlive the change.
func (db *DB) DropUserSessions(userID int64) error {
	return db.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM sessions WHERE user_id=?`, userID)
		return err
	})
}
