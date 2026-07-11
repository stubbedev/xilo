package cli

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/stubbedev/xilo/internal/api"
)

func tokenCmd() *cobra.Command {
	c := &cobra.Command{Use: "token", Short: "Manage push/pull tokens"}
	c.AddCommand(tokenCreateCmd(), tokenListCmd(), tokenRevokeCmd())
	return addAdminFlags(c)
}

func tokenCreateCmd() *cobra.Command {
	var caches []string
	var push, pull, admin bool
	var ttl time.Duration
	c := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a token (the secret is printed once)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var perms []string
			if push {
				perms = append(perms, "push")
			}
			if pull {
				perms = append(perms, "pull")
			}
			if admin {
				perms = append(perms, "admin")
			}
			if len(perms) == 0 {
				return fmt.Errorf("give at least one of --push / --pull / --admin")
			}
			var expires int64
			if ttl > 0 {
				expires = time.Now().Add(ttl).Unix()
			}
			apic, _, db, err := adminTarget(adminServer, adminToken)
			if err != nil {
				return err
			}
			var secret string
			var t api.Token
			if apic != nil {
				var resp api.CreateTokenResp
				if err := apic.do(http.MethodPost, "/api/v1/tokens",
					api.CreateTokenReq{Name: args[0], Caches: caches, Perms: perms, Expires: expires}, &resp); err != nil {
					return err
				}
				secret, t = resp.Secret, resp.Token
			} else {
				defer db.Close()
				sec, st, err := db.CreateToken(args[0], caches, perms, expires)
				if err != nil {
					return err
				}
				secret = sec
				t = api.Token{ID: st.ID, Name: st.Name, Caches: st.Caches, Perms: st.Perms, Expires: st.Expires}
			}
			scope := "all caches"
			if len(t.Caches) > 0 && t.Caches[0] != "*" {
				scope = strings.Join(t.Caches, ",")
			}
			fmt.Printf("token %q (id=%d) perms=%s scope=%s\n\n", t.Name, t.ID, strings.Join(perms, ","), scope)
			fmt.Printf("  %s\n\n", secret)
			fmt.Println("Store it now — it is not recoverable.")
			fmt.Println("Push:  export XILO_TOKEN=<secret>")
			fmt.Println("Pull:  add to ~/.netrc:  machine <host> login xilo password <secret>")
			return nil
		},
	}
	c.Flags().StringSliceVar(&caches, "cache", nil, "restrict to these caches (default: all)")
	c.Flags().BoolVar(&push, "push", false, "grant push")
	c.Flags().BoolVar(&pull, "pull", false, "grant pull")
	c.Flags().BoolVar(&admin, "admin", false, "grant management API access (remote cache/token/gc admin)")
	c.Flags().DurationVar(&ttl, "ttl", 0, "expire the token after this long (e.g. 720h); 0 = never")
	return c
}

func tokenListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List tokens",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			apic, _, db, err := adminTarget(adminServer, adminToken)
			if err != nil {
				return err
			}
			var toks []api.Token
			if apic != nil {
				if err := apic.do(http.MethodGet, "/api/v1/tokens", nil, &toks); err != nil {
					return err
				}
			} else {
				defer db.Close()
				list, err := db.ListTokens()
				if err != nil {
					return err
				}
				for _, t := range list {
					toks = append(toks, api.Token{ID: t.ID, Name: t.Name, Caches: t.Caches,
						Perms: t.Perms, Revoked: t.Revoked, Expires: t.Expires, Created: t.Created})
				}
			}
			now := time.Now().Unix()
			for _, t := range toks {
				state := "active"
				switch {
				case t.Revoked:
					state = "REVOKED"
				case t.Expires != 0 && now >= t.Expires:
					state = "EXPIRED"
				}
				exp := "never"
				if t.Expires != 0 {
					exp = time.Unix(t.Expires, 0).Format("2006-01-02")
				}
				fmt.Printf("%-4d %-16s %-8s perms=%s scope=%s expires=%s\n",
					t.ID, t.Name, state, strings.Join(t.Perms, ","), strings.Join(t.Caches, ","), exp)
			}
			return nil
		},
	}
}

func tokenRevokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <id>",
		Short: "Revoke a token by id (immediate)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("id must be a number: %w", err)
			}
			apic, _, db, err := adminTarget(adminServer, adminToken)
			if err != nil {
				return err
			}
			if apic != nil {
				if err := apic.do(http.MethodPost, "/api/v1/tokens/"+args[0]+"/revoke", nil, nil); err != nil {
					return err
				}
			} else {
				defer db.Close()
				if err := db.RevokeToken(id); err != nil {
					return err
				}
			}
			fmt.Printf("revoked token %d\n", id)
			return nil
		},
	}
}
