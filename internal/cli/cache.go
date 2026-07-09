package cli

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/stubbedev/xilo/internal/config"
	"github.com/stubbedev/xilo/internal/store"
)

func cacheCmd() *cobra.Command {
	c := &cobra.Command{Use: "cache", Short: "Manage caches"}
	c.AddCommand(cacheCreateCmd(), cacheListCmd(), cacheInfoCmd(),
		cacheConfigureCmd(), cacheRotateCmd(), cacheDestroyCmd())
	return c
}

// openDB opens the metadata DB directly for one-shot admin commands.
// ponytail: rare manual op; busy_timeout covers the brief overlap with a
// running server. Concurrent *pushes* all go through the single server process.
func openDB() (*config.Config, *store.DB, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, nil, err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, nil, err
	}
	db, err := store.Open(cfg.DBPath())
	if err != nil {
		return nil, nil, err
	}
	return cfg, db, nil
}

func cacheCreateCmd() *cobra.Command {
	var private bool
	var priority int
	c := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a cache (generates its signing key)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, db, err := openDB()
			if err != nil {
				return err
			}
			defer db.Close()
			ca, err := db.CreateCache(args[0], !private, priority)
			if err != nil {
				return err
			}
			fmt.Printf("created cache %q\n\n", ca.Name)
			fmt.Printf("Add to nix.conf:\n")
			fmt.Printf("  substituters = %s/%s\n", cfg.BaseURL, ca.Name)
			fmt.Printf("  trusted-public-keys = %s\n", ca.PubKey)
			return nil
		},
	}
	c.Flags().BoolVar(&private, "private", false, "require a token to pull")
	c.Flags().IntVar(&priority, "priority", 40, "substituter priority (lower = preferred)")
	return c
}

func cacheListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List caches",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, db, err := openDB()
			if err != nil {
				return err
			}
			defer db.Close()
			caches, err := db.ListCaches()
			if err != nil {
				return err
			}
			for _, ca := range caches {
				fmt.Printf("%-20s %-8s priority=%d  %s\n", ca.Name, visibility(ca.Public), ca.Priority, ca.PubKey)
			}
			return nil
		},
	}
}

func visibility(public bool) string {
	if public {
		return "public"
	}
	return "private"
}

func cacheInfoCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "info <name>",
		Short: "Show a cache's settings and stats",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, db, err := openDB()
			if err != nil {
				return err
			}
			defer db.Close()
			ca, err := db.GetCache(args[0])
			if err != nil {
				return err
			}
			st, err := db.CacheStats(ca.ID)
			if err != nil {
				return err
			}
			fmt.Printf("name:        %s\n", ca.Name)
			fmt.Printf("visibility:  %s\n", visibility(ca.Public))
			fmt.Printf("priority:    %d\n", ca.Priority)
			fmt.Printf("retention:   %s\n", retentionStr(ca.Retention))
			fmt.Printf("max size:    %s\n", capStr(ca.MaxBytes))
			fmt.Printf("public key:  %s\n", ca.PubKey)
			fmt.Printf("substituter: %s/%s\n", cfg.BaseURL, ca.Name)
			fmt.Printf("paths:       %d\n", st.Paths)
			fmt.Printf("chunks:      %d\n", st.Chunks)
			fmt.Printf("logical:     %d bytes\n", st.LogicalBytes)
			fmt.Printf("physical:    %d bytes (compressed, deduped)\n", st.PhysicalBytes)
			return nil
		},
	}
}

func retentionStr(sec int64) string {
	if sec <= 0 {
		return "global default"
	}
	return (time.Duration(sec) * time.Second).String()
}

func cacheConfigureCmd() *cobra.Command {
	var priority int
	var public, private bool
	var retention time.Duration
	var maxSize string
	c := &cobra.Command{
		Use:   "configure <name>",
		Short: "Change a cache's visibility, priority, retention, or storage cap",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, db, err := openDB()
			if err != nil {
				return err
			}
			defer db.Close()
			ca, err := db.GetCache(args[0])
			if err != nil {
				return err
			}
			vis := ca.Public
			if public {
				vis = true
			}
			if private {
				vis = false
			}
			prio := ca.Priority
			if cmd.Flags().Changed("priority") {
				prio = priority
			}
			ret := ca.Retention
			if cmd.Flags().Changed("retention") {
				ret = int64(retention.Seconds())
			}
			maxBytes := ca.MaxBytes
			if cmd.Flags().Changed("max-size") {
				maxBytes, err = config.ParseBytes(maxSize)
				if err != nil {
					return err
				}
			}
			if err := db.UpdateCache(ca.ID, vis, prio, ret, maxBytes); err != nil {
				return err
			}
			fmt.Printf("updated %s: %s priority=%d retention=%s max=%s\n",
				ca.Name, visibility(vis), prio, retentionStr(ret), capStr(maxBytes))
			return nil
		},
	}
	c.Flags().BoolVar(&public, "public", false, "make the cache public")
	c.Flags().BoolVar(&private, "private", false, "make the cache private")
	c.Flags().IntVar(&priority, "priority", 40, "substituter priority")
	c.Flags().DurationVar(&retention, "retention", 0, "per-cache retention window (0 = global default)")
	c.Flags().StringVar(&maxSize, "max-size", "", "per-cache storage cap, e.g. 50GB (0 = unlimited)")
	return c
}

func capStr(b int64) string {
	if b <= 0 {
		return "unlimited"
	}
	return fmt.Sprintf("%d bytes", b)
}

func cacheRotateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rotate <name>",
		Short: "Generate a fresh signing key (invalidates the old trusted-public-key)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, db, err := openDB()
			if err != nil {
				return err
			}
			defer db.Close()
			ca, err := db.GetCache(args[0])
			if err != nil {
				return err
			}
			nc, err := db.RotateKey(ca.ID, ca.Name)
			if err != nil {
				return err
			}
			fmt.Printf("rotated key for %s. Update trusted-public-keys everywhere:\n  %s\n", nc.Name, nc.PubKey)
			return nil
		},
	}
}

func cacheDestroyCmd() *cobra.Command {
	var yes bool
	c := &cobra.Command{
		Use:   "destroy <name>",
		Short: "Delete a cache and its paths (chunks reclaimed on next GC)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !yes {
				return fmt.Errorf("refusing to destroy %q without --yes", args[0])
			}
			_, db, err := openDB()
			if err != nil {
				return err
			}
			defer db.Close()
			ca, err := db.GetCache(args[0])
			if err != nil {
				return err
			}
			if err := db.DeleteCache(ca.ID); err != nil {
				return err
			}
			fmt.Printf("destroyed cache %s\n", ca.Name)
			return nil
		},
	}
	c.Flags().BoolVar(&yes, "yes", false, "confirm destruction")
	return c
}
