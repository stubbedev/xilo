package store

import (
	"container/list"
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"reflect"

	"modernc.org/libc"
	"modernc.org/libc/sys/types"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const stmtCacheSize = 256

type sqliteConn interface {
	driver.Conn
	driver.ConnPrepareContext
	driver.ConnBeginTx
	driver.ExecerContext
	driver.QueryerContext
	driver.Pinger
	driver.SessionResetter
	driver.Validator
}

type sqliteStmt interface {
	driver.Stmt
	driver.StmtExecContext
	driver.StmtQueryContext
}

type sqliteRows interface {
	driver.Rows
	driver.RowsColumnTypeDatabaseTypeName
	driver.RowsColumnTypeLength
	driver.RowsColumnTypeNullable
	driver.RowsColumnTypePrecisionScale
	driver.RowsColumnTypeScanType
}

type stmtConnector struct{ driver.Connector }

func newStmtConnector(dsn string) (driver.Connector, error) {
	c, err := sqlite.NewConnector(dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite connector: %w", err)
	}
	return stmtConnector{c}, nil
}

func (c stmtConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	sc, ok := conn.(sqliteConn)
	if !ok {
		conn.Close()
		return nil, fmt.Errorf("sqlite connection %T lacks the driver interfaces the statement cache forwards", conn)
	}
	if err := enablePlanStability(conn); err != nil {
		return nil, errors.Join(err, conn.Close())
	}
	return &stmtConn{sqliteConn: sc, byQuery: make(map[string]*list.Element, stmtCacheSize)}, nil
}

func enablePlanStability(conn driver.Conn) error {
	v := reflect.ValueOf(conn)
	if v.Kind() != reflect.Pointer || v.Elem().Kind() != reflect.Struct {
		return fmt.Errorf("sqlite connection %T is not a struct pointer", conn)
	}
	db, tlsField := v.Elem().FieldByName("db"), v.Elem().FieldByName("tls")
	if db.Kind() != reflect.Uintptr || db.Uint() == 0 || !tlsField.IsValid() || tlsField.Type() != reflect.TypeFor[*libc.TLS]() || tlsField.IsNil() {
		return fmt.Errorf("sqlite connection %T no longer exposes the db and tls handles needed to set SQLITE_DBCONFIG_ENABLE_QPSG", conn)
	}
	tls := (*libc.TLS)(tlsField.UnsafePointer())
	const vaSlot = 8
	va := libc.Xmalloc(tls, types.Size_t(2*vaSlot))
	if va == 0 {
		return errors.New("sqlite: out of memory setting SQLITE_DBCONFIG_ENABLE_QPSG")
	}
	defer libc.Xfree(tls, va)
	if rc := sqlite3.Xsqlite3_db_config(tls, uintptr(db.Uint()), sqlite3.SQLITE_DBCONFIG_ENABLE_QPSG, libc.VaList(va, int32(1), uintptr(0))); rc != sqlite3.SQLITE_OK {
		return fmt.Errorf("sqlite3_db_config(SQLITE_DBCONFIG_ENABLE_QPSG) returned %d", rc)
	}
	return nil
}

type stmtConn struct {
	sqliteConn
	lru     list.List
	byQuery map[string]*list.Element
}

type cachedStmt struct {
	query   string
	stmt    sqliteStmt
	busy    bool
	evicted bool
}

func (c *stmtConn) acquire(ctx context.Context, query string) (cs *cachedStmt, free bool, err error) {
	if e, ok := c.byQuery[query]; ok {
		cs := e.Value.(*cachedStmt)
		if cs.busy {
			return nil, false, nil
		}
		c.lru.MoveToFront(e)
		cs.busy = true
		return cs, true, nil
	}
	ds, err := c.PrepareContext(ctx, query)
	if err != nil {
		return nil, false, err
	}
	st, ok := ds.(sqliteStmt)
	if !ok {
		return nil, false, errors.Join(fmt.Errorf("sqlite statement %T lacks context execution", ds), ds.Close())
	}
	cs = &cachedStmt{query: query, stmt: st, busy: true}
	c.byQuery[query] = c.lru.PushFront(cs)
	if c.lru.Len() > stmtCacheSize {
		if err := c.evict(c.lru.Back()); err != nil {
			return nil, false, errors.Join(err, c.release(cs))
		}
	}
	return cs, true, nil
}

func (c *stmtConn) evict(e *list.Element) error {
	cs := c.lru.Remove(e).(*cachedStmt)
	delete(c.byQuery, cs.query)
	if cs.busy {
		cs.evicted = true
		return nil
	}
	return cs.stmt.Close()
}

func (c *stmtConn) release(cs *cachedStmt) error {
	cs.busy = false
	if cs.evicted {
		return cs.stmt.Close()
	}
	return nil
}

func (c *stmtConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	cs, free, err := c.acquire(ctx, query)
	if err != nil {
		return nil, err
	}
	if !free {
		return c.sqliteConn.ExecContext(ctx, query, args)
	}
	res, err := cs.stmt.ExecContext(ctx, args)
	return res, errors.Join(err, c.release(cs))
}

func (c *stmtConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	cs, free, err := c.acquire(ctx, query)
	if err != nil {
		return nil, err
	}
	if !free {
		return c.sqliteConn.QueryContext(ctx, query, args)
	}
	rows, err := cs.stmt.QueryContext(ctx, args)
	if err != nil {
		return nil, errors.Join(err, c.release(cs))
	}
	sr, ok := rows.(sqliteRows)
	if !ok {
		return nil, errors.Join(fmt.Errorf("sqlite rows %T lack column type information", rows), rows.Close(), c.release(cs))
	}
	return &stmtRows{sqliteRows: sr, conn: c, cs: cs}, nil
}

func (c *stmtConn) Close() error {
	errs := make([]error, 0, c.lru.Len()+1)
	for e := c.lru.Front(); e != nil; e = e.Next() {
		errs = append(errs, e.Value.(*cachedStmt).stmt.Close())
	}
	c.lru.Init()
	clear(c.byQuery)
	return errors.Join(append(errs, c.sqliteConn.Close())...)
}

type stmtRows struct {
	sqliteRows
	conn *stmtConn
	cs   *cachedStmt
}

func (r *stmtRows) Close() error {
	return errors.Join(r.sqliteRows.Close(), r.conn.release(r.cs))
}
