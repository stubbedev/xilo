package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Account is the tenancy unit and the first URL segment under /c/. Every
// account is an organization: caches belong to one, tokens scope to one, a
// plan and its usage attach to one, and the only way to reach any of it is a
// membership. A single person's workspace is an organization with one member —
// created for them at signup and named after them — so there is one answer to
// "who owns this cache" and one thing to bill.
//
// Kind is provenance now, not permission: nothing branches on it. The
// distinguishing question is whether an account is a live user's own
// workspace, which is Slug == that user's username.
type Account struct {
	ID      int64
	Slug    string
	Kind    string // "org"; historic "user" rows are migrated on open
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

// ErrSlugReserved means the slug once belonged to a user and must not be
// adopted by a new account (see EnsureAccount).
var ErrSlugReserved = errors.New("account name is reserved")

// Membership refusals the admin UI turns into flashes. They are sentinels,
// not sentences: internal/server maps each to an i18n key (flashStore), so
// the wording a user reads lives in one catalog instead of down here. The
// text stays as the CLI and log rendering.
var (
	ErrOwnWorkspace = errors.New("a user's own workspace goes with the user")
	ErrBadRole      = errors.New("grantable roles are admin and user")
	ErrOwnerRole    = errors.New("the owner's role cannot be changed")
	ErrHasOwner     = errors.New("account already has an owner")
	ErrOwnerLocked  = errors.New("the owner cannot be removed")
)

// EnsureAccount returns the account with the given slug, creating it (with
// the given kind) if missing.
//
// A soft-deleted row keeps its slug and id (for audit refs). Reactivating one
// in place is only safe when nothing was left behind in it: DeleteOrg purges
// an organization's caches and tokens, so a deleted org can be recreated
// cleanly, but DeleteUser deliberately LEAVES the caches of that user's own
// workspace in place. Adopting such a slug would silently re-parent those
// private caches to whoever took the name — a cross-tenant takeover — so the
// username of any user that ever existed is reserved for good.
//
// This used to be a comparison of account kinds, which stopped meaning
// anything once every account became an organization; the question was never
// really about kind, it was about whether a user's remains are under the slug.
func (db *DB) EnsureAccount(slug, kind string) (*Account, error) {
	if a, err := db.GetAccount(slug); err == nil {
		return a, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	a := &Account{Slug: slug, Kind: kind, Created: time.Now().Unix()}
	err := db.write(func(tx *sql.Tx) error {
		var curKind, curStatus string
		switch err := tx.QueryRow(`SELECT kind, status FROM accounts WHERE slug=?`, slug).
			Scan(&curKind, &curStatus); {
		case errors.Is(err, sql.ErrNoRows):
			// No row — fall through to a fresh insert.
		case err != nil:
			return err
		case curStatus == "deleted":
			var one int
			if tx.QueryRow(`SELECT 1 FROM users WHERE username=?`, slug).Scan(&one) == nil {
				return ErrSlugReserved
			}
		}
		// Concurrent creators race benignly: ON CONFLICT keeps the winner. The
		// WHERE reactivates only a soft-deleted row and is a no-op for a live
		// account; plan_id resets so a reactivated row does not inherit the
		// deleted account's plan.
		if _, err := tx.Exec(`INSERT INTO accounts (slug, kind, created) VALUES (?,?,?)
			ON CONFLICT (slug) DO UPDATE SET status='active', created=excluded.created, plan_id=0
			WHERE accounts.status='deleted'`,
			a.Slug, a.Kind, a.Created); err != nil {
			return err
		}
		return tx.QueryRow(`SELECT id, kind, plan_id, created FROM accounts WHERE slug=? AND status<>'deleted'`, slug).
			Scan(&a.ID, &a.Kind, &a.PlanID, &a.Created)
	})
	if err != nil {
		return nil, err
	}
	return a, nil
}

// GetAccount and GetAccountByID resolve only live accounts; a soft-deleted one
// reads as ErrNotFound everywhere in the app (its row survives solely to keep
// historical id references intact).
func (db *DB) GetAccount(slug string) (*Account, error) {
	var a Account
	err := db.r.QueryRow(`SELECT id, slug, kind, plan_id, created FROM accounts WHERE slug=? AND status<>'deleted'`, slug).
		Scan(&a.ID, &a.Slug, &a.Kind, &a.PlanID, &a.Created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &a, err
}

func (db *DB) GetAccountByID(id int64) (*Account, error) {
	var a Account
	err := db.r.QueryRow(`SELECT id, slug, kind, plan_id, created FROM accounts WHERE id=? AND status<>'deleted'`, id).
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

// ListAccounts and UserAccounts hide soft-deleted accounts — a deleted org
// must not surface in nav, token scoping, or the admin listings.
func (db *DB) ListAccounts() ([]Account, error) {
	return db.listAccounts(`SELECT id, slug, kind, plan_id, created FROM accounts WHERE status<>'deleted' ORDER BY slug`)
}

// UserAccounts lists the accounts a user belongs to.
func (db *DB) UserAccounts(userID int64) ([]Account, error) {
	return db.listAccounts(`SELECT a.id, a.slug, a.kind, a.plan_id, a.created FROM accounts a
		JOIN account_members m ON m.account_id = a.id WHERE m.user_id=? AND a.status<>'deleted' ORDER BY a.slug`, userID)
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

// DeleteOrg soft-deletes an organization. Everything it held is really gone —
// memberships, tokens, caches and their paths (FK cascade), egress ledger — so
// the caches stop serving; only the accounts row survives, flagged
// status='deleted', so audit-log references to it still resolve. Chunk blobs
// are NOT touched here: dedup means a chunk may back other accounts' paths, so
// only the GC mark-sweep decides what actually leaves disk.
//
// A live user's own workspace is refused: it is created with them and goes
// with them, which is what keeps "every user has somewhere to put a cache"
// true without a second rule enforcing it.
func (db *DB) DeleteOrg(id int64) error {
	return db.write(func(tx *sql.Tx) error {
		var slug string
		err := tx.QueryRow(`SELECT slug FROM accounts WHERE id=?`, id).Scan(&slug)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		var one int
		if tx.QueryRow(`SELECT 1 FROM users WHERE username=? AND status<>'deleted'`, slug).Scan(&one) == nil {
			return ErrOwnWorkspace
		}
		for _, q := range []string{
			`DELETE FROM account_members WHERE account_id=?`,
			`DELETE FROM tokens WHERE account_id=?`,
			`DELETE FROM caches WHERE account_id=?`,
			`DELETE FROM account_egress WHERE account_id=?`,
			`UPDATE accounts SET status='deleted' WHERE id=?`,
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
		return ErrBadRole
	}
	return db.write(func(tx *sql.Tx) error {
		var cur string
		isMember := tx.QueryRow(`SELECT role FROM account_members WHERE account_id=? AND user_id=?`, accountID, userID).Scan(&cur) == nil
		if isMember && cur == "owner" {
			return ErrOwnerRole
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
			return ErrHasOwner
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
			return ErrOwnerLocked
		}
		_, err := tx.Exec(`DELETE FROM account_members WHERE account_id=? AND user_id=?`, accountID, userID)
		return err
	})
}

// OwnsOrgs reports whether the user owns an organization other than their own
// workspace — deleting them would orphan something other people are in, so it
// is refused until those are gone. Their own workspace is excluded: it is
// named after them and goes with them, so counting it would make every user
// undeletable.
func (db *DB) OwnsOrgs(userID int64) bool {
	var one int
	return db.r.QueryRow(`SELECT 1 FROM account_members m
		JOIN accounts a ON a.id = m.account_id
		JOIN users u ON u.id = m.user_id
		WHERE m.user_id=? AND m.role='owner' AND a.status<>'deleted' AND a.slug <> u.username`, userID).Scan(&one) == nil
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

// ResolveAccount picks the account an unqualified cache name belongs to.
//
// There is deliberately no account named "default": inventing one meant
// `xilo cache create mycache` silently created an *organisation* called
// "default" that then sat in the dashboard beside real accounts forever. When
// the instance has exactly one account the answer is unambiguous; otherwise
// the caller is told to write the name out in full.
func (db *DB) ResolveAccount() (string, error) {
	accs, err := db.ListAccounts()
	if err != nil {
		return "", err
	}
	switch len(accs) {
	case 0:
		return "", errors.New("no accounts exist yet — write the cache as <account>/<name> and it will be created")
	case 1:
		return accs[0].Slug, nil
	}
	names := make([]string, 0, len(accs))
	for _, a := range accs {
		names = append(names, a.Slug)
	}
	return "", fmt.Errorf("this instance has several accounts (%s) — write the cache as <account>/<name>",
		strings.Join(names, ", "))
}

// Account lifecycle states. A tenancy that stops paying is not deleted — its
// bytes are still on disk and its owner may well come back — so it degrades in
// two steps instead: read-only first, then closed. "deleted" is the end of the
// line and is already what the soft-delete writes.
const (
	StatusActive    = "active"    // normal
	StatusPastDue   = "past_due"  // pull still works; nothing new may be pushed
	StatusSuspended = "suspended" // nothing serves at all
)

// ErrBadStatus rejects a status the lifecycle does not define, so a typo in a
// form cannot quietly park an account in a state nothing checks for.
var ErrBadStatus = errors.New("unknown account status")

// AccountStatus is an account's lifecycle state, "" when there is no such
// account. Read on the binary-cache path, so it stays a single indexed lookup
// by primary key.
func (db *DB) AccountStatus(accountID int64) string {
	var status string
	if err := db.r.QueryRow(`SELECT status FROM accounts WHERE id=?`, accountID).Scan(&status); err != nil {
		return ""
	}
	return status
}

// SetAccountStatus moves an account through the lifecycle. Deletion is not
// reachable from here — that is DeleteOrg's job, which also purges what the
// account held; this only ever changes how much of it still answers.
func (db *DB) SetAccountStatus(accountID int64, status string) error {
	switch status {
	case StatusActive, StatusPastDue, StatusSuspended:
	default:
		return ErrBadStatus
	}
	return db.write(func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE accounts SET status=? WHERE id=? AND status<>'deleted'`, status, accountID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}
