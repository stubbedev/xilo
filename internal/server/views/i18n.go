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
	"caches.title":      "Caches",
	"caches.empty":      "No caches yet.",
	"caches.new":        "New cache",
	"caches.name":       "Cache name",
	"caches.private":    "Private",
	"caches.priority":   "Priority",
	"caches.create":     "Create",
	"caches.visibility": "Visibility",

	// tokens
	"tokens.title":   "Tokens",
	"tokens.empty":   "No tokens yet.",
	"tokens.new":     "New token",
	"tokens.id":      "ID",
	"tokens.name":    "Name",
	"tokens.perms":   "Permissions",
	"tokens.scope":   "Scope",
	"tokens.expires": "Expires",
	"tokens.status":  "Status",
	"tokens.all":     "all caches",
	"tokens.push":    "Push",
	"tokens.pull":    "Pull",
	"tokens.ttl":     "TTL",
	"tokens.create":  "Create",
	"tokens.revoke":  "Revoke",
	"tok.active":     "Active",
	"tok.expired":    "Expired",
	"tok.revoked":    "Revoked",
	"tok.never":      "never",

	// cache detail
	"cd.stats":      "Stats",
	"cd.use":        "Use this cache",
	"cd.usehint":    "Add to nix.conf, a flake, or via the CLI.",
	"cd.push":       "Push",
	"cd.private":    "Private cache — a pull token is required.",
	"cd.settings":   "Settings",
	"cd.maxsize":    "Max size",
	"cd.retention":  "Retention",
	"cd.save":       "Save",
	"cd.rotate":     "Rotate key",
	"cd.rotatehint": "Generates a new signing keypair. Clients must trust the new public key.",
	"cd.maint":      "Maintenance",
	"cd.gc":         "Run GC",
	"cd.gchint":     "Frees disk space by deleting chunks no path references.",
	"cd.delete":     "Delete cache",
	"cd.deletehint": "Permanently removes this cache and every path in it.",
	"cd.paths":      "Paths",
	"cd.search":     "Search store paths…",
	"cd.dosearch":   "Search",
	"cd.pathsempty": "No paths yet — push a build to see it here.",
	"cd.nomatch":    "No paths match.",
	"cd.showing":    "Showing",
	"paths.path":    "Store path",
	"paths.size":    "Size",
	"paths.pulled":  "Last pulled",

	// visibility
	"vis.public":  "Public — anyone can pull",
	"vis.private": "Private — pull token required",

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

	// units & field hints
	"unit.days":          "days",
	"unit.hours":         "hours",
	"caches.prioritytip": "Lower value wins when multiple caches have a path",
	"tokens.permanent":   "Permanent (never expires)",
	"tokens.actions":     "Actions",
	"tokens.edit":        "Edit",
	"tokens.pushtip":     "Push — can upload to the cache",
	"tokens.pulltip":     "Pull — can download from the cache",
	"unit.months":        "months",
	"unit.years":         "years",

	// search
	"caches.search":  "Search caches…",
	"tokens.search":  "Search tokens…",
	"search.nomatch": "Nothing matches your search.",

	// pagination
	"pager.prev": "Prev",
	"pager.next": "Next",

	// generic actions
	"action.new":    "New",
	"action.cancel": "Cancel",

	// not found
	"nf.title": "Page not found",
	"nf.sub":   "That page doesn't exist or was moved.",
	"nf.back":  "Back to caches",
}
