package cli

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/stubbedev/xilo/internal/config"
	"github.com/stubbedev/xilo/internal/server"
	"github.com/stubbedev/xilo/internal/storage"
	"github.com/stubbedev/xilo/internal/store"
)

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
			db, err := store.Open(cfg.DBPath())
			if err != nil {
				return err
			}
			defer db.Close()
			st, err := storage.New(cfg.Storage)
			if err != nil {
				return err
			}
			srv, err := server.New(cfg, db, st)
			if err != nil {
				return err
			}
			return srv.Run()
		},
	}
}
