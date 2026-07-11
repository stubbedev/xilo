package views

import (
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/stubbedev/xilo/internal/store"
)

// cls joins a base class with optional extras into one class attribute value.
func cls(base string, extra ...string) string {
	return strings.Join(append([]string{base}, extra...), " ")
}

// pathParts splits "/nix/store/<hash>-<name>" for compact display: an 8-char
// short hash and the package name. Unparseable paths return "" and the path.
func pathParts(p string) (hash, name string) {
	s, ok := strings.CutPrefix(p, "/nix/store/")
	if !ok {
		return "", p
	}
	hash, name, ok = strings.Cut(s, "-")
	if !ok {
		return "", s
	}
	if len(hash) > 8 {
		hash = hash[:8]
	}
	return hash, name
}

// TokenStatus is the display state of a token.
func TokenStatus(t store.Token) string {
	switch {
	case t.Revoked:
		return "revoked"
	case t.Expired(time.Now().Unix()):
		return "expired"
	default:
		return "active"
	}
}

// TokenExpiry renders a token's expiry as a date, or "never".
func TokenExpiry(t store.Token) string {
	if t.Expires == 0 {
		return T("tok.never")
	}
	return time.Unix(t.Expires, 0).Format("2006-01-02")
}

// TokenActive reports whether a token can still be revoked (not already dead).
func TokenActive(t store.Token) bool {
	return !t.Revoked && !t.Expired(time.Now().Unix())
}

// Setup snippets are built as Go strings so newlines and braces survive templ
// text handling verbatim, and the <pre> and copy button always match.

func snippetNixConf(d CacheData) string {
	return "extra-substituters = " + d.BaseURL + "/c/" + d.Cache.Ref() +
		"\nextra-trusted-public-keys = " + d.Cache.PubKey
}

func snippetFlake(d CacheData) string {
	return "nixConfig = {\n" +
		"  extra-substituters = [ \"" + d.BaseURL + "/c/" + d.Cache.Ref() + "\" ];\n" +
		"  extra-trusted-public-keys = [ \"" + d.Cache.PubKey + "\" ];\n" +
		"};"
}

func snippetCLI(d CacheData) string {
	return "xilo login " + d.BaseURL + " --token <token>\nxilo use " + d.Cache.Ref()
}

func snippetPush(d CacheData) string {
	return "XILO_URL=" + d.BaseURL + " XILO_TOKEN=<token> xilo push " + d.Cache.Ref() + " ./result"
}

// hxSwapAttrs makes a link (or its descendants) swap just one region via
// htmx instead of the boosted full-body navigation.
func hxSwapAttrs(target string) templ.Attributes {
	if target == "" {
		return nil
	}
	return templ.Attributes{
		"hx-target": target,
		"hx-select": target,
		"hx-swap":   "outerHTML show:none",
	}
}

// Remaining is the seconds until a unix expiry (0 for never/past), rounded
// up to whole days (or hours under two days) so the prefilled TTL reads as
// "60 days", not "1437 hours".
func Remaining(expires int64) int64 {
	if expires == 0 {
		return 0
	}
	left := expires - time.Now().Unix()
	switch {
	case left <= 0:
		return 0
	case left > 48*3600:
		return (left + 86399) / 86400 * 86400
	default:
		return (left + 3599) / 3600 * 3600
	}
}

// visValue is the form value for the visibility toggle's hidden input.
func visValue(private bool) string {
	if private {
		return "on"
	}
	return ""
}

// hasPerm reports whether a token carries a permission.
func hasPerm(t store.Token, perm string) bool {
	for _, p := range t.Perms {
		if p == perm {
			return true
		}
	}
	return false
}

// ariaSort maps a column's sort state to the aria-sort attribute value.
func ariaSort(s SortCtx, key string) string {
	if s.Key != key {
		return "none"
	}
	if s.Dir == "asc" {
		return "ascending"
	}
	return "descending"
}

// Ago renders a unix timestamp as a coarse relative time ("3h ago").
func Ago(ts int64) string {
	if ts <= 0 {
		return "never"
	}
	d := time.Since(time.Unix(ts, 0))
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return itoa(int64(d.Minutes())) + "m ago"
	case d < 24*time.Hour:
		return itoa(int64(d.Hours())) + "h ago"
	case d < 30*24*time.Hour:
		return itoa(int64(d.Hours()/24)) + "d ago"
	default:
		return time.Unix(ts, 0).Format("2006-01-02")
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
