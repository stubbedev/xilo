package views

import "github.com/stubbedev/xilo/internal/store"

// defaultAccount picks the namespace preselected in the create-cache dialog:
// "default" when present, else the first account, else "".
// defaultAccount is the account a create form starts on: the viewing context
// when one is set, otherwise the user's own personal account, otherwise the
// first they administer. It no longer prefers an account literally named
// "default" — nothing creates one any more, and treating that name as special
// was half of why the account layer read as magic.
func defaultAccount(d DashboardData) string {
	if d.Nav.Active != "" {
		return d.Nav.Active
	}
	for _, a := range d.Accounts {
		if a.Slug == d.Nav.UserName {
			return a.Slug
		}
	}
	if len(d.Accounts) > 0 {
		return d.Accounts[0].Slug
	}
	return ""
}

// firstStr returns the first element or "".
func firstStr(s []string) string {
	if len(s) > 0 {
		return s[0]
	}
	return ""
}

// planName is the plan name preset ("" on create).
func planName(p *store.Plan) string {
	if p == nil {
		return ""
	}
	return p.Name
}

// planLimit prefills a numeric plan cap; empty for 0/unlimited.
func planLimit(p *store.Plan, which string) string {
	if p == nil {
		return ""
	}
	v := p.MaxCaches
	if which == "members" {
		v = p.MaxMembers
	}
	if v == 0 {
		return ""
	}
	return itoa(v)
}

// planBytes is the storage cap preset (0 on create).
func planBytes(p *store.Plan) int64 {
	if p == nil {
		return 0
	}
	return p.MaxStorage
}

// planSecs is the retention cap preset (0 on create).
func planSecs(p *store.Plan) int64 {
	if p == nil {
		return 0
	}
	return p.MaxRetention
}

// tokenScopeValue is the cache-scope select value for a token; a new token
// preselects its account's first cache (admin-only tokens have no scope).
func tokenScopeValue(t *store.Token, d DashboardData) string {
	if t == nil {
		cs := tokenScopeCaches(nil, d)
		// Preselect a cache in the account being viewed, so the common case
		// (switch context, mint a token) needs no second pick.
		if acct := defaultAccount(d); acct != "" {
			for _, u := range cs {
				if u.Cache.Account == acct {
					return u.Cache.Ref()
				}
			}
		}
		if len(cs) > 0 {
			return cs[0].Cache.Ref()
		}
		return ""
	}
	if scopeAll(t.Caches) {
		return ""
	}
	if t.AccountID != 0 {
		return t.Account + "/" + t.Caches[0]
	}
	return t.Caches[0]
}

// canMintToken reports whether the acting account has a cache to scope a new
// token to — the create button stays disabled until it does.
func canMintToken(d DashboardData) bool {
	return len(tokenScopeCaches(nil, d)) > 0
}

// tokenScopeCaches lists the caches the scope picker may offer.
//
// On create that is every cache in view: the picked cache now decides the
// owning account, so narrowing the list by the sidebar's viewing context would
// hide caches the user can perfectly well mint for. On edit it stays inside
// the token's own account — a token cannot be moved between accounts.
func tokenScopeCaches(t *store.Token, d DashboardData) []CacheUsage {
	if t == nil || t.AccountID == 0 {
		return d.AllCaches
	}
	var out []CacheUsage
	for _, u := range d.AllCaches {
		if u.Cache.Account == t.Account {
			out = append(out, u)
		}
	}
	return out
}

// tokenName is the token name preset ("" on create).
func tokenName(t *store.Token) string {
	if t == nil {
		return ""
	}
	return t.Name
}

// tokenPerm reports a permission checkbox's default state; pull defaults on for
// new tokens.
func tokenPerm(t *store.Token, perm string) bool {
	if t == nil {
		return perm == "pull"
	}
	return hasPerm(*t, perm)
}

// tokenPermanent reports whether a token's expiry switch defaults to
// permanent. New tokens default to a TTL (30 days preselected); only an
// existing never-expiring token starts with the switch on.
func tokenPermanent(t *store.Token) bool {
	return t != nil && t.Expires == 0
}
