package store

import (
	"database/sql"
	"strings"
	"time"
)

// AuditEntry is one recorded admin/API mutation. Actor is the acting user's
// name at the time (denormalized for readability); UserID resolves the row via
// GetUser even after the user soft-deletes. IP/UserAgent/DurationMs capture the
// request context for observability.
type AuditEntry struct {
	ID         int64
	TS         int64
	UserID     int64
	Actor      string // "" = no session (token/CLI/pre-login)
	Method     string
	Path       string
	Status     int
	IP         string
	UserAgent  string
	DurationMs int64
}

// Audit records one mutation. Fire-and-forget: callers log the error but never
// fail the request over an audit-write miss. TS is stamped here — callers leave
// it zero.
func (db *DB) Audit(e AuditEntry) error {
	return db.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`INSERT INTO audit_log (ts,user_id,actor,method,path,status,ip,user_agent,duration_ms)
			 VALUES (?,?,?,?,?,?,?,?,?)`,
			time.Now().Unix(), e.UserID, e.Actor, e.Method, e.Path, e.Status, e.IP, e.UserAgent, e.DurationMs)
		return err
	})
}

// SearchAudit lists a page of action-log entries. The query is split on
// whitespace; every term must substring-match the actor, method or path
// (case-insensitive). sortKey (time|actor|method|path|status) + sortDir
// (asc|desc) pick the order; the default is newest first. total is the match
// count before limit/offset.
func (db *DB) SearchAudit(q string, limit, offset int, sortKey, sortDir string) (entries []AuditEntry, total int64, err error) {
	where := `1=1`
	var args []any
	for _, term := range strings.Fields(q) {
		where += ` AND (lower(actor) LIKE ? ESCAPE '\' OR lower(method) LIKE ? ESCAPE '\' OR lower(path) LIKE ? ESCAPE '\' OR lower(ip) LIKE ? ESCAPE '\')`
		p := substrPattern(term)
		args = append(args, p, p, p, p)
	}
	dir := ` DESC`
	if sortDir == "asc" {
		dir = ` ASC`
	}
	// Default (and "time"): order by id, which is monotonic with ts and unique.
	order := `id` + dir
	switch sortKey {
	case "actor":
		order = `lower(actor)` + dir + `, id DESC`
	case "method":
		order = `method` + dir + `, id DESC`
	case "path":
		order = `lower(path)` + dir + `, id DESC`
	case "status":
		order = `status` + dir + `, id DESC`
	}
	args = append(args, limit, offset)
	rows, err := db.r.Query(
		`SELECT id,ts,user_id,actor,method,path,status,ip,user_agent,duration_ms, COUNT(*) OVER ()
		   FROM audit_log
		  WHERE `+where+`
		  ORDER BY `+order+` LIMIT ? OFFSET ?`,
		args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.TS, &e.UserID, &e.Actor, &e.Method, &e.Path, &e.Status, &e.IP, &e.UserAgent, &e.DurationMs, &total); err != nil {
			return nil, 0, err
		}
		entries = append(entries, e)
	}
	return entries, total, rows.Err()
}

// PruneAuditBatch deletes up to limit action-log entries older than cutoff
// (unix seconds) in a single write transaction, returning the number removed.
// Callers loop it with pauses between calls so a large backlog never holds the
// writer goroutine for long. The subquery form is portable across SQLite and
// PostgreSQL (neither reliably supports DELETE ... LIMIT directly).
func (db *DB) PruneAuditBatch(cutoff int64, limit int) (int64, error) {
	var n int64
	err := db.write(func(tx *sql.Tx) error {
		res, err := tx.Exec(
			`DELETE FROM audit_log WHERE id IN (
				SELECT id FROM audit_log WHERE ts < ? ORDER BY id LIMIT ?)`,
			cutoff, limit)
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		return nil
	})
	return n, err
}
