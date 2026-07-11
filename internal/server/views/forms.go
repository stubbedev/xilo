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

// firstStr returns the first element or "".
func firstStr(s []string) string {
	if len(s) > 0 {
		return s[0]
	}
	return ""
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
		return "Instance-wide"
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
