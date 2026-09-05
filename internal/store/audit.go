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

// SearchAudit lists a page of activity entries. The query is split on
// whitespace; every term must substring-match the actor, method or path
// (case-insensitive). method ("" = any) narrows to one HTTP method; status
// ("" = any, else "2xx".."5xx") narrows to one status class. sortKey
// (time|actor|method|path|status) + sortDir (asc|desc) pick the order; the
// default is newest first. total is the match count before limit/offset.
func (db *DB) SearchAudit(q, method, status string, limit, offset int, sortKey, sortDir string) (entries []AuditEntry, total int64, err error) {
	where := `1=1`
	var args []any
	if method != "" {
		where += ` AND method=?`
		args = append(args, method)
	}
	if lo, ok := statusClass(status); ok {
		where += ` AND status>=? AND status<?`
		args = append(args, lo, lo+100)
	}
	var whereSb47 strings.Builder
	for term := range strings.FieldsSeq(q) {
		whereSb47.WriteString(` AND (lower(actor) LIKE ? ESCAPE '\' OR lower(method) LIKE ? ESCAPE '\' OR lower(path) LIKE ? ESCAPE '\' OR lower(ip) LIKE ? ESCAPE '\')`)
		p := substrPattern(term)
		args = append(args, p, p, p, p)
	}
	where += whereSb47.String()
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
	// Counted separately, not as a `COUNT(*) OVER ()` window: the window made
	// every page read the whole table to fill in one number, so the default
	// view (newest 25, straight off the primary key) paid for every row ever
	// recorded.
	if err := db.r.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, limit, offset)
	rows, err := db.r.Query(
		`SELECT id,ts,user_id,actor,method,path,status,ip,user_agent,duration_ms
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
		if err := rows.Scan(&e.ID, &e.TS, &e.UserID, &e.Actor, &e.Method, &e.Path, &e.Status, &e.IP, &e.UserAgent, &e.DurationMs); err != nil {
			return nil, 0, err
		}
		entries = append(entries, e)
	}
	return entries, total, rows.Err()
}

// statusClass maps a "2xx".."5xx" filter to its lower bound.
func statusClass(s string) (int, bool) {
	switch s {
	case "2xx":
		return 200, true
	case "3xx":
		return 300, true
	case "4xx":
		return 400, true
	case "5xx":
		return 500, true
	}
	return 0, false
}

// AuditStats is the activity page's summary: everything ever recorded (within
// retention), how much of it failed, how many distinct signed-in actors, and
// the mean request duration.
type AuditStats struct {
	Total, Failed, Actors, AvgMs int64
}

// AuditStats aggregates the whole audit_log in one scan.
func (db *DB) AuditStats() (AuditStats, error) {
	var st AuditStats
	err := db.r.QueryRow(`SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN status>=400 THEN 1 ELSE 0 END),0),
		COUNT(DISTINCT NULLIF(actor,'')),
		COALESCE(CAST(AVG(duration_ms) AS INTEGER),0) FROM audit_log`).
		Scan(&st.Total, &st.Failed, &st.Actors, &st.AvgMs)
	return st, err
}

// PruneAuditBatch deletes up to limit activity entries older than cutoff
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
