package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// User is a dashboard account. Role "owner" manages everything and is held
// only by the bootstrap account — it is never granted, demoted, or deleted;
// "user" can
// sign in and manage their own account (namespace membership scopes what they
// see — added with namespaces).
type User struct {
	ID          int64
	Name        string
	Email       string // optional; unique when set; usable for sign-in
	PassHash    string
	Role        string // "owner" | "user"
	Status      string // "active" | "pending" (awaiting approval)
	TOTPEnabled bool
	Created     int64
}

const userCols = `id,username,COALESCE(email,''),password_hash,role,status,totp_enabled,created`

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var u User
	var totp int
	if err := row.Scan(&u.ID, &u.Name, &u.Email, &u.PassHash, &u.Role, &u.Status, &totp, &u.Created); err != nil {
		return nil, err
	}
	u.TOTPEnabled = totp != 0
	return &u, nil
}

// CreateUser inserts a dashboard user with a bcrypt password hash, plus their
// personal account (kind "user", slug == username) with the user as its
// admin. Usernames and account slugs share one global pool.
func (db *DB) CreateUser(name, email, passHash, role string) (*User, error) {
	return db.createUser(name, email, passHash, role, "active")
}

// CreatePendingUser is CreateUser for self-registration awaiting approval.
func (db *DB) CreatePendingUser(name, email, passHash string) (*User, error) {
	return db.createUser(name, email, passHash, "user", "pending")
}

func (db *DB) createUser(name, email, passHash, role, status string) (*User, error) {
	if !ValidSlug(name) {
		return nil, errors.New("invalid username (lowercase letters, digits, - and _)")
	}
	u := &User{Name: name, Email: email, PassHash: passHash, Role: role, Status: status, Created: time.Now().Unix()}
	err := db.write(func(tx *sql.Tx) error {
		var taken int
		if err := tx.QueryRow(`SELECT 1 FROM accounts WHERE slug=?`, name).Scan(&taken); err == nil {
			return errors.New("name already taken")
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var email any
		if u.Email != "" {
			email = u.Email
		}
		if err := tx.QueryRow(
			`INSERT INTO users (username,email,password_hash,role,status,created) VALUES (?,?,?,?,?,?) RETURNING id`,
			u.Name, email, u.PassHash, u.Role, u.Status, u.Created).Scan(&u.ID); err != nil {
			return err
		}
		var accID int64
		if err := tx.QueryRow(`INSERT INTO accounts (slug, kind, created) VALUES (?,?,?) RETURNING id`,
			u.Name, "user", u.Created).Scan(&accID); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO account_members (account_id, user_id, role) VALUES (?,?,'owner')`, accID, u.ID)
		return err
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

// GetUserByLogin resolves a sign-in identifier: username, or email when it
// contains an @.
func (db *DB) GetUserByLogin(login string) (*User, error) {
	if strings.Contains(login, "@") {
		u, err := scanUser(db.r.QueryRow(`SELECT `+userCols+` FROM users WHERE email=?`, login))
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return u, err
	}
	return db.GetUserByName(login)
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

func (db *DB) SetUserPassword(id int64, passHash string) error {
	return db.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE users SET password_hash=? WHERE id=?`, passHash, id)
		return err
	})
}

// SetUserStatus flips approval state ("active" | "pending").
func (db *DB) SetUserStatus(id int64, status string) error {
	return db.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE users SET status=? WHERE id=?`, status, id)
		return err
	})
}

// SetUserEmail updates the sign-in alias ("" clears it).
func (db *DB) SetUserEmail(id int64, email string) error {
	return db.write(func(tx *sql.Tx) error {
		var v any
		if email != "" {
			v = email
		}
		_, err := tx.Exec(`UPDATE users SET email=? WHERE id=?`, v, id)
		return err
	})
}

// DeleteUser removes a user along with their passkeys, sessions and org
// memberships. Their personal account is removed only when empty; with caches
// it stays (orphaned, super-admin managed) rather than cascading data away.
func (db *DB) DeleteUser(id int64) error {
	return db.write(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM passkeys WHERE user_id=?`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM sessions WHERE user_id=?`, id); err != nil {
			return err
		}
		var name string
		if err := tx.QueryRow(`SELECT username FROM users WHERE id=?`, id).Scan(&name); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM account_members WHERE user_id=?`, id); err != nil {
			return err
		}
		// Credentials die with the user: every token of their personal account
		// is revoked (org tokens belong to the org, not to its members).
		if _, err := tx.Exec(`UPDATE tokens SET revoked=1 WHERE account_id IN
			(SELECT id FROM accounts WHERE slug=? AND kind='user')`, name); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM accounts WHERE slug=? AND kind='user'
			AND NOT EXISTS (SELECT 1 FROM caches WHERE account_id = accounts.id)`, name); err != nil {
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
	if err != nil {
		return nil, false, err
	}
	secret, err = db.unseal(secret)
	return secret, e != 0, err
}

// SetUserTOTPSecret stores a freshly-generated secret (not yet enabled).
func (db *DB) SetUserTOTPSecret(id int64, secret []byte) error {
	sealed, err := db.seal(secret)
	if err != nil {
		return err
	}
	return db.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE users SET totp_secret=?, totp_enabled=0 WHERE id=?`, sealed, id)
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
