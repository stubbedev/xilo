package store

import (
	"database/sql"
	"errors"
	"time"
)

// Account is the tenancy unit and the first URL segment under /c/: a personal
// account (kind "user", slug == the username) or an organization (kind
// "org"). Caches belong to an account; tokens can be scoped to one; users
// join orgs as admin or user.
type Account struct {
	ID      int64
	Slug    string
	Kind    string // "user" | "org"
	PlanID  int64  // 0 = no plan (unlimited)
	Created int64
}

// AccountMember links a user to an account. Admins manage the account's
// caches and tokens; members get visibility. A personal account has exactly
// its user as admin.
type AccountMember struct {
	AccountID int64
	UserID    int64
	UserName  string
	Role      string // "owner" (creator, fixed) | "admin" | "user"
}

// ValidSlug reports whether s can name an account (or user — one pool).
// Caches mount under /c/, so slugs never collide with top-level routes and
// no denylist is needed.
func ValidSlug(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		ok := r == '-' || r == '_' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if !ok {
			return false
		}
	}
	return true
}

// EnsureAccount returns the account with the given slug, creating it (with
// the given kind) if missing.
func (db *DB) EnsureAccount(slug, kind string) (*Account, error) {
	if a, err := db.GetAccount(slug); err == nil {
		return a, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	a := &Account{Slug: slug, Kind: kind, Created: time.Now().Unix()}
	err := db.write(func(tx *sql.Tx) error {
		// Concurrent creators race benignly: ON CONFLICT keeps the winner.
		if _, err := tx.Exec(`INSERT INTO accounts (slug, kind, created) VALUES (?,?,?) ON CONFLICT (slug) DO NOTHING`,
			a.Slug, a.Kind, a.Created); err != nil {
			return err
		}
		return tx.QueryRow(`SELECT id, kind, plan_id, created FROM accounts WHERE slug=?`, slug).
			Scan(&a.ID, &a.Kind, &a.PlanID, &a.Created)
	})
	if err != nil {
		return nil, err
	}
	return a, nil
}

func (db *DB) GetAccount(slug string) (*Account, error) {
	var a Account
	err := db.r.QueryRow(`SELECT id, slug, kind, plan_id, created FROM accounts WHERE slug=?`, slug).
		Scan(&a.ID, &a.Slug, &a.Kind, &a.PlanID, &a.Created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &a, err
}

func (db *DB) GetAccountByID(id int64) (*Account, error) {
	var a Account
	err := db.r.QueryRow(`SELECT id, slug, kind, plan_id, created FROM accounts WHERE id=?`, id).
		Scan(&a.ID, &a.Slug, &a.Kind, &a.PlanID, &a.Created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &a, err
}

// SetAccountPlan assigns a plan (0 = none/unlimited).
func (db *DB) SetAccountPlan(id, planID int64) error {
	return db.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE accounts SET plan_id=? WHERE id=?`, planID, id)
		return err
	})
}

func (db *DB) ListAccounts() ([]Account, error) {
	return db.listAccounts(`SELECT id, slug, kind, plan_id, created FROM accounts ORDER BY slug`)
}

// UserAccounts lists the accounts a user belongs to.
func (db *DB) UserAccounts(userID int64) ([]Account, error) {
	return db.listAccounts(`SELECT a.id, a.slug, a.kind, a.plan_id, a.created FROM accounts a
		JOIN account_members m ON m.account_id = a.id WHERE m.user_id=? ORDER BY a.slug`, userID)
}

func (db *DB) listAccounts(q string, args ...any) ([]Account, error) {
	rows, err := db.r.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		var a Account
		if err := rows.Scan(&a.ID, &a.Slug, &a.Kind, &a.PlanID, &a.Created); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeleteOrg removes an organization and everything it holds: memberships,
// tokens, caches and their paths (FK cascade), and its egress ledger.
// Personal accounts are refused — they die with their user. Chunk blobs are
// NOT touched here: dedup means a chunk may back other accounts' paths, so
// only the GC mark-sweep decides what actually leaves disk.
func (db *DB) DeleteOrg(id int64) error {
	return db.write(func(tx *sql.Tx) error {
		var kind string
		err := tx.QueryRow(`SELECT kind FROM accounts WHERE id=?`, id).Scan(&kind)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if kind != "org" {
			return errors.New("only organizations can be deleted")
		}
		for _, q := range []string{
			`DELETE FROM account_members WHERE account_id=?`,
			`DELETE FROM tokens WHERE account_id=?`,
			`DELETE FROM caches WHERE account_id=?`,
			`DELETE FROM account_egress WHERE account_id=?`,
			`DELETE FROM accounts WHERE id=?`,
		} {
			if _, err := tx.Exec(q, id); err != nil {
				return err
			}
		}
		return nil
	})
}

// SetMember adds a user to an ORG account or updates their role. Only
// "admin" and "user" are grantable; the owner (the org's original creator)
// can never be granted, changed, or displaced — that rules out privilege
// escalation and ownerless orgs by construction. Personal accounts have
// exactly their owner — extra members are refused here, not just hidden in
// the UI.
func (db *DB) SetMember(accountID, userID int64, role string) error {
	if role != "admin" && role != "user" {
		return errors.New("grantable roles are admin and user")
	}
	return db.write(func(tx *sql.Tx) error {
		var kind string
		if err := tx.QueryRow(`SELECT kind FROM accounts WHERE id=?`, accountID).Scan(&kind); err != nil {
			return err
		}
		var cur string
		isMember := tx.QueryRow(`SELECT role FROM account_members WHERE account_id=? AND user_id=?`, accountID, userID).Scan(&cur) == nil
		if isMember && cur == "owner" {
			return errors.New("the owner's role cannot be changed")
		}
		if kind != "org" && !isMember {
			return errors.New("personal accounts cannot have additional members")
		}
		_, err := tx.Exec(`INSERT INTO account_members (account_id, user_id, role) VALUES (?,?,?)
			 ON CONFLICT (account_id, user_id) DO UPDATE SET role=excluded.role`, accountID, userID, role)
		return err
	})
}

// MakeOwner records the account's owner — its original creator. Exactly one
// per account, set at creation time, never transferable.
func (db *DB) MakeOwner(accountID, userID int64) error {
	return db.write(func(tx *sql.Tx) error {
		var one int
		if tx.QueryRow(`SELECT 1 FROM account_members WHERE account_id=? AND role='owner'`, accountID).Scan(&one) == nil {
			return errors.New("account already has an owner")
		}
		_, err := tx.Exec(`INSERT INTO account_members (account_id, user_id, role) VALUES (?,?,'owner')
			 ON CONFLICT (account_id, user_id) DO UPDATE SET role='owner'`, accountID, userID)
		return err
	})
}

// RemoveMember drops a user from an account. The owner cannot be removed —
// an org keeps its creator for its lifetime.
func (db *DB) RemoveMember(accountID, userID int64) error {
	return db.write(func(tx *sql.Tx) error {
		var cur string
		if tx.QueryRow(`SELECT role FROM account_members WHERE account_id=? AND user_id=?`, accountID, userID).Scan(&cur) == nil && cur == "owner" {
			return errors.New("the owner cannot be removed")
		}
		_, err := tx.Exec(`DELETE FROM account_members WHERE account_id=? AND user_id=?`, accountID, userID)
		return err
	})
}

// OwnsOrgs reports whether the user is the owner of any organization —
// such a user cannot be deleted until their orgs are gone.
func (db *DB) OwnsOrgs(userID int64) bool {
	var one int
	return db.r.QueryRow(`SELECT 1 FROM account_members m JOIN accounts a ON a.id = m.account_id
		WHERE m.user_id=? AND m.role='owner' AND a.kind='org'`, userID).Scan(&one) == nil
}

// ListMembers returns an account's members with usernames.
func (db *DB) ListMembers(accountID int64) ([]AccountMember, error) {
	rows, err := db.r.Query(`SELECT m.account_id, m.user_id, u.username, m.role
		FROM account_members m JOIN users u ON u.id = m.user_id
		WHERE m.account_id=? ORDER BY u.username`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AccountMember
	for rows.Next() {
		var m AccountMember
		if err := rows.Scan(&m.AccountID, &m.UserID, &m.UserName, &m.Role); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MemberRole returns the user's role in an account ("" if not a member).
func (db *DB) MemberRole(accountID, userID int64) string {
	var role string
	if err := db.r.QueryRow(`SELECT role FROM account_members WHERE account_id=? AND user_id=?`,
		accountID, userID).Scan(&role); err != nil {
		return ""
	}
	return role
}
