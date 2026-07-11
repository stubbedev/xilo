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
	"footer.tagline": "· self-hosted Nix cache",
	"footer.status":  "Status",

	// status dashboard
	"status.title":       "Status",
	"status.subtitle":    "Live health, traffic and storage.",
	"status.rate":        "Refresh",
	"status.windowsel":   "Window",
	"status.from":        "From",
	"status.to":          "To",
	"status.custom":      "Custom",
	"status.apply":       "Apply",
	"status.updated":     "Updated",
	"status.peak":        "peak",
	"status.health":      "Health",
	"status.ok":          "Healthy",
	"status.err":         "Degraded",
	"status.uptime":      "Uptime",
	"status.hits":        "Hit ratio",
	"status.stored":      "On disk",
	"status.authfail":    "Auth failures",
	"status.nars":        "NARs served",
	"status.reqs":        "Requests",
	"status.req":         "Requests / second",
	"status.lat":         "Avg latency",
	"status.thru":        "NAR throughput",
	"status.storedchart": "Storage",

	// login
	"login.title":      "Sign in",
	"login.subtitle":   "Manage caches and tokens.",
	"login.username":   "Username",
	"login.password":   "Password",
	"login.code":       "2FA code",
	"login.submit":     "Sign in",
	"login.nopassword": "No admin password set. Configure admin.password or XILO_ADMIN_PASSWORD.",
	"login.passkey":    "Sign in with a passkey",
	"login.codetitle":  "Two-factor code",
	"login.codesub":    "Enter the 6-digit code from your authenticator app.",
	"login.verify":     "Verify",
	"login.back":       "Back to sign in",

	// dashboard — overview
	"dash.title":    "Overview",
	"dash.subtitle": "Nix binary caches on this instance.",
	"kpi.caches":    "caches",
	"kpi.paths":     "paths",
	"kpi.disk":      "disk used",
	"kpi.dedup":     "dedup",
	"kpi.logical":   "logical",
	"kpi.physical":  "on disk",
	"cache.paths":   "paths",
	"cache.nocap":   "no cap",

	// caches
	"caches.title":      "Caches",
	"caches.empty":      "No caches yet.",
	"caches.new":        "New cache",
	"caches.name":       "Cache name",
	"caches.priority":   "Priority",
	"caches.create":     "Create",
	"caches.visibility": "Visibility",

	// tokens
	"tokens.title":   "Tokens",
	"tokens.empty":   "No tokens yet.",
	"tokens.new":     "New token",
	"tokens.name":    "Name",
	"tokens.perms":   "Permissions",
	"tokens.scope":   "Scope",
	"tokens.expires": "Expires",
	"tokens.status":  "Status",
	"tokens.all":     "all caches",
	"tokens.push":    "Push",
	"tokens.pull":    "Pull",
	"tokens.admin":   "Admin",
	"tokens.create":  "Create",
	"tokens.revoke":  "Revoke",
	"tok.active":     "Active",
	"tok.expired":    "Expired",
	"tok.revoked":    "Revoked",
	"tok.never":      "never",

	// cache detail
	"cd.stats":      "Stats",
	"cd.use":        "Use this cache",
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
	"cd.pathsempty": "No paths yet — push a build to see it here.",
	"cd.nomatch":    "No paths match.",
	"paths.path":    "Store path",
	"paths.size":    "Size",
	"paths.pulled":  "Last pulled",

	// visibility
	"vis.public":  "Public",
	"vis.private": "Private",

	// settings
	"set.title":       "Settings",
	"set.subtitle":    "Admin account security.",
	"set.password":    "Change password",
	"set.current":     "Current password",
	"set.new":         "New password",
	"set.new2":        "Repeat new password",
	"set.pw.short":    "Too short — at least 8 characters.",
	"set.pw.weak":     "Weak — add length or mix in cases, digits, symbols.",
	"set.pw.strong":   "Strong password.",
	"set.pw.mismatch": "Passwords do not match.",
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
	"unit.hours":       "hours",
	"unit.days":        "days",
	"unit.months":      "months",
	"unit.years":       "years",
	"tokens.permanent": "Permanent (never expires)",
	"tokens.actions":   "Actions",
	"tokens.edit":      "Edit",

	// search
	"caches.search":  "Search caches…",
	"tokens.search":  "Search tokens…",
	"search.nomatch": "Nothing matches your search.",

	// pagination
	"pager.prev": "Prev",
	"pager.next": "Next",

	// passkeys
	"set.passkeys":        "Passkeys",
	"set.passkeys.hint":   "Sign in with a hardware key or platform authenticator instead of a password.",
	"set.passkeys.add":    "Add passkey",
	"set.passkeys.remove": "Remove",
	"confirm.passkey":     "Remove this passkey? You can no longer sign in with it.",

	// confirmations
	"confirm.title":  "Are you sure?",
	"confirm.revoke": "Revoke this token? Clients using it lose access immediately.",
	"confirm.rotate": "Rotate the signing key? The current trusted-public-key stops verifying.",
	"confirm.gc":     "Run garbage collection now?",
	"confirm.delete": "Delete this cache and all its paths? This cannot be undone.",
	"confirm.2fa":    "Disable two-factor authentication?",
	"confirm.user":   "Delete this user? Their passkeys and sessions go with them.",

	// user management
	"set.users":     "Users",
	"users.new":     "New user",
	"users.create":  "Create",
	"users.name":    "Username",
	"users.newpw":   "New password",
	"users.role":    "Role",
	"users.admin":   "Admin",
	"users.member":  "Member",
	"users.reset":   "Reset password",
	"users.promote": "Make admin",
	"users.demote":  "Make member",
	"users.delete":  "Delete user",
	"users.hint":    "Accounts that can sign in to this dashboard.",

	// generic actions
	"action.new":     "New",
	"action.cancel":  "Cancel",
	"action.confirm": "Confirm",

	// not found
	"nf.title": "Page not found",
	"nf.sub":   "That page doesn't exist or was moved.",
	"nf.back":  "Back to caches",
}
