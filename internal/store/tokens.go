package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"slices"
	"strings"
	"time"
)

// Perms carried by tokens. pull/push gate the cache protocol; the rest gate
// the management API (attic parity: create/configure/destroy per scope,
// admin = full instance control).
var ValidPerms = []string{"pull", "push", "create", "configure", "destroy", "admin"}

type Token struct {
	ID        int64
	AccountID int64  // 0 = instance-wide token
	Account   string // resolved slug, "" for instance-wide
	Name      string
	Caches    []string // single-cache scope; see allowsCache
	Perms     []string
	Revoked   bool
	Expires   int64 // unix; 0 = never
	Created   int64
}

// CreateToken generates a new secret (returned ONCE, only its hash is
// stored). A token is valid for exactly one cache: an instance token
// (accountID 0) names it "account/cache", an account token by its bare name.
// Admin-only (management) tokens carry no cache scope.
func (db *DB) CreateToken(accountID int64, name string, caches, perms []string, expires int64) (secret string, t *Token, err error) {
	raw := make([]byte, 32)
	if _, err = rand.Read(raw); err != nil {
		return "", nil, err
	}
	secret = base64.RawURLEncoding.EncodeToString(raw)
	if len(perms) == 0 {
		perms = []string{"pull"}
	}
	if caches, err = scopeOne(accountID, caches, perms); err != nil {
		return "", nil, err
	}
	t = &Token{AccountID: accountID, Name: name, Caches: caches, Perms: perms, Expires: expires, Created: time.Now().Unix()}
	err = db.write(func(tx *sql.Tx) error {
		return tx.QueryRow(
			`INSERT INTO tokens (account_id,name,hash,caches,perms,revoked,expires,created) VALUES (?,?,?,?,?,0,?,?) RETURNING id`,
			t.AccountID, t.Name, db.HashToken(secret), strings.Join(caches, ","), strings.Join(perms, ","), t.Expires, t.Created).Scan(&t.ID)
	})
	if err != nil {
		return "", nil, err
	}
	return secret, t, nil
}

const tokenCols = `t.id,t.account_id,COALESCE(a.slug,''),t.name,t.caches,t.perms,t.revoked,t.expires,t.created`
const tokenFrom = ` FROM tokens t LEFT JOIN accounts a ON a.id = t.account_id `

func (db *DB) ListTokens() ([]Token, error) {
	return db.listTokens(`SELECT ` + tokenCols + tokenFrom + `ORDER BY t.id`)
}

// ListAccountTokens lists tokens owned by one account.
func (db *DB) ListAccountTokens(accountID int64) ([]Token, error) {
	return db.listTokens(`SELECT `+tokenCols+tokenFrom+`WHERE t.account_id=? ORDER BY t.id`, accountID)
}

func (db *DB) listTokens(q string, args ...any) ([]Token, error) {
	rows, err := db.r.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Token
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

func scanToken(row interface{ Scan(...any) error }) (*Token, error) {
	var t Token
	var caches, perms string
	var revoked int
	if err := row.Scan(&t.ID, &t.AccountID, &t.Account, &t.Name, &caches, &perms, &revoked, &t.Expires, &t.Created); err != nil {
		return nil, err
	}
	t.Caches = strings.Split(caches, ",")
	t.Perms = strings.Split(perms, ",")
	t.Revoked = revoked != 0
	return &t, nil
}

// Expired reports whether the token has a set expiry in the past.
func (t *Token) Expired(now int64) bool { return t.Expires != 0 && now >= t.Expires }

// GetToken fetches one token's metadata by id.
func (db *DB) GetToken(id int64) (*Token, error) {
	row := db.r.QueryRow(`SELECT `+tokenCols+tokenFrom+`WHERE t.id=?`, id)
	t, err := scanToken(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return t, err
}

// UpdateToken rewrites a token's metadata (name, scope, perms, expiry). The
// secret itself is immutable — rotating credentials means a new token.
func (db *DB) UpdateToken(id int64, name string, caches, perms []string, expires int64) error {
	if len(perms) == 0 {
		perms = []string{"pull"}
	}
	var accountID int64
	if err := db.r.QueryRow(`SELECT account_id FROM tokens WHERE id=?`, id).Scan(&accountID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	var err error
	if caches, err = scopeOne(accountID, caches, perms); err != nil {
		return err
	}
	return db.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE tokens SET name=?, caches=?, perms=?, expires=? WHERE id=?`,
			name, strings.Join(caches, ","), strings.Join(perms, ","), expires, id)
		return err
	})
}

// RevokeToken flags a token revoked by id. Revocation is immediate — the next
// request re-reads this row.
func (db *DB) RevokeToken(id int64) error {
	return db.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE tokens SET revoked=1 WHERE id=?`, id)
		return err
	})
}

// lookupLive fetches a live (not revoked/expired) token by secret.
func (db *DB) lookupLive(secret string, now int64) (*Token, bool) {
	t, err := scanToken(db.r.QueryRow(`SELECT `+tokenCols+tokenFrom+`WHERE t.hash=?`, db.HashToken(secret)))
	if err != nil || t.Revoked || t.Expired(now) {
		return nil, false
	}
	return t, true
}

// Authorize reports whether the secret grants perm on account/cache. A single
// indexed read on the pool — cheap enough to run per request, and revocation
// is always seen because there is no cache to invalidate.
func (db *DB) Authorize(secret, account, cache, perm string, now int64) bool {
	t, ok := db.lookupLive(secret, now)
	if !ok {
		return false
	}
	return slices.Contains(t.Perms, perm) && t.allowsCache(account, cache)
}

// allowsCache reports whether the token's single-cache scope is
// account/cache. Admin-only tokens store "*" but never reach here for cache
// access — admin grants no pull/push.
func (t *Token) allowsCache(account, cache string) bool {
	if len(t.Caches) != 1 {
		return false
	}
	if t.AccountID != 0 {
		return t.Account == account && t.Caches[0] == cache
	}
	return t.Caches[0] == account+"/"+cache
}

// scopeOne enforces the scope rule: a token is valid for exactly one cache.
// Admin-only (management) tokens have no cache scope and store "*" — the
// cache protocol never consults it.
func scopeOne(accountID int64, caches, perms []string) ([]string, error) {
	adminOnly := !slices.ContainsFunc(perms, func(p string) bool { return p != "admin" })
	if adminOnly {
		return []string{"*"}, nil
	}
	if len(caches) != 1 || caches[0] == "*" || strings.HasSuffix(caches[0], "/*") {
		return nil, errors.New("token scope must name exactly one cache")
	}
	c := caches[0]
	if accountID == 0 && !strings.Contains(c, "/") {
		return nil, errors.New("instance token scope must be account/cache: " + c)
	}
	if accountID != 0 && strings.Contains(c, "/") {
		return nil, errors.New("account token scope must be a bare cache name: " + c)
	}
	return caches, nil
}

// AuthorizeAdmin reports whether the secret is a live token carrying the
// "admin" perm — the gate for instance-wide management API calls.
func (db *DB) AuthorizeAdmin(secret string, now int64) bool {
	t, ok := db.lookupLive(secret, now)
	return ok && slices.Contains(t.Perms, "admin")
}

// AuthorizeNS reports whether the secret grants a management perm
// (create / configure / destroy) for account/cache. A full
// "admin" token always passes.
func (db *DB) AuthorizeNS(secret, account, cache, perm string, now int64) bool {
	t, ok := db.lookupLive(secret, now)
	if !ok {
		return false
	}
	if slices.Contains(t.Perms, "admin") {
		return true
	}
	return slices.Contains(t.Perms, perm) && t.allowsCache(account, cache)
}
