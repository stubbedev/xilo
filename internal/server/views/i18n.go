package views

// T returns the UI string for a message id. All user-facing labels,
// placeholders, and helper text flow through here so they are DRY and
// translatable — add a locale map and select by request language later.
func T(id string) string {
	if s, ok := en[id]; ok {
		return s
	}
	return id
}

// en is the default (English) catalog. Keep values concise.
var en = map[string]string{
	// nav / chrome
	"nav.caches":     "Caches",
	"nav.settings":   "Settings",
	"nav.logout":     "Log out",
	"nav.theme":      "Toggle theme",
	"footer.tagline": "xilo · self-hosted Nix cache",
	"footer.health":  "Health",
	"footer.metrics": "Metrics",

	// login
	"login.title":      "Sign in",
	"login.subtitle":   "Manage caches and tokens.",
	"login.password":   "Password",
	"login.code":       "2FA code",
	"login.submit":     "Sign in",
	"login.nopassword": "No admin password set. Configure admin.password or XILO_ADMIN_PASSWORD.",

	// dashboard — overview
	"dash.title":    "Overview",
	"dash.subtitle": "Nix binary caches on this instance.",
	"kpi.caches":    "caches",
	"kpi.paths":     "paths",
	"kpi.disk":      "disk used",
	"kpi.dedup":     "dedup",
	"kpi.logical":   "logical",
	"kpi.physical":  "on disk",
	"kpi.chunks":    "chunks",
	"cache.paths":   "paths",
	"cache.nocap":   "no cap",

	// caches
	"caches.title":    "Caches",
	"caches.empty":    "No caches yet.",
	"caches.new":      "New cache",
	"caches.name":     "cache name",
	"caches.private":  "private",
	"caches.priority": "priority",
	"caches.create":   "Create",

	// tokens
	"tokens.title":   "Tokens",
	"tokens.empty":   "No tokens.",
	"tokens.new":     "New token",
	"tokens.id":      "ID",
	"tokens.name":    "name",
	"tokens.perms":   "Perms",
	"tokens.scope":   "Scope",
	"tokens.expires": "Expires",
	"tokens.status":  "Status",
	"tokens.all":     "all caches",
	"tokens.push":    "push",
	"tokens.pull":    "pull",
	"tokens.ttl":     "ttl",
	"tokens.create":  "Create",
	"tokens.revoke":  "Revoke",
	"tok.active":     "active",
	"tok.expired":    "expired",
	"tok.revoked":    "revoked",
	"tok.never":      "never",

	// cache detail
	"cd.stats":      "Stats",
	"cd.use":        "Use this cache",
	"cd.usehint":    "Add to nix.conf, a flake, or via the CLI.",
	"cd.push":       "Push",
	"cd.private":    "Private cache — a pull token is required.",
	"cd.settings":   "Settings",
	"cd.maxsize":    "max size",
	"cd.retention":  "retention",
	"cd.save":       "Save",
	"cd.rotate":     "Rotate key",
	"cd.rotatehint": "New keypair; redistribute the public key.",
	"cd.maint":      "Maintenance",
	"cd.gc":         "Run GC",
	"cd.gchint":     "Deletes unreferenced chunks.",
	"cd.delete":     "Delete cache",
	"cd.deletehint": "Removes the cache and its paths.",

	// visibility
	"vis.public":  "public",
	"vis.private": "private",

	// settings
	"set.title":       "Settings",
	"set.subtitle":    "Admin account security.",
	"set.password":    "Change password",
	"set.current":     "Current password",
	"set.new":         "New password",
	"set.update":      "Update",
	"set.2fa":         "Two-factor auth",
	"set.2fa.on":      "on",
	"set.2fa.enabled": "A code is required at sign-in.",
	"set.2fa.disable": "Disable 2FA",
	"set.2fa.hint":    "Add an authenticator app for sign-in codes.",
	"set.2fa.enable":  "Enable 2FA",
	"set.enroll":      "Enable two-factor",
	"set.enrollhint":  "Scan the QR, then enter a code to confirm.",
	"set.manual":      "Or enter this secret:",
	"set.confirm":     "Confirm",
	"set.code":        "6-digit code",

	// copy
	"copy":        "Copy",
	"copy.copied": "Copied",

	// generic actions
	"action.new":      "New",
	"action.cancel":   "Cancel",
	"footer.metrics2": "Metrics",
}
