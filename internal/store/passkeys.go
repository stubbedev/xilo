package store

import (
	"database/sql"
	"time"
)

// Passkey is one registered WebAuthn credential, owned by a user. Credential
// holds the go-webauthn credential as JSON — the store stays webauthn-agnostic.
type Passkey struct {
	ID         int64
	UserID     int64
	Name       string
	Credential []byte
	Created    int64
}

// AddPasskey stores a newly registered credential for a user.
func (db *DB) AddPasskey(userID int64, name string, credential []byte) error {
	return db.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO passkeys (user_id, name, credential, created) VALUES (?,?,?,?)`,
			userID, name, credential, time.Now().Unix())
		return err
	})
}

// UpdatePasskeyCredential rewrites a credential blob (sign counter updates).
func (db *DB) UpdatePasskeyCredential(id int64, credential []byte) error {
	return db.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE passkeys SET credential=? WHERE id=?`, credential, id)
		return err
	})
}

// ListPasskeys returns all registered passkeys, oldest first (passkey login
// matches the asserted credential against every user's keys).
func (db *DB) ListPasskeys() ([]Passkey, error) {
	return db.listPasskeys(`SELECT id, user_id, name, credential, created FROM passkeys ORDER BY id`)
}

// ListUserPasskeys returns one user's passkeys, oldest first.
func (db *DB) ListUserPasskeys(userID int64) ([]Passkey, error) {
	return db.listPasskeys(`SELECT id, user_id, name, credential, created FROM passkeys WHERE user_id=? ORDER BY id`, userID)
}

func (db *DB) listPasskeys(q string, args ...any) ([]Passkey, error) {
	rows, err := db.r.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Passkey
	for rows.Next() {
		var p Passkey
		if err := rows.Scan(&p.ID, &p.UserID, &p.Name, &p.Credential, &p.Created); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeletePasskey removes a credential by id, scoped to its owner.
func (db *DB) DeletePasskey(userID, id int64) error {
	return db.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM passkeys WHERE id=? AND user_id=?`, id, userID)
		return err
	})
}
