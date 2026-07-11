package store

import (
	"context"
	"database/sql/driver"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// Postgres support rides the exact same SQL as SQLite: every query in this
// package is written once with `?` placeholders, and this connector rewrites
// them to `$1…$n` on the way into pgx. Keeping the rewrite at the driver
// boundary means the dynamic IN(...) builders and every call site stay
// dialect-free.

// pgConnector wraps the pgx stdlib connector with placeholder rewriting.
type pgConnector struct{ inner driver.Connector }

func newPGConnector(dsn string) (driver.Connector, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	return pgConnector{inner: stdlib.GetConnector(*cfg)}, nil
}

func (c pgConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return pgConn{conn: conn}, nil
}

func (c pgConnector) Driver() driver.Driver { return nil }

// pgConn implements the optional database/sql driver interfaces the stdlib
// pgx conn provides, rewriting the query text in each.
type pgConn struct{ conn driver.Conn }

func (c pgConn) Prepare(query string) (driver.Stmt, error) {
	return c.conn.Prepare(rebind(query))
}

func (c pgConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if p, ok := c.conn.(driver.ConnPrepareContext); ok {
		return p.PrepareContext(ctx, rebind(query))
	}
	return c.conn.Prepare(rebind(query))
}

func (c pgConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if e, ok := c.conn.(driver.ExecerContext); ok {
		return e.ExecContext(ctx, rebind(query), args)
	}
	return nil, driver.ErrSkip
}

func (c pgConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if q, ok := c.conn.(driver.QueryerContext); ok {
		return q.QueryContext(ctx, rebind(query), args)
	}
	return nil, driver.ErrSkip
}

func (c pgConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if b, ok := c.conn.(driver.ConnBeginTx); ok {
		return b.BeginTx(ctx, opts)
	}
	return c.conn.Begin() //nolint:staticcheck // fallback per database/sql contract
}

func (c pgConn) Begin() (driver.Tx, error) { return c.conn.Begin() } //nolint:staticcheck

func (c pgConn) Close() error { return c.conn.Close() }

func (c pgConn) Ping(ctx context.Context) error {
	if p, ok := c.conn.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

func (c pgConn) ResetSession(ctx context.Context) error {
	if r, ok := c.conn.(driver.SessionResetter); ok {
		return r.ResetSession(ctx)
	}
	return nil
}

func (c pgConn) IsValid() bool {
	if v, ok := c.conn.(driver.Validator); ok {
		return v.IsValid()
	}
	return true
}

// rebind rewrites `?` placeholders to `$1…$n`, skipping text inside
// single-quoted SQL literals (e.g. ESCAPE '\' or a literal '?').
func rebind(query string) string {
	if !strings.ContainsRune(query, '?') {
		return query
	}
	var b strings.Builder
	b.Grow(len(query) + 8)
	n := 0
	inStr := false
	for i := 0; i < len(query); i++ {
		ch := query[i]
		switch {
		case ch == '\'':
			inStr = !inStr
			b.WriteByte(ch)
		case ch == '?' && !inStr:
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
		default:
			b.WriteByte(ch)
		}
	}
	return b.String()
}
