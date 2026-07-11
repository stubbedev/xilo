package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"time"
)

// Perms carried by tokens. pull/push gate the cache protocol; the rest gate
// the management API (attic parity: create/configure/destroy per scope,
// admin = full instance control).
var ValidPerms = []string{"pull", "push", "create-cache", "configure-cache", "destroy-cache", "admin"}

type Token struct {
	ID        int64
	AccountID int64  // 0 = instance-wide token
	Account   string // resolved slug, "" for instance-wide
	Name      string
	Caches    []string // scope patterns; see scopeAllows
	Perms     []string
	Revoked   bool
	Expires   int64 // unix; 0 = never
	Created   int64
}

// HashToken is the stored form of a secret token.
func HashToken(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// CreateToken generates a new secret (returned ONCE, only its hash is stored)
// scoped to the given cache patterns and perms. accountID 0 = instance-wide
// token whose patterns are "*", "account/*" or "account/cache"; an account
// token's patterns are "*" or bare cache names within its account. caches
// nil/empty = all.
func (db *DB) CreateToken(accountID int64, name string, caches, perms []string, expires int64) (secret string, t *Token, err error) {
	raw := make([]byte, 32)
	if _, err = rand.Read(raw); err != nil {
		return "", nil, err
	}
	secret = base64.RawURLEncoding.EncodeToString(raw)
	if len(caches) == 0 {
		caches = []string{"*"}
	}
	if len(perms) == 0 {
		perms = []string{"pull"}
	}
	if accountID == 0 {
		for _, c := range caches {
			if c != "*" && !strings.Contains(c, "/") {
				return "", nil, errors.New("instance token scope must be *, account/* or account/cache: " + c)
			}
		}
	}
	t = &Token{AccountID: accountID, Name: name, Caches: caches, Perms: perms, Expires: expires, Created: time.Now().Unix()}
	err = db.write(func(tx *sql.Tx) error {
		return tx.QueryRow(
			`INSERT INTO tokens (account_id,name,hash,caches,perms,revoked,expires,created) VALUES (?,?,?,?,?,0,?,?) RETURNING id`,
			t.AccountID, t.Name, HashToken(secret), strings.Join(caches, ","), strings.Join(perms, ","), t.Expires, t.Created).Scan(&t.ID)
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
	if len(caches) == 0 {
		caches = []string{"*"}
	}
	if len(perms) == 0 {
		perms = []string{"pull"}
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
	t, err := scanToken(db.r.QueryRow(`SELECT `+tokenCols+tokenFrom+`WHERE t.hash=?`, HashToken(secret)))
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

// allowsCache reports whether the token's scope covers account/cache.
func (t *Token) allowsCache(account, cache string) bool {
	if t.AccountID != 0 {
		// Account token: only its own account, bare-name patterns.
		return t.Account == account && scopeAllows(t.Caches, cache)
	}
	// Instance token: "*", "account/*" or "account/cache" patterns.
	for _, p := range t.Caches {
		if p == "*" || p == account+"/*" || p == account+"/"+cache {
			return true
		}
	}
	return false
}

func scopeAllows(scoped []string, cache string) bool {
	return slices.Contains(scoped, "*") || slices.Contains(scoped, cache)
}

// AuthorizeAdmin reports whether the secret is a live token carrying the
// "admin" perm — the gate for instance-wide management API calls.
func (db *DB) AuthorizeAdmin(secret string, now int64) bool {
	t, ok := db.lookupLive(secret, now)
	return ok && slices.Contains(t.Perms, "admin")
}

// AuthorizeNS reports whether the secret grants a management perm
// (create-cache / configure-cache / destroy-cache) for account/cache. A full
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
