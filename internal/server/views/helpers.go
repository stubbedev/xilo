package views

import (
	"time"

	"github.com/stubbedev/xilo/internal/store"
)

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
		return "never"
	}
	return time.Unix(t.Expires, 0).Format("2006-01-02")
}

// TokenActive reports whether a token can still be revoked (not already dead).
func TokenActive(t store.Token) bool {
	return !t.Revoked && !t.Expired(time.Now().Unix())
}
