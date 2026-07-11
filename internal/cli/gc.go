package cli

import (
	"fmt"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/stubbedev/xilo/internal/api"
)

func gcCmd() *cobra.Command {
	var olderThan time.Duration
	c := &cobra.Command{
		Use:   "gc",
		Short: "Garbage-collect unreferenced chunks",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			apic, cfg, db, err := adminTarget(adminServer, adminToken)
			if err != nil {
				return err
			}
			if apic != nil {
				var resp api.GCResp
				req := api.GCReq{EvictOlderThan: int64(olderThan.Seconds())}
				if err := apic.do(http.MethodPost, "/api/v1/gc", req, &resp); err != nil {
					return err
				}
				if olderThan > 0 {
					fmt.Printf("evicted %d paths older than %s\n", resp.Evicted, olderThan)
				}
				fmt.Printf("removed %d chunks, freed %d bytes\n", resp.Deleted, resp.FreedBytes)
				return nil
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

			sts, err := openStorages(cfg)
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
			var deleted int
			var freed int64
			for name, st := range sts {
				d, f, err := db.GC(cmd.Context(), st, name, graceCutoff)
				deleted += d
				freed += f
				if err != nil {
					return err
				}
			}
			fmt.Printf("removed %d chunks, freed %d bytes\n", deleted, freed)
			return nil
		},
	}
	c.Flags().DurationVar(&olderThan, "older-than", 0,
		"also evict store paths not pulled within this window (e.g. 720h)")
	return addAdminFlags(c)
}
