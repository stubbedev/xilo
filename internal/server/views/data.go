package views

import (
	"fmt"
	"time"

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

// scopeAll reports whether a token's cache list means "every cache".
func scopeAll(caches []string) bool {
	return len(caches) == 0 || (len(caches) == 1 && caches[0] == "*")
}

// retValue prefills the retention input with the current value in hours
// (retention granularity is coarse; hours read better than "720h0m0s").
func retValue(secs int64) string {
	if secs <= 0 {
		return ""
	}
	if secs%3600 == 0 {
		return fmt.Sprintf("%dh", secs/3600)
	}
	return (time.Duration(secs) * time.Second).String()
}

// capValue prefills the max-size input with the current cap (or empty).
func capValue(cap int64, bytes func(int64) string) string {
	if cap <= 0 {
		return ""
	}
	return bytes(cap)
}

// DashboardData is everything the caches dashboard renders.
type DashboardData struct {
	Global    store.Global
	Caches    []CacheUsage
	Tokens    []store.Token
	Flash     Flash
	ServerCap int64 // global storage cap bytes, 0 = unlimited
	Bytes     func(int64) string
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
