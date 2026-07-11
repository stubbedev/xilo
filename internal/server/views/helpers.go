package views

import (
	"crypto/sha1"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/stubbedev/xilo/internal/store"
	"github.com/templui/templui/components/badge"
	"github.com/templui/templui/components/button"
	"github.com/templui/templui/components/progress"
)

// capVariant grades a usage bar: success (fine), warning (≥75%), danger (≥95%).
func capVariant(used, capacity int64) progress.Variant {
	switch fillClass(used, capacity) {
	case "over":
		return progress.VariantDanger
	case "warn":
		return progress.VariantWarning
	default:
		return progress.VariantSuccess
	}
}

// confirmVariant is the button variant for a confirm action's submit button.
func confirmVariant(danger bool) button.Variant {
	if danger {
		return button.VariantDestructive
	}
	return button.VariantDefault
}

// confirmTriggerProps builds the inline trigger button's props for a Confirm.
func confirmTriggerProps(c Confirm) button.Props {
	v := c.TriggerVariant
	if v == "" {
		v = button.VariantOutline
	}
	p := button.Props{Variant: v, Type: button.TypeButton, Size: button.SizeSm}
	if c.IconOnly {
		p.Size = button.SizeIcon
		if c.TriggerTooltip != "" {
			p.Attributes = templ.Attributes{"aria-label": c.TriggerTooltip, "title": c.TriggerTooltip}
		}
	}
	return p
}

// segVariant is the button variant for a segmented-control option.
func segVariant(active bool) button.Variant {
	if active {
		return button.VariantDefault
	}
	return button.VariantOutline
}

// toastDuration is the flash toast lifetime (ms). A flash paired with a secret
// lingers longer so the user reads it before copying.
func toastDuration(flash Flash) int {
	if flash.Code != "" {
		return 10000
	}
	return 5000
}

// statusVariant maps a token status to a badge variant.
func statusVariant(status string) badge.Variant {
	switch status {
	case "active":
		return badge.VariantDefault
	case "expired":
		return badge.VariantSecondary
	case "revoked":
		return badge.VariantDestructive
	default:
		return badge.VariantOutline
	}
}

// contextLabel is the current context-switcher label ("All accounts" or slug).
func contextLabel(nav Nav) string {
	if nav.Active == "" {
		return "All accounts"
	}
	return nav.Active
}

// initials derives up to two uppercase initials from a display name for avatars.
func initials(name string) string {
	fields := strings.Fields(name)
	if len(fields) == 0 {
		return "?"
	}
	first := []rune(fields[0])
	out := strings.ToUpper(string(first[0]))
	if len(fields) > 1 {
		last := []rune(fields[len(fields)-1])
		out += strings.ToUpper(string(last[0]))
	}
	return out
}

// copyID is a stable DOM id for a copy target, derived from its value.
func copyID(value string) string {
	sum := sha1.Sum([]byte(value))
	return "cp-" + hex.EncodeToString(sum[:6])
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

// parseDate parses a yyyy-mm-dd query value; zero time when empty/invalid.
func parseDate(s string) time.Time {
	t, _ := time.ParseInLocation("2006-01-02", s, time.Local)
	return t
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

// planLimits summarizes a plan's caps in one line.
func planLimits(p store.Plan) string {
	part := func(label string, v int64, fmtv string) string {
		if v == 0 {
			return ""
		}
		return label + " " + fmtv + " · "
	}
	out := part("caches", p.MaxCaches, itoa(p.MaxCaches)) +
		part("members", p.MaxMembers, itoa(p.MaxMembers)) +
		part("storage", p.MaxStorage, humanBytesV(p.MaxStorage)) +
		part("retention", p.MaxRetention, itoa(p.MaxRetention/86400)+"d")
	if out == "" {
		return T("plan.unlimited")
	}
	return strings.TrimSuffix(out, " · ")
}

// humanBytesV formats bytes without needing the injected formatter.
func humanBytesV(b int64) string {
	switch {
	case b >= 1<<40:
		return strconv.FormatInt(b>>40, 10) + " TiB"
	case b >= 1<<30:
		return strconv.FormatInt(b>>30, 10) + " GiB"
	case b >= 1<<20:
		return strconv.FormatInt(b>>20, 10) + " MiB"
	}
	return strconv.FormatInt(b, 10) + " B"
}
