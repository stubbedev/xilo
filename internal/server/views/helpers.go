package views

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/stubbedev/xilo/internal/narinfo"
	"github.com/stubbedev/xilo/internal/store"
	"github.com/templui/templui/components/alert"
	"github.com/templui/templui/components/badge"
	"github.com/templui/templui/components/button"
	"github.com/templui/templui/components/progress"
	"github.com/templui/templui/components/toast"
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

// capTone colours a stored-bytes readout as it approaches its quota. It is
// what replaced the separate quota progress bar: the number already carried
// the quantity, only the urgency was missing.
func capTone(used, capacity int64) string {
	switch fillClass(used, capacity) {
	case "over":
		return "text-destructive"
	case "warn":
		return "text-warning"
	default:
		return ""
	}
}

// capBar is capTone's bar-fill twin: a quota bar is the same grading in
// paint. Under 75% it stays neutral — a bar that is green from 1% on says
// "good" about a cache that is simply empty.
func capBar(used, capacity int64) string {
	switch fillClass(used, capacity) {
	case "over":
		return "bg-destructive"
	case "warn":
		return "bg-warning"
	default:
		return "bg-foreground/60"
	}
}

// sectionAction is a ListSection's header control, or nothing when the list is
// empty: the empty state carries the create button in that case, and two of
// them a few pixels apart is one too many.
func sectionAction(show bool, c templ.Component) templ.Component {
	if !show {
		return nil
	}
	return c
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
		p.Class = "size-8"
		if c.TriggerTooltip != "" {
			p.Attributes = templ.Attributes{"aria-label": c.TriggerTooltip}
		}
	}
	return p
}

// chipVariant is the filter-chip variant: both states borderless so the row
// keeps one height (outline adds a border the primary variant lacks). Selected
// is a filled neutral, not the primary colour — which state a filter is in is
// not an action, and primary is spoken for.
func chipVariant(active bool) button.Variant {
	if active {
		return button.VariantSecondary
	}
	return button.VariantGhost
}

// segVariant is the button variant for a segmented-control option — same
// reasoning as chipVariant.
func segVariant(active bool) button.Variant {
	if active {
		return button.VariantSecondary
	}
	return button.VariantGhost
}

// toastDuration is the flash toast lifetime (ms). A flash paired with a secret
// lingers longer so the user reads it before copying.
func toastDuration(flash Flash) int {
	if flash.Code != "" {
		return 10000
	}
	return 5000
}

// authAlertVariant styles auth-page flashes: destructive unless marked OK.
func authAlertVariant(f Flash) alert.Variant {
	if f.OK {
		return alert.VariantDefault
	}
	return alert.VariantDestructive
}

// dialogWidth is the DialogBox content class: md for confirms and one-field
// forms, lg for multi-field forms.
func dialogWidth(wide bool) string {
	if wide {
		return "text-left sm:max-w-lg"
	}
	return "text-left sm:max-w-md"
}

// rowActionType picks submit vs plain button for a RowAction.
func rowActionType(submit bool) button.Type {
	if submit {
		return button.TypeSubmit
	}
	return button.TypeButton
}

// healthTone colors the health word on the status page. The poll script
// toggles these same two classes, so they have to be the ones it knows.
func healthTone(healthy bool) string {
	if healthy {
		return "text-success"
	}
	return "text-destructive"
}

// failTone colors the failed-requests tile once there is anything to see.
func failTone(failed int64) string {
	if failed > 0 {
		return "text-destructive"
	}
	return ""
}

// auditMethodVariant colors an HTTP method badge so destructive actions stand
// out in activities at a glance.
func auditMethodVariant(method string) badge.Variant {
	switch method {
	case "DELETE":
		return badge.VariantDestructive
	case "GET":
		return badge.VariantOutline
	default:
		return badge.VariantSecondary
	}
}

// auditStatusVariant colors an HTTP status badge: client/server errors red,
// redirects muted, success neutral.
func auditStatusVariant(status int) badge.Variant {
	switch {
	case status >= 400:
		return badge.VariantDestructive
	case status >= 300:
		return badge.VariantOutline
	default:
		return badge.VariantSecondary
	}
}

// userRole is the RoleIcon key for an instance user: the owner is the
// superadmin, an unapproved sign-up shows as pending.
func userRole(u store.User) string {
	switch {
	case u.Status == "pending":
		return "pending"
	case u.Superadmin():
		return "superadmin"
	default:
		return "user"
	}
}

// planPrice / planInterval / planExternal fill the plan dialog; p is nil on
// create, where a plan starts free.
func planPrice(p *store.Plan) string {
	if p == nil || p.PriceCents == 0 {
		return ""
	}
	return strconv.FormatFloat(float64(p.PriceCents)/100, 'f', -1, 64)
}

func planInterval(p *store.Plan) string {
	if p == nil {
		return "month"
	}
	return p.Interval
}

func planExternal(p *store.Plan) string {
	if p == nil {
		return ""
	}
	return p.ExternalID
}

// PlanPriceLabel is what a plan costs, in the catalogue and wherever a tenant
// is shown their plan. A plan with no price is free — which is every plan on a
// self-hosted instance, so it says so plainly rather than showing "0.00".
func PlanPriceLabel(ctx context.Context, p store.Plan) string {
	if p.PriceCents == 0 {
		return T(ctx, "plan.free")
	}
	amount := strconv.FormatFloat(float64(p.PriceCents)/100, 'f', 2, 64)
	if p.Interval == "year" {
		return Tf(ctx, "plan.peryear", amount)
	}
	return Tf(ctx, "plan.permonth", amount)
}

// accountStatusIcon / accountStatusTone map an account's lifecycle state to
// its glyph and color: paused is a warning, closed is a refusal.
func accountStatusIcon(status string) string {
	if status == "suspended" {
		return "circle-slash"
	}
	return "circle-pause"
}

func accountStatusTone(status string) string {
	if status == "suspended" {
		return "text-destructive"
	}
	return "text-warning"
}

// roleIcon / roleTone map a role to its glyph and color.
func roleIcon(role string) string {
	switch role {
	case "superadmin":
		return "shield-check"
	case "owner":
		return "crown"
	case "admin":
		return "shield"
	case "pending":
		return "clock"
	default:
		return "user"
	}
}

func roleTone(role string) string {
	switch role {
	case "superadmin", "owner":
		return "text-primary"
	case "pending":
		return "text-muted-foreground"
	default:
		return ""
	}
}

// statusIcon / statusTone map a token status to its glyph and color.
func statusIcon(status string) string {
	switch status {
	case "active":
		return "circle-check"
	case "revoked":
		return "ban"
	default:
		return "clock"
	}
}

func statusTone(status string) string {
	switch status {
	case "active":
		return "text-success"
	case "revoked":
		return "text-destructive"
	default:
		return "text-muted-foreground"
	}
}

// copyID is a stable DOM id for a copy target, derived from its value.
func copyID(value string) string {
	sum := sha1.Sum([]byte(value))
	return "cp-" + hex.EncodeToString(sum[:6])
}

// pathParts splits "/nix/store/<hash>-<name>" into the store hash and the
// package name. The hash comes back whole — StoreHash decides how much of it
// reads at full contrast. Unparseable paths return "" and the path.
func pathParts(p string) (hash, name string) {
	s, ok := strings.CutPrefix(p, "/nix/store/")
	if !ok {
		return "", p
	}
	hash, name, ok = strings.Cut(s, "-")
	if !ok {
		return "", s
	}
	return hash, name
}

// hashHead and hashTail split a store hash where the eye does: nobody reads
// 32 base32 characters, they match the first few and skim the rest. StoreHash
// renders the head at full contrast and the tail dimmed. Either half may be
// empty — a hash already shortened to 8 has no tail.
func hashHead(h string) string {
	if len(h) > 8 {
		return h[:8]
	}
	return h
}

func hashTail(h string) string {
	if len(h) > 8 {
		return h[8:]
	}
	return ""
}

// pathURL is the admin detail page for a store path in a cache.
func pathURL(c store.Cache, storePath string) string {
	return "/admin/cache/" + c.Ref() + "/path/" + narinfo.StoreHash(storePath)
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
func TokenExpiry(ctx context.Context, t store.Token) string {
	if t.Expires == 0 {
		return T(ctx, "tok.never")
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

// snippetToken is the token to render into a snippet: the real one when a
// token was just minted on this page, otherwise a placeholder to replace.
func snippetToken(d CacheData) string {
	if d.Secret != "" {
		return d.Secret
	}
	return "<token>"
}

// snippetNetrc is the ~/.netrc line a private cache needs. Nix sends the token
// as HTTP basic auth on pulls, so a substituter line alone gets a 401 — the
// page used to show only the substituter and leave that to be discovered.
func snippetNetrc(d CacheData) string {
	return "machine " + d.Host + " login xilo password " + snippetToken(d)
}

func snippetFlake(d CacheData) string {
	return "nixConfig = {\n" +
		"  extra-substituters = [ \"" + d.BaseURL + "/c/" + d.Cache.Ref() + "\" ];\n" +
		"  extra-trusted-public-keys = [ \"" + d.Cache.PubKey + "\" ];\n" +
		"};"
}

func snippetCLI(d CacheData) string {
	return "xilo login " + d.BaseURL + " --token " + snippetToken(d) +
		"\nxilo use " + d.Cache.Ref()
}

func snippetPush(d CacheData) string {
	return "xilo login " + d.BaseURL + " --token " + snippetToken(d) +
		"\nxilo push " + d.Cache.Ref() + " ./result"
}

// hxSwapAttrs makes a link fetch `url` via htmx and swap just one region in
// place of a full-page navigation; plain-anchor fallback still works.
func hxSwapAttrs(url, target string) templ.Attributes {
	if url == "" || target == "" {
		return nil
	}
	return templ.Attributes{
		"hx-get":       url,
		"hx-target":    target,
		"hx-select":    target,
		"hx-swap":      "outerHTML show:none",
		"hx-indicator": target,
		"hx-push-url":  "true",
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

// hasPerm reports whether a token carries a permission. "manage" is the one
// switch standing for the three per-cache management perms, so it is set only
// when the token carries all of them.
func hasPerm(t store.Token, perm string) bool {
	if perm == "manage" {
		for _, m := range store.ManagePerms {
			if !slices.Contains(t.Perms, m) {
				return false
			}
		}
		return true
	}
	return slices.Contains(t.Perms, perm)
}

// displayPerms is the badge list for a token: the three management perms
// collapse back into the single "manage" they were minted as, so the table
// reads the way the form that created it did.
func displayPerms(t store.Token) []string {
	var out []string
	for _, p := range t.Perms {
		if slices.Contains(store.ManagePerms, p) {
			continue
		}
		out = append(out, p)
	}
	if hasPerm(t, "manage") {
		out = append(out, "manage")
	}
	return out
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
func Ago(ctx context.Context, ts int64) string {
	if ts <= 0 {
		return T(ctx, "tok.never")
	}
	d := time.Since(time.Unix(ts, 0))
	switch {
	case d < time.Minute:
		return T(ctx, "time.justnow")
	case d < time.Hour:
		return Tf(ctx, "time.mago", int64(d.Minutes()))
	case d < 24*time.Hour:
		return Tf(ctx, "time.hago", int64(d.Hours()))
	case d < 30*24*time.Hour:
		return Tf(ctx, "time.dago", int64(d.Hours()/24))
	default:
		return time.Unix(ts, 0).Format("2006-01-02")
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// stamp renders a unix timestamp as an absolute local datetime — used where the
// exact moment matters (activities), paired with Ago for the coarse view.
func stamp(ts int64) string {
	if ts <= 0 {
		return ""
	}
	return time.Unix(ts, 0).Format("2006-01-02 15:04:05")
}

// planLimits summarizes a plan's caps in one line.
func planLimits(ctx context.Context, p store.Plan) string {
	part := func(label string, v int64, fmtv string) string {
		if v == 0 {
			return ""
		}
		return label + " " + fmtv + MetaSep
	}
	out := part(T(ctx, "plan.caches"), p.MaxCaches, itoa(p.MaxCaches)) +
		part(T(ctx, "plan.members"), p.MaxMembers, itoa(p.MaxMembers)) +
		part(T(ctx, "plan.storage"), p.MaxStorage, humanBytesV(p.MaxStorage)) +
		part(T(ctx, "plan.retention"), p.MaxRetention, itoa(p.MaxRetention/86400)+"d")
	if out == "" {
		return T(ctx, "plan.unlimited")
	}
	return strings.TrimSuffix(out, MetaSep)
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

// toastKind is one entry in the client-side toast catalogue. The toast layer
// (toastLayer, layout.templ) renders a <template> per kind; fire one with
// data-toast="<Key>" on any element, or xiloToast('<Key>', 'optional text')
// from script. Adding a toast means adding a row here plus its i18n key —
// never a second <template> and listener pair.
type toastKind struct {
	Key      string        // data-toast value / xiloToast() argument
	MsgKey   string        // i18n key for the default message
	Variant  toast.Variant // decides the icon and the accent colour
	Duration int           // ms on screen
}

var toastKinds = []toastKind{
	{Key: "copied", MsgKey: "copy.done", Variant: toast.VariantSuccess, Duration: 2000},
	{Key: "error", MsgKey: "toast.error", Variant: toast.VariantError, Duration: 6000},
}

// orgCreateAction is where the create-organization form posts: instance admins
// create for the instance, everyone else for themselves.
func orgCreateAction(isAdmin bool) string {
	if isAdmin {
		return "/admin/orgs"
	}
	return "/admin/neworg"
}

// canDeleteOrg reports whether the viewer may delete this organization — the
// same rule the handler enforces: instance admin, or the organization's owner.
func canDeleteOrg(d OrgsData, info OrgInfo) bool {
	if d.IsAdmin {
		return true
	}
	for _, m := range info.Members {
		if m.UserName == d.Nav.UserName && m.Role == "owner" {
			return true
		}
	}
	return false
}

// orDash is a value or an em dash: a settings row with nothing set says so in
// the value column rather than leaving a hole.
func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// totpState is the word a two-factor row shows: enabled or disabled.
func totpState(ctx context.Context, on bool) string {
	if on {
		return T(ctx, "set.2fa.on")
	}
	return T(ctx, "set.2fa.off")
}

// chartMax is the ceiling a status series is drawn against: the next
// 1/2/5×10ⁿ above its peak. Round ceilings put ticks on round numbers (an
// all-zero series stopped being labelled 0.2, 0.4, 0.6…), and a ceiling that
// only moves between magnitudes keeps the axis still while the poller feeds
// new points in under the line.
func chartMax(points []float64) *float64 {
	m := 0.0
	for _, v := range points {
		m = max(m, v)
	}
	if m <= 0 {
		one := 1.0
		return &one
	}
	e := math.Pow(10, math.Floor(math.Log10(m)))
	for _, f := range []float64{1, 2, 5} {
		if m <= f*e {
			v := f * e
			return &v
		}
	}
	v := 10 * e
	return &v
}

// searchThreshold is the shortest list worth a search box inside a selectbox.
// A list you can take in at a glance is faster to point at than to type into,
// and the box costs a row of the popover, a focus stop and a keystroke of
// doubt about whether typing filters or types into the field.
const searchThreshold = 4

// noSearch answers selectbox.ContentProps.NoSearch for a list of n options.
func noSearch(n int) bool { return n <= searchThreshold }

// vtStyle names an element for the View Transitions API, so the same object
// keeps its identity across a navigation: the cache line on the dashboard and
// the title of the cache page share a name and the browser flies one into the
// other instead of cross-fading both. The name is derived from the text, which
// is what makes the two sides agree without threading an id through the page —
// so it must be something unique on the page (a cache ref, a page title). Two
// elements sharing a name abort the whole transition.
func vtStyle(s string) string {
	var b strings.Builder
	b.WriteString("view-transition-name:vt-")
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + 32)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
