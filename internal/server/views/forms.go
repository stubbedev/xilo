package views

import "github.com/stubbedev/xilo/internal/store"

// defaultAccount picks the namespace preselected in the create-cache dialog:
// "default" when present, else the first account, else "".
func defaultAccount(accounts []store.Account) string {
	for _, a := range accounts {
		if a.Slug == "default" {
			return "default"
		}
	}
	if len(accounts) > 0 {
		return accounts[0].Slug
	}
	return ""
}

// activeOr prefers the viewing context over a fallback for dialog presets.
func activeOr(nav Nav, fallback string) string {
	if nav.Active != "" {
		return nav.Active
	}
	return fallback
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
		if cs := tokenScopeCaches(nil, d); len(cs) > 0 {
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

// tokenAccount is the read-only owning-account label: the stored owner when
// editing, the viewing context (falling back to the personal account) when
// creating. Pre-existing instance-wide tokens (CLI/API-minted) show as such.
func tokenAccount(t *store.Token, d DashboardData) string {
	if t == nil {
		return activeOr(d.Nav, d.Nav.UserName)
	}
	if t.AccountID == 0 {
		return T("tokens.instance")
	}
	return t.Account
}

// tokenScopeCaches narrows the scope picker to the token's account.
func tokenScopeCaches(t *store.Token, d DashboardData) []CacheUsage {
	acct := tokenAccount(t, d)
	if t != nil && t.AccountID == 0 {
		return d.AllCaches
	}
	var out []CacheUsage
	for _, u := range d.AllCaches {
		if u.Cache.Account == acct {
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
// permanent; new tokens never expire unless a TTL is chosen.
func tokenPermanent(t *store.Token) bool {
	return t == nil || t.Expires == 0
}
