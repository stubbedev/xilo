package cli

import (
	"fmt"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/stubbedev/xilo/internal/api"
	"github.com/stubbedev/xilo/internal/gc"
)

func gcCmd() *cobra.Command {
	var olderThan time.Duration
	c := &cobra.Command{
		Use:   "gc",
		Short: "Evict paths by retention and storage limits, then sweep unreferenced chunks",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			apic, cfg, db, err := adminTarget(adminServer, adminToken)
			if err != nil {
				return err
			}
			var res api.GCResp
			if apic != nil {
				req := api.GCReq{EvictOlderThan: int64(olderThan.Seconds())}
				if err := apic.do(http.MethodPost, "/api/v1/gc", req, &res); err != nil {
					return err
				}
			} else {
				defer db.Close()
				policy, err := cfg.GC.Policy()
				if err != nil {
					return err
				}
				sts, err := openStorages(cfg)
				if err != nil {
					return err
				}
				r, err := gc.Run(cmd.Context(), db, sts, gc.Sweep{
					Policy:        policy,
					ConfiguredCap: cfg.Limits.TotalBytes(),
					OlderThan:     olderThan,
				}, time.Now())
				if err != nil {
					return err
				}
				res = api.GCResp{Evicted: int64(r.Evicted), Deleted: r.Deleted, FreedBytes: r.Freed}
			}
			fmt.Printf("evicted %d paths\n", res.Evicted)
			fmt.Printf("removed %d chunks, freed %d bytes\n", res.Deleted, res.FreedBytes)
			return nil
		},
	}
	c.Flags().DurationVar(&olderThan, "older-than", 0,
		"also evict store paths not pulled within this window (e.g. 720h)")
	return addAdminFlags(c)
}
