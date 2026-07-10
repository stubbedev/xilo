package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/stubbedev/xilo/internal/storage"
)

func gcCmd() *cobra.Command {
	var olderThan time.Duration
	c := &cobra.Command{
		Use:   "gc",
		Short: "Garbage-collect unreferenced chunks",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, db, err := openDB()
			if err != nil {
				return err
			}
			defer db.Close()

			// LRU eviction: drop paths not accessed within the window, then
			// sweep the chunks they orphaned.
			if olderThan > 0 {
				cutoff := time.Now().Add(-olderThan).Unix()
				n, err := db.EvictPathsOlderThan(cutoff)
				if err != nil {
					return err
				}
				fmt.Printf("evicted %d paths older than %s\n", n, olderThan)
			}

			st, err := storage.New(cfg.Storage)
			if err != nil {
				return err
			}
			// Grace window: never sweep chunks newer than this (mirrors the
			// server so a manual gc can't race an in-flight push). A bad
			// duration fails SAFE to 1h — grace 0 would sweep in-flight chunks.
			grace, perr := time.ParseDuration(cfg.GC.Grace)
			if perr != nil && cfg.GC.Grace != "" && cfg.GC.Grace != "0" {
				fmt.Printf("bad gc.grace %q, using 1h\n", cfg.GC.Grace)
				grace = time.Hour
			}
			graceCutoff := time.Now().Add(-grace).Unix()
			deleted, freed, err := db.GC(cmd.Context(), st, graceCutoff)
			if err != nil {
				return err
			}
			fmt.Printf("removed %d chunks, freed %d bytes\n", deleted, freed)
			return nil
		},
	}
	c.Flags().DurationVar(&olderThan, "older-than", 0,
		"also evict store paths not pulled within this window (e.g. 720h)")
	return c
}
