package cli

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/stubbedev/xilo/internal/api"
)

// statusCmd answers "what is this machine set up to do?" in one screen.
//
// The client keeps its state in four places — the saved profile, the managed
// block in nix.conf, ~/.netrc, and the token row on the server — and until
// this command existed none of them were readable from the CLI. A push that
// came back 401 gave you nothing to inspect.
func statusCmd() *cobra.Command {
	var url, token string
	c := &cobra.Command{
		Use:   "status",
		Short: "Show the active profile, token, cache and local Nix wiring",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cc := loadClientConfig()
			name := profileFlag
			if name == "" {
				name = cc.Default
			}
			url, token = resolveServer(url, token)

			row := func(label, value string) {
				fmt.Printf("%-10s %s\n", styleDim(label), value)
			}
			if name == "" && url == "" {
				fmt.Println(styleWarn("no server configured"))
				fmt.Println(styleDim("  run `xilo login <url> --token <token>` to save one"))
				return nil
			}
			if name != "" {
				row("profile", name+"  "+styleDim(clientConfigPath()))
			}
			if url == "" {
				fmt.Println(styleWarn("no server URL — pass --url or run `xilo login <url>`"))
				return nil
			}
			row("server", url+"  "+serverHealth(cmd.Context(), url))

			if token == "" {
				row("identity", styleWarn("no token saved — pushes and private pulls will 401"))
			} else {
				statusIdentity(row, url, token)
			}

			cache := defaultCache()
			if cache == "" {
				row("cache", styleDim("no default — pass one to `xilo push`, or `xilo use <account>/<cache> --default`"))
			} else {
				statusCache(cmd.Context(), row, url, cache, token)
			}
			statusLocalNix(row, url, cache)
			return nil
		},
	}
	c.Flags().StringVar(&url, "url", "", "server base URL (env XILO_URL / saved login)")
	c.Flags().StringVar(&token, "token", "", "token to describe (env XILO_TOKEN / saved login)")
	c.Flags().StringVarP(&profileFlag, "profile", "p", "", "saved server profile to inspect")
	return c
}

// serverHealth is a one-word reachability probe, so an unreachable server is
// distinguishable from a rejected token.
func serverHealth(ctx context.Context, url string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(url, "/")+"/healthz", nil)
	if err != nil {
		return styleWarn("unreachable")
	}
	hc := &http.Client{Timeout: 5 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return styleWarn("unreachable: " + err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return styleWarn("unhealthy: " + resp.Status)
	}
	return styleOK("reachable")
}

// statusIdentity reports what the saved token actually is, straight from the
// server — the answer no local file holds.
func statusIdentity(row func(string, string), url, token string) {
	who, err := newAPIClient(url, token).whoami()
	if err != nil {
		row("identity", styleWarn("token rejected: "+err.Error()))
		return
	}
	scope := who.Cache
	if scope == "" {
		scope = styleWarn("instance root (no cache scope)")
	}
	row("identity", fmt.Sprintf("%s → %s", styleAccent(who.Token), scope))
	row("perms", strings.Join(displayPerms(who), ", ")+"   "+expiryNote(who.Expires))
}

// displayPerms collapses the three management perms back into the single
// "manage" they are minted as, matching the dashboard.
func displayPerms(who *api.WhoamiResp) []string {
	manage := true
	for _, m := range []string{"create", "configure", "destroy"} {
		manage = manage && slices.Contains(who.Perms, m)
	}
	var out []string
	for _, p := range who.Perms {
		if manage && slices.Contains([]string{"create", "configure", "destroy"}, p) {
			continue
		}
		out = append(out, p)
	}
	if manage {
		out = append(out, "manage")
	}
	return out
}

// expiryNote renders a token expiry with the days left, since "2026-11-02"
// alone makes the reader do the subtraction.
func expiryNote(expires int64) string {
	if expires == 0 {
		return styleDim("expires never")
	}
	t := time.Unix(expires, 0)
	days := int(time.Until(t).Hours() / 24)
	if days < 0 {
		return styleWarn("expired " + t.Format("2006-01-02"))
	}
	return styleDim(fmt.Sprintf("expires %s (%d days)", t.Format("2006-01-02"), days))
}

// statusCache describes the default push/pull target.
func statusCache(ctx context.Context, row func(string, string), url, cache, token string) {
	cfg, err := fetchCacheConfig(ctx, url, cache, token)
	if err != nil {
		row("cache", styleAccent(cache)+"  "+styleWarn(err.Error()))
		return
	}
	row("cache", styleAccent(cache)+"  "+visibility(cfg.Public))
}

// statusLocalNix reports whether this machine's Nix is actually wired to the
// cache: a substituter line in the managed block, and a netrc entry for
// private pulls. Both are written by `xilo use` and invisible afterwards.
func statusLocalNix(row func(string, string), url, cache string) {
	body, _ := os.ReadFile(filepath.Join(configDir(), "nix", "nix.conf"))
	subs, keys := parseManagedBlock(string(body))
	switch {
	case len(subs) == 0:
		row("nix.conf", styleDim("no xilo substituters — run `xilo use <account>/<cache>`"))
	case cache != "" && !slices.Contains(subs, strings.TrimRight(url, "/")+"/c/"+cache):
		row("nix.conf", styleWarn(cache+" not wired")+styleDim(fmt.Sprintf("  (%d other substituter(s))", len(subs))))
	default:
		row("nix.conf", styleOK(fmt.Sprintf("%d substituter(s), %d trusted key(s)", len(subs), len(keys))))
	}

	host := hostOf(url)
	netrc, err := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".netrc"))
	switch {
	case err != nil:
		row("netrc", styleDim("no ~/.netrc — fine unless a cache you pull is private"))
	case strings.Contains(string(netrc), "machine "+host+" "), strings.Contains(string(netrc), "machine "+host+"\n"):
		row("netrc", styleOK("entry for "+host))
	default:
		row("netrc", styleWarn("no entry for "+host)+styleDim(" — private pulls will 401; run `xilo use <account>/<cache>`"))
	}
}
