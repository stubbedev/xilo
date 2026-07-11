package store

import "testing"

func TestRebind(t *testing.T) {
	cases := []struct{ in, want string }{
		{`SELECT 1`, `SELECT 1`},
		{`SELECT * FROM t WHERE a=?`, `SELECT * FROM t WHERE a=$1`},
		{`INSERT INTO t VALUES (?,?,?)`, `INSERT INTO t VALUES ($1,$2,$3)`},
		// `?` inside a string literal must survive.
		{`SELECT '?' , a FROM t WHERE b=?`, `SELECT '?' , a FROM t WHERE b=$1`},
		// The ESCAPE clause used by SearchPaths.
		{`SELECT a FROM t WHERE a LIKE ? ESCAPE '\' AND b=?`, `SELECT a FROM t WHERE a LIKE $1 ESCAPE '\' AND b=$2`},
		// Ten+ placeholders cross into two-digit ordinals.
		{`(?,?,?,?,?,?,?,?,?,?,?)`, `($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`},
	}
	for _, c := range cases {
		if got := rebind(c.in); got != c.want {
			t.Errorf("rebind(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
