package cli

import (
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/stubbedev/xilo/internal/api"
	"github.com/stubbedev/xilo/internal/config"
	"github.com/stubbedev/xilo/internal/store"
)

// adminServer/adminToken are the shared --server/--token flags for admin
// commands (cache, token, gc). --server forces remote API mode.
var adminServer, adminToken string

// addAdminFlags registers the remote-mode flags on an admin command.
func addAdminFlags(c *cobra.Command) *cobra.Command {
	c.PersistentFlags().StringVar(&adminServer, "server", "", "manage a remote server via its API (default: local DB if present)")
	c.PersistentFlags().StringVar(&adminToken, "token", "", "admin token for --server (env XILO_TOKEN)")
	return c
}

func cacheCmd() *cobra.Command {
	c := &cobra.Command{Use: "cache", Short: "Manage caches"}
	c.AddCommand(cacheCreateCmd(), cacheListCmd(), cacheInfoCmd(),
		cacheConfigureCmd(), cacheRotateCmd(), cacheDestroyCmd())
	return addAdminFlags(c)
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
	db, err := openStore(cfg)
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
			apic, cfg, db, err := adminTarget(adminServer, adminToken)
			if err != nil {
				return err
			}
			if apic != nil {
				var ca api.Cache
				if err := apic.do(http.MethodPost, "/api/v1/caches",
					api.CreateCacheReq{Name: args[0], Public: !private, Priority: priority}, &ca); err != nil {
					return err
				}
				printCacheCreated(apic.base, ca.Name, ca.PubKey)
				return nil
			}
			defer db.Close()
			ca, err := db.CreateCache(args[0], !private, priority)
			if err != nil {
				return err
			}
			printCacheCreated(cfg.BaseURL, ca.Name, ca.PubKey)
			return nil
		},
	}
	c.Flags().BoolVar(&private, "private", false, "require a token to pull")
	c.Flags().IntVar(&priority, "priority", 40, "substituter priority (lower = preferred)")
	return c
}

func printCacheCreated(baseURL, name, pubkey string) {
	fmt.Printf("created cache %q\n\n", name)
	fmt.Printf("Add to nix.conf:\n")
	fmt.Printf("  substituters = %s/%s\n", baseURL, name)
	fmt.Printf("  trusted-public-keys = %s\n", pubkey)
}

func cacheListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List caches",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			apic, _, db, err := adminTarget(adminServer, adminToken)
			if err != nil {
				return err
			}
			var rows []api.Cache
			if apic != nil {
				if err := apic.do(http.MethodGet, "/api/v1/caches", nil, &rows); err != nil {
					return err
				}
			} else {
				defer db.Close()
				caches, err := db.ListCaches()
				if err != nil {
					return err
				}
				for i := range caches {
					rows = append(rows, apiCacheRow(&caches[i]))
				}
			}
			for _, ca := range rows {
				fmt.Printf("%-20s %-8s priority=%d  %s\n", ca.Name, visibility(ca.Public), ca.Priority, ca.PubKey)
			}
			return nil
		},
	}
}

// apiCacheRow converts a store cache to the wire shape for shared printing.
func apiCacheRow(c *store.Cache) api.Cache {
	return api.Cache{
		Name: c.Name, Public: c.Public, Priority: c.Priority,
		Retention: c.Retention, MaxBytes: c.MaxBytes, PubKey: c.PubKey, Created: c.Created,
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
			apic, cfg, db, err := adminTarget(adminServer, adminToken)
			if err != nil {
				return err
			}
			var d api.CacheDetail
			base := cfg.BaseURL
			if apic != nil {
				if err := apic.do(http.MethodGet, "/api/v1/caches/"+args[0], nil, &d); err != nil {
					return err
				}
				base = apic.base
			} else {
				defer db.Close()
				ca, err := db.GetCache(args[0])
				if err != nil {
					return err
				}
				st, err := db.CacheStats(ca.ID)
				if err != nil {
					return err
				}
				d = api.CacheDetail{Cache: apiCacheRow(ca), Paths: st.Paths, Chunks: st.Chunks,
					LogicalBytes: st.LogicalBytes, PhysicalBytes: st.PhysicalBytes}
			}
			fmt.Printf("name:        %s\n", d.Name)
			fmt.Printf("visibility:  %s\n", visibility(d.Public))
			fmt.Printf("priority:    %d\n", d.Priority)
			fmt.Printf("retention:   %s\n", retentionStr(d.Retention))
			fmt.Printf("max size:    %s\n", capStr(d.MaxBytes))
			fmt.Printf("public key:  %s\n", d.PubKey)
			fmt.Printf("substituter: %s/%s\n", base, d.Name)
			fmt.Printf("paths:       %d\n", d.Paths)
			fmt.Printf("chunks:      %d\n", d.Chunks)
			fmt.Printf("logical:     %d bytes\n", d.LogicalBytes)
			fmt.Printf("physical:    %d bytes (compressed, deduped)\n", d.PhysicalBytes)
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
			apic, _, db, err := adminTarget(adminServer, adminToken)
			if err != nil {
				return err
			}
			// Build the partial update from the flags actually given.
			var req api.ConfigureCacheReq
			if public {
				t := true
				req.Public = &t
			}
			if private {
				f := false
				req.Public = &f
			}
			if cmd.Flags().Changed("priority") {
				req.Priority = &priority
			}
			if cmd.Flags().Changed("retention") {
				secs := int64(retention.Seconds())
				req.Retention = &secs
			}
			if cmd.Flags().Changed("max-size") {
				b, err := config.ParseBytes(maxSize)
				if err != nil {
					return err
				}
				req.MaxBytes = &b
			}
			var ca api.Cache
			if apic != nil {
				if err := apic.do(http.MethodPatch, "/api/v1/caches/"+args[0], req, &ca); err != nil {
					return err
				}
			} else {
				defer db.Close()
				cur, err := db.GetCache(args[0])
				if err != nil {
					return err
				}
				ca = apiCacheRow(cur)
				if req.Public != nil {
					ca.Public = *req.Public
				}
				if req.Priority != nil {
					ca.Priority = *req.Priority
				}
				if req.Retention != nil {
					ca.Retention = *req.Retention
				}
				if req.MaxBytes != nil {
					ca.MaxBytes = *req.MaxBytes
				}
				if err := db.UpdateCache(cur.ID, ca.Public, ca.Priority, ca.Retention, ca.MaxBytes); err != nil {
					return err
				}
			}
			fmt.Printf("updated %s: %s priority=%d retention=%s max=%s\n",
				ca.Name, visibility(ca.Public), ca.Priority, retentionStr(ca.Retention), capStr(ca.MaxBytes))
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
			apic, _, db, err := adminTarget(adminServer, adminToken)
			if err != nil {
				return err
			}
			var name, pubkey string
			if apic != nil {
				var ca api.Cache
				if err := apic.do(http.MethodPost, "/api/v1/caches/"+args[0]+"/rotate", nil, &ca); err != nil {
					return err
				}
				name, pubkey = ca.Name, ca.PubKey
			} else {
				defer db.Close()
				ca, err := db.GetCache(args[0])
				if err != nil {
					return err
				}
				nc, err := db.RotateKey(ca.ID, ca.Name)
				if err != nil {
					return err
				}
				name, pubkey = nc.Name, nc.PubKey
			}
			fmt.Printf("rotated key for %s. Update trusted-public-keys everywhere:\n  %s\n", name, pubkey)
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
			apic, _, db, err := adminTarget(adminServer, adminToken)
			if err != nil {
				return err
			}
			if apic != nil {
				if err := apic.do(http.MethodDelete, "/api/v1/caches/"+args[0], nil, nil); err != nil {
					return err
				}
			} else {
				defer db.Close()
				ca, err := db.GetCache(args[0])
				if err != nil {
					return err
				}
				if err := db.DeleteCache(ca.ID); err != nil {
					return err
				}
			}
			fmt.Printf("destroyed cache %s\n", args[0])
			return nil
		},
	}
	c.Flags().BoolVar(&yes, "yes", false, "confirm destruction")
	return c
}

// openStore honors the configured commit durability.
func openStore(cfg *config.Config) (*store.DB, error) {
	if cfg.Durability == "full" {
		return store.OpenDurable(cfg.DBPath())
	}
	return store.Open(cfg.DBPath())
}
