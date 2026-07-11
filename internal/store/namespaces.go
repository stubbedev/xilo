package store

import (
	"database/sql"
	"errors"
	"time"
)

// Namespace is the tenancy unit: caches live in a namespace, tokens can be
// scoped to one, and users join one as owner or member.
type Namespace struct {
	ID      int64
	Name    string
	Created int64
}

// NamespaceMember links a user to a namespace. Owners manage the namespace's
// caches and tokens; members get visibility.
type NamespaceMember struct {
	NamespaceID int64
	UserID      int64
	UserName    string
	Role        string // "owner" | "member"
}

// EnsureNamespace returns the namespace named name, creating it if missing.
func (db *DB) EnsureNamespace(name string) (*Namespace, error) {
	if ns, err := db.GetNamespace(name); err == nil {
		return ns, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	ns := &Namespace{Name: name, Created: time.Now().Unix()}
	err := db.write(func(tx *sql.Tx) error {
		// Concurrent creators race benignly: ON CONFLICT keeps the winner.
		if _, err := tx.Exec(`INSERT INTO namespaces (name, created) VALUES (?,?) ON CONFLICT (name) DO NOTHING`,
			ns.Name, ns.Created); err != nil {
			return err
		}
		return tx.QueryRow(`SELECT id, created FROM namespaces WHERE name=?`, name).Scan(&ns.ID, &ns.Created)
	})
	if err != nil {
		return nil, err
	}
	return ns, nil
}

func (db *DB) GetNamespace(name string) (*Namespace, error) {
	var ns Namespace
	err := db.r.QueryRow(`SELECT id, name, created FROM namespaces WHERE name=?`, name).
		Scan(&ns.ID, &ns.Name, &ns.Created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &ns, err
}

func (db *DB) ListNamespaces() ([]Namespace, error) {
	return db.listNamespaces(`SELECT id, name, created FROM namespaces ORDER BY name`)
}

// UserNamespaces lists the namespaces a user belongs to.
func (db *DB) UserNamespaces(userID int64) ([]Namespace, error) {
	return db.listNamespaces(`SELECT n.id, n.name, n.created FROM namespaces n
		JOIN namespace_members m ON m.namespace_id = n.id WHERE m.user_id=? ORDER BY n.name`, userID)
}

func (db *DB) listNamespaces(q string, args ...any) ([]Namespace, error) {
	rows, err := db.r.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Namespace
	for rows.Next() {
		var ns Namespace
		if err := rows.Scan(&ns.ID, &ns.Name, &ns.Created); err != nil {
			return nil, err
		}
		out = append(out, ns)
	}
	return out, rows.Err()
}

// DeleteNamespace removes an EMPTY namespace (no caches). Refusing non-empty
// deletion keeps a fat-fingered admin from cascading a tenant's data away.
func (db *DB) DeleteNamespace(id int64) error {
	return db.write(func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM caches WHERE namespace_id=?`, id).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return errors.New("namespace still has caches — destroy them first")
		}
		if _, err := tx.Exec(`DELETE FROM namespace_members WHERE namespace_id=?`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM tokens WHERE namespace_id=?`, id); err != nil {
			return err
		}
		_, err := tx.Exec(`DELETE FROM namespaces WHERE id=?`, id)
		return err
	})
}

// SetMember adds a user to a namespace or updates their role.
func (db *DB) SetMember(nsID, userID int64, role string) error {
	return db.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO namespace_members (namespace_id, user_id, role) VALUES (?,?,?)
			 ON CONFLICT (namespace_id, user_id) DO UPDATE SET role=excluded.role`, nsID, userID, role)
		return err
	})
}

// RemoveMember drops a user from a namespace.
func (db *DB) RemoveMember(nsID, userID int64) error {
	return db.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM namespace_members WHERE namespace_id=? AND user_id=?`, nsID, userID)
		return err
	})
}

// ListMembers returns a namespace's members with usernames.
func (db *DB) ListMembers(nsID int64) ([]NamespaceMember, error) {
	rows, err := db.r.Query(`SELECT m.namespace_id, m.user_id, u.username, m.role
		FROM namespace_members m JOIN users u ON u.id = m.user_id
		WHERE m.namespace_id=? ORDER BY u.username`, nsID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NamespaceMember
	for rows.Next() {
		var m NamespaceMember
		if err := rows.Scan(&m.NamespaceID, &m.UserID, &m.UserName, &m.Role); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MemberRole returns the user's role in a namespace ("" if not a member).
func (db *DB) MemberRole(nsID, userID int64) string {
	var role string
	if err := db.r.QueryRow(`SELECT role FROM namespace_members WHERE namespace_id=? AND user_id=?`,
		nsID, userID).Scan(&role); err != nil {
		return ""
	}
	return role
}
