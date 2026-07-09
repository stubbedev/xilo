package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"slices"
	"strings"
	"time"
)

type Token struct {
	ID      int64
	Name    string
	Caches  []string // cache names, or ["*"] for all
	Perms   []string // "push", "pull"
	Revoked bool
	Expires int64 // unix; 0 = never
	Created int64
}

// HashToken is the stored form of a secret token.
func HashToken(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// CreateToken generates a new secret (returned ONCE, only its hash is stored)
// scoped to the given caches and perms. caches nil/empty means all ("*").
func (db *DB) CreateToken(name string, caches, perms []string, expires int64) (secret string, t *Token, err error) {
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
	t = &Token{Name: name, Caches: caches, Perms: perms, Expires: expires, Created: time.Now().Unix()}
	err = db.write(func(tx *sql.Tx) error {
		res, e := tx.Exec(
			`INSERT INTO tokens (name,hash,caches,perms,revoked,expires,created) VALUES (?,?,?,?,0,?,?)`,
			t.Name, HashToken(secret), strings.Join(caches, ","), strings.Join(perms, ","), t.Expires, t.Created)
		if e != nil {
			return e
		}
		t.ID, e = res.LastInsertId()
		return e
	})
	if err != nil {
		return "", nil, err
	}
	return secret, t, nil
}

func (db *DB) ListTokens() ([]Token, error) {
	rows, err := db.r.Query(`SELECT id,name,caches,perms,revoked,expires,created FROM tokens ORDER BY id`)
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
	if err := row.Scan(&t.ID, &t.Name, &caches, &perms, &revoked, &t.Expires, &t.Created); err != nil {
		return nil, err
	}
	t.Caches = strings.Split(caches, ",")
	t.Perms = strings.Split(perms, ",")
	t.Revoked = revoked != 0
	return &t, nil
}

// Expired reports whether the token has a set expiry in the past.
func (t *Token) Expired(now int64) bool { return t.Expires != 0 && now >= t.Expires }

// RevokeToken flags a token revoked by id. Revocation is immediate — the next
// request re-reads this row.
func (db *DB) RevokeToken(id int64) error {
	return db.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE tokens SET revoked=1 WHERE id=?`, id)
		return err
	})
}

// Authorize reports whether the secret grants perm on cache. A single indexed
// read on the WAL pool — cheap enough to run per request, and revocation is
// always seen because there is no cache to invalidate.
func (db *DB) Authorize(secret, cache, perm string, now int64) bool {
	row := db.r.QueryRow(`SELECT caches,perms,revoked,expires FROM tokens WHERE hash=?`, HashToken(secret))
	var caches, perms string
	var revoked, expires int64
	if err := row.Scan(&caches, &perms, &revoked, &expires); err != nil {
		return false
	}
	if revoked != 0 || (expires != 0 && now >= expires) {
		return false
	}
	return scopeAllows(strings.Split(caches, ","), cache) &&
		slices.Contains(strings.Split(perms, ","), perm)
}

func scopeAllows(scoped []string, cache string) bool {
	return slices.Contains(scoped, "*") || slices.Contains(scoped, cache)
}
