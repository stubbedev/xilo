package views

import (
	"fmt"
	"net/url"
	"strconv"

	"github.com/stubbedev/xilo/internal/store"
)

// barMax is the <progress> max for a cache bar: its cap when capped, else the
// global stored total so bars are comparable (min 1 to avoid divide-by-zero).
func barMax(u CacheUsage, globalStored int64) int64 {
	if u.Cache.MaxBytes > 0 {
		return u.Cache.MaxBytes
	}
	if globalStored < 1 {
		return 1
	}
	return globalStored
}

func dedupRatio(logical, stored int64) string {
	if stored <= 0 {
		return "1.00"
	}
	return fmt.Sprintf("%.2f", float64(logical)/float64(stored))
}

// capSuffix appends " / <cap>" to the disk-used tile when a server cap is set.
func capSuffix(cap int64, bytes func(int64) string) string {
	if cap <= 0 {
		return ""
	}
	return " / " + bytes(cap)
}

// capLabel is like capSuffix for the per-cache stat tile.
func capLabel(cap int64, bytes func(int64) string) string {
	if cap <= 0 {
		return " (no cap)"
	}
	return " / " + bytes(cap)
}

// fillPct is the 0–100 fill of used against a cap (0 if uncapped).
func fillPct(used, capacity int64) int {
	if capacity <= 0 {
		return 0
	}
	p := used * 100 / capacity
	if p > 100 {
		p = 100
	}
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
	for k, vals := range s.Query {
		v[k] = vals
	}
	v.Set(s.SortParam, key)
	v.Set(s.DirParam, dir)
	v.Del(s.PageParam)
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
	pages := (len(items) + n - 1) / n
	if pages < 1 {
		pages = 1
	}
	if page < 1 {
		page = 1
	}
	if page > pages {
		page = pages
	}
	lo := (page - 1) * n
	hi := lo + n
	if hi > len(items) {
		hi = len(items)
	}
	return items[lo:hi], page, pages
}

// DashboardData is everything the caches dashboard renders.
type DashboardData struct {
	Global     store.Global
	Caches     []CacheUsage
	Tokens     []store.Token
	Flash      Flash
	ServerCap  int64 // global storage cap bytes, 0 = unlimited
	Bytes      func(int64) string
	CachePager Pager
	TokenPager Pager
	TokenSort  SortCtx
	CacheQuery string
	TokenQuery string
}

// CacheUsage is one cache plus its measured storage footprint.
type CacheUsage struct {
	Cache store.Cache
	Bytes int64 // compressed on-disk size of this cache's chunks
	Paths int64
}

// Pct returns the fill percentage against a cap (0..100), 0 if uncapped.
func (u CacheUsage) Pct() int {
	if u.Cache.MaxBytes <= 0 {
		return 0
	}
	p := u.Bytes * 100 / u.Cache.MaxBytes
	if p > 100 {
		p = 100
	}
	return int(p)
}

// Over reports whether the cache is at/over its cap.
func (u CacheUsage) Over() bool {
	return u.Cache.MaxBytes > 0 && u.Bytes >= u.Cache.MaxBytes
}
