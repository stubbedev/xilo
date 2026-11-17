//go:build !noserver

package cli

import (
	"path/filepath"
	"testing"

	"github.com/stubbedev/xilo/internal/store"
)

func TestBootstrapAdmin(t *testing.T) {
	isolateEnv(t)
	db, err := store.Open(filepath.Join(t.TempDir(), "xilo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// empty password -> no-op
	if err := bootstrapAdmin(db, "", false); err != nil {
		t.Fatal(err)
	}
	if db.UsersExist() {
		t.Fatal("admin created from empty password")
	}

	if err := bootstrapAdmin(db, "hunter2", false); err != nil {
		t.Fatal(err)
	}
	if !db.UsersExist() {
		t.Fatal("admin not created")
	}
	u1, err := db.GetUserByName("admin")
	if err != nil {
		t.Fatal(err)
	}
	hash1 := u1.PassHash

	// second bootstrap with a different password must not overwrite
	if err := bootstrapAdmin(db, "other", false); err != nil {
		t.Fatal(err)
	}
	u2, _ := db.GetUserByName("admin")
	hash2 := u2.PassHash
	if hash1 != hash2 {
		t.Fatal("existing admin credential overwritten")
	}
}
