package store

import (
	"database/sql"
	"time"
)

// Passkey is one registered WebAuthn credential. Credential holds the
// go-webauthn credential as JSON — the store stays webauthn-agnostic.
type Passkey struct {
	ID         int64
	Name       string
	Credential []byte
	Created    int64
}

// AddPasskey stores a newly registered credential.
func (db *DB) AddPasskey(name string, credential []byte) error {
	return db.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO passkeys (name, credential, created) VALUES (?,?,?)`,
			name, credential, time.Now().Unix())
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

// ListPasskeys returns all registered passkeys, oldest first.
func (db *DB) ListPasskeys() ([]Passkey, error) {
	rows, err := db.r.Query(`SELECT id, name, credential, created FROM passkeys ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Passkey
	for rows.Next() {
		var p Passkey
		if err := rows.Scan(&p.ID, &p.Name, &p.Credential, &p.Created); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeletePasskey removes a credential by id.
func (db *DB) DeletePasskey(id int64) error {
	return db.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM passkeys WHERE id=?`, id)
		return err
	})
}
