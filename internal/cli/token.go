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
	var cache string
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
			// A token is valid for exactly one cache and belongs to the
			// account owning it, so it shows in that account's dashboard
			// view. Bare cache names mean default/. Admin-only tokens carry
			// no cache and are instance-wide.
			var caches []string
			var account string
			if cache != "" {
				var bare string
				account, bare = splitRef(cache)
				caches = []string{bare}
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
					api.CreateTokenReq{Name: args[0], Account: account, Caches: caches, Perms: perms, Expires: expires}, &resp); err != nil {
					return err
				}
				secret, t = resp.Secret, resp.Token
			} else {
				defer db.Close()
				var accountID int64
				if account != "" {
					acct, err := db.GetAccount(account)
					if err != nil {
						return fmt.Errorf("account %q: %w", account, err)
					}
					accountID = acct.ID
				}
				sec, st, err := db.CreateToken(accountID, args[0], caches, perms, expires)
				if err != nil {
					return err
				}
				secret = sec
				t = api.Token{ID: st.ID, Account: account, Name: st.Name, Caches: st.Caches, Perms: st.Perms, Expires: st.Expires}
			}
			scope := strings.Join(t.Caches, ",")
			if t.Account != "" {
				scope = t.Account + "/" + scope
			}
			fmt.Printf("%s token %s (id=%d) perms=%s scope=%s\n\n", styleOK("created"), styleAccent(t.Name), t.ID, strings.Join(perms, ","), scope)
			fmt.Printf("  %s\n\n", styleAccent(secret))
			fmt.Println(styleDim("Store it now — it is not recoverable."))
			fmt.Println("Push:  export XILO_TOKEN=<secret>")
			fmt.Println("Pull:  add to ~/.netrc:  machine <host> login xilo password <secret>")
			return nil
		},
	}
	c.Flags().StringVar(&cache, "cache", "", "the single cache this token is valid for (required unless --admin)")
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
					toks = append(toks, api.Token{ID: t.ID, Account: t.Account, Name: t.Name, Caches: t.Caches,
						Perms: t.Perms, Revoked: t.Revoked, Expires: t.Expires, Created: t.Created})
				}
			}
			now := time.Now().Unix()
			trows := make([][]string, 0, len(toks))
			for _, t := range toks {
				state := styleOK("active")
				switch {
				case t.Revoked:
					state = styleWarn("revoked")
				case t.Expires != 0 && now >= t.Expires:
					state = styleDim("expired")
				}
				exp := "never"
				if t.Expires != 0 {
					exp = time.Unix(t.Expires, 0).Format("2006-01-02")
				}
				scope := strings.Join(t.Caches, ",")
				if t.Account != "" {
					scope = t.Account + "/" + scope
				}
				trows = append(trows, []string{
					strconv.FormatInt(t.ID, 10), t.Name, state,
					strings.Join(t.Perms, ","), scope, exp,
				})
			}
			fmt.Println(renderTable([]string{"ID", "NAME", "STATUS", "PERMS", "SCOPE", "EXPIRES"}, trows))
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
