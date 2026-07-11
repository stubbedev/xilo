package store

import (
	"database/sql"
	"errors"
	"time"
)

// User is a dashboard account. Role "admin" manages everything; "member" can
// sign in and manage their own account (namespace membership scopes what they
// see — added with namespaces).
type User struct {
	ID          int64
	Name        string
	PassHash    string
	Role        string // "admin" | "member"
	TOTPEnabled bool
	Created     int64
}

const userCols = `id,username,password_hash,role,totp_enabled,created`

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var u User
	var totp int
	if err := row.Scan(&u.ID, &u.Name, &u.PassHash, &u.Role, &totp, &u.Created); err != nil {
		return nil, err
	}
	u.TOTPEnabled = totp != 0
	return &u, nil
}

// CreateUser inserts a dashboard account with a bcrypt password hash.
func (db *DB) CreateUser(name, passHash, role string) (*User, error) {
	u := &User{Name: name, PassHash: passHash, Role: role, Created: time.Now().Unix()}
	err := db.write(func(tx *sql.Tx) error {
		return tx.QueryRow(
			`INSERT INTO users (username,password_hash,role,created) VALUES (?,?,?,?) RETURNING id`,
			u.Name, u.PassHash, u.Role, u.Created).Scan(&u.ID)
	})
	if err != nil {
		return nil, err
	}
	return u, nil
}

func (db *DB) GetUser(id int64) (*User, error) {
	u, err := scanUser(db.r.QueryRow(`SELECT `+userCols+` FROM users WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return u, err
}

func (db *DB) GetUserByName(name string) (*User, error) {
	u, err := scanUser(db.r.QueryRow(`SELECT `+userCols+` FROM users WHERE username=?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return u, err
}

func (db *DB) ListUsers() ([]User, error) {
	rows, err := db.r.Query(`SELECT ` + userCols + ` FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

// UsersExist reports whether any account exists (first-run detection).
func (db *DB) UsersExist() bool {
	var one int
	return db.r.QueryRow(`SELECT 1 FROM users LIMIT 1`).Scan(&one) == nil
}

// CountAdmins counts admin-role users — deleting or demoting the last one is
// refused at the handler layer.
func (db *DB) CountAdmins() (int, error) {
	var n int
	err := db.r.QueryRow(`SELECT COUNT(*) FROM users WHERE role='admin'`).Scan(&n)
	return n, err
}

func (db *DB) SetUserPassword(id int64, passHash string) error {
	return db.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE users SET password_hash=? WHERE id=?`, passHash, id)
		return err
	})
}

func (db *DB) SetUserRole(id int64, role string) error {
	return db.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE users SET role=? WHERE id=?`, role, id)
		return err
	})
}

// DeleteUser removes an account along with its passkeys and sessions.
func (db *DB) DeleteUser(id int64) error {
	return db.write(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM passkeys WHERE user_id=?`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM sessions WHERE user_id=?`, id); err != nil {
			return err
		}
		_, err := tx.Exec(`DELETE FROM users WHERE id=?`, id)
		return err
	})
}

// UserTOTP returns a user's enrolled secret (nil if none) and enabled state.
func (db *DB) UserTOTP(id int64) (secret []byte, enabled bool, err error) {
	var e int
	err = db.r.QueryRow(`SELECT totp_secret, totp_enabled FROM users WHERE id=?`, id).Scan(&secret, &e)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, ErrNotFound
	}
	return secret, e != 0, err
}

// SetUserTOTPSecret stores a freshly-generated secret (not yet enabled).
func (db *DB) SetUserTOTPSecret(id int64, secret []byte) error {
	return db.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE users SET totp_secret=?, totp_enabled=0 WHERE id=?`, secret, id)
		return err
	})
}

// SetUserTOTPEnabled toggles 2FA; disabling clears the secret.
func (db *DB) SetUserTOTPEnabled(id int64, enabled bool) error {
	return db.write(func(tx *sql.Tx) error {
		if !enabled {
			_, err := tx.Exec(`UPDATE users SET totp_enabled=0, totp_secret=NULL WHERE id=?`, id)
			return err
		}
		_, err := tx.Exec(`UPDATE users SET totp_enabled=1 WHERE id=?`, id)
		return err
	})
}
