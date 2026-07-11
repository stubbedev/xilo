package store

import (
	"database/sql"
	"errors"
	"time"
)

// Plan is a quota bundle selectable at signup and assignable to accounts.
// Zero values mean unlimited/disallowed-nothing: the absence of a plan
// (accounts.plan_id = 0) is also "no limits".
type Plan struct {
	ID           int64
	Name         string
	MaxCaches    int64 // 0 = unlimited
	MaxMembers   int64 // 0 = unlimited (org members)
	MaxStorage   int64 // logical bytes across the account's caches; 0 = unlimited
	MaxRetention int64 // seconds; caps per-cache retention; 0 = unlimited
	OrgsAllowed  bool  // may this account's user create organizations?
	Public       bool  // selectable at self-registration
	Created      int64
}

const planCols = `id,name,max_caches,max_members,max_storage,max_retention,orgs_allowed,public,created`

func scanPlan(row interface{ Scan(...any) error }) (*Plan, error) {
	var p Plan
	var orgs, pub int
	if err := row.Scan(&p.ID, &p.Name, &p.MaxCaches, &p.MaxMembers, &p.MaxStorage, &p.MaxRetention, &orgs, &pub, &p.Created); err != nil {
		return nil, err
	}
	p.OrgsAllowed = orgs != 0
	p.Public = pub != 0
	return &p, nil
}

func (db *DB) CreatePlan(p *Plan) (*Plan, error) {
	p.Created = time.Now().Unix()
	err := db.write(func(tx *sql.Tx) error {
		return tx.QueryRow(
			`INSERT INTO plans (name,max_caches,max_members,max_storage,max_retention,orgs_allowed,public,created)
			 VALUES (?,?,?,?,?,?,?,?) RETURNING id`,
			p.Name, p.MaxCaches, p.MaxMembers, p.MaxStorage, p.MaxRetention, b2i(p.OrgsAllowed), b2i(p.Public), p.Created).Scan(&p.ID)
	})
	if err != nil {
		return nil, err
	}
	return p, nil
}

func (db *DB) UpdatePlan(p *Plan) error {
	return db.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`UPDATE plans SET name=?, max_caches=?, max_members=?, max_storage=?, max_retention=?, orgs_allowed=?, public=? WHERE id=?`,
			p.Name, p.MaxCaches, p.MaxMembers, p.MaxStorage, p.MaxRetention, b2i(p.OrgsAllowed), b2i(p.Public), p.ID)
		return err
	})
}

// DeletePlan refuses while any account still uses the plan.
func (db *DB) DeletePlan(id int64) error {
	return db.write(func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM accounts WHERE plan_id=?`, id).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return errors.New("plan is in use by accounts")
		}
		_, err := tx.Exec(`DELETE FROM plans WHERE id=?`, id)
		return err
	})
}

func (db *DB) GetPlan(id int64) (*Plan, error) {
	if id == 0 {
		return nil, ErrNotFound
	}
	p, err := scanPlan(db.r.QueryRow(`SELECT `+planCols+` FROM plans WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

func (db *DB) ListPlans() ([]Plan, error) {
	return db.listPlans(`SELECT ` + planCols + ` FROM plans ORDER BY name`)
}

// PublicPlans lists the plans offered at self-registration.
func (db *DB) PublicPlans() ([]Plan, error) {
	return db.listPlans(`SELECT ` + planCols + ` FROM plans WHERE public=1 ORDER BY name`)
}

func (db *DB) listPlans(q string, args ...any) ([]Plan, error) {
	rows, err := db.r.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Plan
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// AccountPlan resolves an account's plan; (nil, nil) means no plan = no limits.
func (db *DB) AccountPlan(a *Account) (*Plan, error) {
	if a == nil || a.PlanID == 0 {
		return nil, nil
	}
	p, err := db.GetPlan(a.PlanID)
	if errors.Is(err, ErrNotFound) {
		return nil, nil // plan deleted out from under the account: unlimited
	}
	return p, err
}

// ---- instance settings (DB-backed policy; yaml stays deployment-only) ----

// Setting reads one instance setting ("" when unset).
func (db *DB) Setting(key string) string {
	var v string
	if err := db.r.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v); err != nil {
		return ""
	}
	return v
}

// SetSetting upserts one instance setting.
func (db *DB) SetSetting(key, value string) error {
	return db.write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO settings (key, value) VALUES (?,?)
			 ON CONFLICT (key) DO UPDATE SET value=excluded.value`, key, value)
		return err
	})
}

// SettingBool reads a boolean setting with a default.
func (db *DB) SettingBool(key string, def bool) bool {
	switch db.Setting(key) {
	case "1":
		return true
	case "0":
		return false
	}
	return def
}
