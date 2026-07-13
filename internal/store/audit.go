package store

import (
	"database/sql"
	"time"
)

// AuditEntry is one recorded admin/API mutation. Actor is the acting user's
// name at the time (denormalized for readability); UserID resolves the row via
// GetUser even after the user soft-deletes.
type AuditEntry struct {
	ID     int64
	TS     int64
	UserID int64
	Actor  string // "" = no session (token/CLI/pre-login)
	Method string
	Path   string
	Status int
}

// Audit records one mutation. Fire-and-forget: callers log the error but never
// fail the request over an audit-write miss.
func (db *DB) Audit(userID int64, actor, method, path string, status int) error {
	return db.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`INSERT INTO audit_log (ts,user_id,actor,method,path,status) VALUES (?,?,?,?,?,?)`,
			time.Now().Unix(), userID, actor, method, path, status)
		return err
	})
}

// ListAudit returns the most recent entries, newest first.
func (db *DB) ListAudit(limit int) ([]AuditEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.r.Query(
		`SELECT id,ts,user_id,actor,method,path,status FROM audit_log ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.TS, &e.UserID, &e.Actor, &e.Method, &e.Path, &e.Status); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
