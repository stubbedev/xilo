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

// bootstrapAdmin seeds the first account ("admin", role admin) from the
// config/env password on first run. Once any user exists, the config value is
// ignored so a stale env var can't reset a password.
func bootstrapAdmin(db *store.DB, password string) error {
	if db.UsersExist() || password == "" {
		return nil
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	log.Printf("bootstrapping admin account from configured password")
	_, err = db.CreateUser("admin", "", string(hash), "admin")
	return err
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
			if err := bootstrapAdmin(db, cfg.Admin.Password); err != nil {
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
