package views

import (
	"context"
	"fmt"
	"maps"
	"net/url"
	"strconv"
	"strings"

	"github.com/stubbedev/xilo/internal/store"
)

// unlimitedClass styles an uncapped cache's bar: nothing bounds it, so a
// partial fill is meaningless (raw MiB read as a percentage). It renders as a
// FULL striped bar — green while healthy, warn-colored when the server-wide
// cap is under pressure (global eviction will bite uncapped caches too).
func unlimitedClass(globalStored, serverCap int64) string {
	if c := fillClass(globalStored, serverCap); c != "" {
		return "unlimited " + c
	}
	return "unlimited"
}

func dedupRatio(logical, stored int64) string {
	if stored <= 0 {
		return "1.00"
	}
	return fmt.Sprintf("%.2f", float64(logical)/float64(stored))
}

// firstRun and noTokensYet mark the states where a list has nothing in it and
// nothing is being filtered. The list's own controls — search over an empty
// list, a create button duplicating the one in the empty state — are hidden
// there; a filtered list that came back empty keeps them, because the search
// box is how you get out of that.
func firstRun(d DashboardData) bool {
	return len(d.Caches) == 0 && d.CacheQuery == ""
}

func noTokensYet(d DashboardData) bool {
	return len(d.Tokens) == 0 && d.TokenQuery == ""
}

// savedBytes is what deduplication kept off the disk. Never negative: a fresh
// instance can report more stored than logical for a moment while a push is
// still being accounted for, and "-2 KiB saved" reads as a bug.
func savedBytes(logical, physical int64) int64 {
	return max(logical-physical, 0)
}

// capSuffix appends " / <cap>" to the disk-used tile when a server cap is set.
func capSuffix(cap int64, bytes func(int64) string) string {
	if cap <= 0 {
		return ""
	}
	return " / " + bytes(cap)
}

// capLabel is like capSuffix for the per-cache stat tile.
func capLabel(ctx context.Context, cap int64, bytes func(int64) string) string {
	if cap <= 0 {
		return " (" + T(ctx, "cache.nocap") + ")"
	}
	return " / " + bytes(cap)
}

// fillPct is the 0–100 fill of used against a cap (0 if uncapped).
func fillPct(used, capacity int64) int {
	if capacity <= 0 {
		return 0
	}
	p := min(used*100/capacity, 100)
	return int(p)
}

// fillClass grades a capped usage bar: "" (fine), "warn" (≥75%), "over" (≥95%).
func fillClass(used, capacity int64) string {
	switch p := fillPct(used, capacity); {
	case capacity <= 0:
		return ""
	case p >= 95:
		return "over"
	case p >= 75:
		return "warn"
	default:
		return ""
	}
}

// scopeAll reports whether a token's cache list means "every cache".
func scopeAll(caches []string) bool {
	return len(caches) == 0 || (len(caches) == 1 && caches[0] == "*")
}

// durParts splits a duration in seconds into a value + unit (h/d/mo/y) for a
// number-input + unit-select pair, picking the largest unit that divides
// evenly (months = 30 d, years = 365 d). Zero → empty value, days preselected.
func durParts(secs int64) (string, string) {
	switch {
	case secs <= 0:
		return "", "d"
	case secs%31536000 == 0:
		return strconv.FormatInt(secs/31536000, 10), "y"
	case secs%2592000 == 0:
		return strconv.FormatInt(secs/2592000, 10), "mo"
	case secs%86400 == 0:
		return strconv.FormatInt(secs/86400, 10), "d"
	default:
		// ponytail: sub-hour retention rounds to hours; nobody GCs by the minute
		return strconv.FormatInt(secs/3600, 10), "h"
	}
}

// sizeParts splits a byte count into a value + binary unit for a number-input
// + unit-select pair. Zero → empty value, GiB preselected.
func sizeParts(b int64) (string, string) {
	switch {
	case b <= 0:
		return "", "GiB"
	case b%(1<<40) == 0:
		return strconv.FormatInt(b>>40, 10), "TiB"
	case b%(1<<30) == 0:
		return strconv.FormatInt(b>>30, 10), "GiB"
	default:
		// ponytail: caps come from this form in whole MiB; stray bytes round down
		return strconv.FormatInt(b>>20, 10), "MiB"
	}
}

// durValue / durUnit split a duration for the DurationInput field pair.
func durValue(secs int64) string { v, _ := durParts(secs); return v }
func durUnit(secs int64) string  { _, u := durParts(secs); return u }

// sizeValue / sizeUnit split a byte count for the SizeInput field pair.
func sizeValue(b int64) string { v, _ := sizeParts(b); return v }
func sizeUnit(b int64) string  { _, u := sizeParts(b); return u }

// SortCtx carries a table's active sort and everything needed to build
// header links: click a column to sort ascending, click again to flip.
type SortCtx struct {
	Path                string     // request path the links point at
	Query               url.Values // current query params to preserve
	SortParam, DirParam string     // param names, e.g. "sort"/"dir" or "tokens[sort]"
	PageParam           string     // page-number param to reset on sort change
	Key, Dir            string     // active sort ("" key = default order)
	Target              string     // htmx swap region selector ("" = inherit/boost)
}

// URL builds the header link for a column key.
func (s SortCtx) URL(key string) string {
	dir := "asc"
	if s.Key == key && s.Dir == "asc" {
		dir = "desc"
	}
	v := url.Values{}
	maps.Copy(v, s.Query)
	v.Set(s.SortParam, key)
	v.Set(s.DirParam, dir)
	v.Del(s.PageParam)
	return s.Path + "?" + v.Encode()
}

// With rebuilds the listing URL with one query param set (removed when val is
// empty) and paging reset: filter chips that compose with the active search
// and sort.
func (s SortCtx) With(key, val string) string {
	v := url.Values{}
	maps.Copy(v, s.Query)
	if val == "" {
		v.Del(key)
	} else {
		v.Set(key, val)
	}
	v.Del(s.PageParam)
	if len(v) == 0 {
		return s.Path
	}
	return s.Path + "?" + v.Encode()
}

// Pager describes one paginated listing: prev/next hrefs (empty = disabled)
// and the 1-based position. Pages <= 1 renders nothing.
type Pager struct {
	Prev, Next  string
	Page, Pages int
	Target      string // htmx swap region selector ("" = inherit/boost)
}

// PageOf slices items for a 1-based page of size n, clamping page into range.
// Returns the slice, the clamped page, and the page count (min 1).
func PageOf[T any](items []T, page, n int) ([]T, int, int) {
	pages := max((len(items)+n-1)/n, 1)
	if page < 1 {
		page = 1
	}
	if page > pages {
		page = pages
	}
	lo := (page - 1) * n
	hi := min(lo+n, len(items))
	return items[lo:hi], page, pages
}

// DashboardData is everything the caches dashboard renders.
type DashboardData struct {
	Nav        Nav
	Global     store.Global
	Caches     []CacheUsage // current page of the (possibly searched) cache list
	AllCaches  []CacheUsage // every visible cache — scope pickers, preconditions
	Tokens     []store.Token
	Accounts   []store.Account // accounts the viewer can create caches/tokens in
	Storages   []string        // configured blob backends, default first
	IsAdmin    bool
	Flash      Flash
	ServerCap  int64 // global storage cap bytes, 0 = unlimited
	Bytes      func(int64) string
	CachePager Pager
	TokenPager Pager
	TokenSort  SortCtx
	CacheQuery string
	TokenQuery string
}

// OrgInfo is one account with its membership and usage, for the settings page.
type OrgInfo struct {
	Account store.Account
	Members []store.AccountMember
	Plan    *store.Plan // nil = no plan (unlimited)
	Used    int64       // logical bytes stored
	Egress  int64       // NAR bytes served this month
}

// MetaSep separates the clauses of a meta line. An em space, not a middle
// dot: the dot was punctuation pretending to be structure, and three of them
// in a row ("0 B stored · 1 member · plan free") read as one long sentence
// rather than three facts. Space is not a character the browser can collapse
// here — U+2003 is not ASCII whitespace — so it survives in prose and in mono.
const MetaSep = " "

// UsageLine renders "12 GiB stored (of 50 GiB)   3 GiB egress this month".
func (o OrgInfo) UsageLine(ctx context.Context) string {
	// One format string per clause: a translator moves the words inside a
	// clause, we only decide which clauses appear.
	out := Tf(ctx, "acct.usestored", humanBytesV(o.Used))
	if o.Plan != nil && o.Plan.MaxStorage > 0 {
		out = Tf(ctx, "acct.useof", humanBytesV(o.Used), humanBytesV(o.Plan.MaxStorage))
	}
	if o.Egress > 0 {
		out += MetaSep + Tf(ctx, "acct.useegress", humanBytesV(o.Egress))
	}
	if o.Plan != nil {
		out += MetaSep + Tf(ctx, "acct.useplan", o.Plan.Name)
	}
	return out
}

// CacheUsage is one cache plus its measured storage footprint.
type CacheUsage struct {
	Cache   store.Cache
	Bytes   int64 // compressed on-disk size of this cache's chunks
	Logical int64 // sum of NarSize (for the tenant dedup tile)
	Paths   int64
}

// Pct returns the fill percentage against a cap (0..100), 0 if uncapped.
func (u CacheUsage) Pct() int {
	if u.Cache.MaxBytes <= 0 {
		return 0
	}
	p := min(u.Bytes*100/u.Cache.MaxBytes, 100)
	return int(p)
}

// Over reports whether the cache is at/over its cap.
func (u CacheUsage) Over() bool {
	return u.Cache.MaxBytes > 0 && u.Bytes >= u.Cache.MaxBytes
}

// Count condenses large counters for stat tiles: 21393 → "21.4k", 2_100_000 →
// "2.1m". Below 1000 the raw number reads fine and stays exact. Values whose
// one-decimal rounding reaches 1000 bump to the next unit (999_999 → "1m",
// never "1000.0k").
func Count(n int64) string {
	if n < 1000 {
		return strconv.FormatInt(n, 10)
	}
	v := float64(n)
	for _, unit := range []string{"k", "m", "b"} {
		v /= 1000
		if v < 999.95 || unit == "b" {
			return trimZero(fmt.Sprintf("%.1f", v)) + unit
		}
	}
	return "" // unreachable
}

// trimZero drops a trailing ".0" so round values read clean ("12k" not "12.0k").
func trimZero(s string) string { return strings.TrimSuffix(s, ".0") }
