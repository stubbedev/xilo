//go:build !noserver

package cli

import (
	"log"
	"os"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/bcrypt"

	"github.com/stubbedev/xilo/internal/config"
	"github.com/stubbedev/xilo/internal/server"
	"github.com/stubbedev/xilo/internal/store"
)

// bootstrapAdmin seeds the instance's superadmin ("admin") from the config/env
// password on first run. Once any user exists the config value is ignored, so
// a stale env var can't reset a password.
//
// A superadmin runs the service and owns nothing in it. On a single-tenant
// instance that would leave a fresh install with nowhere to put a cache, so
// boot also creates the first workspace and makes them its owner: one person
// wearing both hats is the whole point of the hobbyist shape. A multi-tenant
// instance — one taking signups — creates none: its tenants arrive by signing up.
func bootstrapAdmin(db *store.DB, password string, selfService bool) error {
	if db.UsersExist() || password == "" {
		return nil
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	log.Printf("bootstrapping superadmin account from configured password")
	u, err := db.CreateUser("admin", "", string(hash), store.RoleSuperadmin)
	if err != nil || selfService {
		return err
	}
	acc, err := db.EnsureAccount(u.Name, "org")
	if err != nil {
		return err
	}
	log.Printf("single-tenant instance: created workspace %q owned by admin", acc.Slug)
	return db.MakeOwner(acc.ID, u.ID)
}

func serveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Run the cache server",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(configPath)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
				return err
			}
			db, err := openStore(cfg)
			if err != nil {
				return err
			}
			defer db.Close()
			if err := bootstrapAdmin(db, cfg.Admin.Password, cfg.SelfService); err != nil {
				return err
			}
			sts, err := openStorages(cfg)
			if err != nil {
				return err
			}
			srv, err := server.New(cfg, db, sts)
			if err != nil {
				return err
			}
			return srv.Run()
		},
	}
}
