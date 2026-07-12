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

// tokenNamespaceDefault preselects the viewing context for new tokens;
// non-admins (who cannot mint instance-wide tokens) fall back to an account.
func tokenNamespaceDefault(d DashboardData) string {
	if d.Nav.Active != "" {
		return d.Nav.Active
	}
	if !d.IsAdmin {
		return defaultAccount(d.Accounts)
	}
	return ""
}

// tokenNamespacePlaceholder names the empty selection: instance-wide is an
// admin-only concept.
func tokenNamespacePlaceholder(d DashboardData) string {
	if d.IsAdmin {
		return T("tokens.pickaccount")
	}
	return T("caches.pickaccount")
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

// tokenScopeValue is the cache-scope select value for a token ("*" = all).
func tokenScopeValue(t *store.Token) string {
	if t == nil || scopeAll(t.Caches) {
		return "*"
	}
	if t.AccountID != 0 {
		return t.Account + "/" + t.Caches[0]
	}
	return t.Caches[0]
}

// tokenAccountLabel is the read-only owning-namespace label on token edit.
func tokenAccountLabel(t *store.Token) string {
	if t == nil || t.AccountID == 0 {
		return T("tokens.instance")
	}
	return t.Account
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

// tokenPermanent reports whether a token's expiry switch defaults to permanent.
func tokenPermanent(t *store.Token) bool {
	return t != nil && t.Expires == 0
}

// tokenTTL is the prefilled TTL seconds for the duration input.
func tokenTTL(t *store.Token) int64 {
	if t == nil {
		return 30 * 86400
	}
	return Remaining(t.Expires)
}
