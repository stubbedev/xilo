package views

import (
	"context"
	"fmt"
	"strings"
)

// Locale is one shipped catalog. Name is written in the language itself —
// a picker that says "German" to someone who only reads German is no help.
type Locale struct {
	ID, Name string
}

// Locales is the language picker, English first. Adding a language means a
// catalog map, an entry here, and nothing else: T falls back per key, so a
// half-finished catalog renders the rest in English instead of breaking.
var Locales = []Locale{
	{"en", "English"},
	{"de", "Deutsch"},
	{"fr", "Français"},
	{"es", "Español"},
	{"zh", "中文"},
	{"ja", "日本語"},
}

var catalogs = map[string]map[string]string{
	"en": en, "de": de, "fr": fr, "es": es, "zh": zh, "ja": ja,
}

// ValidLocale reports whether id names a shipped catalog.
func ValidLocale(id string) bool {
	_, ok := catalogs[id]
	return ok
}

// MatchLocale maps an Accept-Language header (or any "de-AT,de;q=0.9" list)
// to a shipped catalog id, "en" when nothing matches. Quality values are not
// weighed: the header is already in preference order and a tie between two
// languages we both ship is not worth the parser.
func MatchLocale(header string) string {
	for tag := range strings.SplitSeq(header, ",") {
		tag, _, _ = strings.Cut(tag, ";")
		tag = strings.TrimSpace(strings.ToLower(tag))
		if base, _, _ := strings.Cut(tag, "-"); ValidLocale(base) {
			return base
		}
	}
	return "en"
}

type langKey struct{}

// WithLocale puts the viewer's language on the context. Every T reads it from
// there, so a handler sets it once per request and nothing else threads it.
func WithLocale(ctx context.Context, id string) context.Context {
	if !ValidLocale(id) {
		return ctx
	}
	return context.WithValue(ctx, langKey{}, id)
}

// LocaleFrom reports the context's language, "en" when none was set.
func LocaleFrom(ctx context.Context) string {
	if id, ok := ctx.Value(langKey{}).(string); ok {
		return id
	}
	return "en"
}

// T returns the UI string for a message id in the context's language. All
// user-facing labels, placeholders, helper text and flashes flow through here
// so they are DRY and translatable.
//
// Lookup falls back twice: a key the active catalog is missing comes from
// English, and a key nothing defines returns the id itself (visible in the
// page, and caught by TestCatalogCoversEveryLookup before it ships).
func T(ctx context.Context, id string) string {
	if c, ok := catalogs[LocaleFrom(ctx)]; ok {
		if s, ok := c[id]; ok {
			return s
		}
	}
	if s, ok := en[id]; ok {
		return s
	}
	return id
}

// en is the default (English) catalog. Keep values concise.
var en = map[string]string{
	// one-shot flash messages (server handlers)
	"flash.gcdone":          "Removed %d chunks, freed %s.",
	"flash.rotated":         "Signing key rotated. Update trusted-public-keys everywhere:",
	"flash.badname":         "Names cannot contain '/'.",
	"flash.notadmin":        "You do not administer this account.",
	"flash.cachecreated":    "Cache %s created.",
	"flash.cachedeleted":    "Cache %s deleted.",
	"flash.tokencreated":    "Token %q created. Copy it now — it is not shown again:",
	"flash.tokenupdated":    "Token updated.",
	"flash.pickaccount":     "Pick an account for the cache.",
	"flash.tokenrevoked":    "Token revoked.",
	"flash.emailfailed":     "Could not save your email address.",
	"flash.appearancesaved": "Appearance saved.",
	"flash.emailreq":        "This instance requires a valid email address.",
	"flash.pwwrong":         "Current password is incorrect.",
	"flash.pwshort":         "Password must be at least 8 characters.",
	"flash.pwlong":          "Password must be at most 72 characters.",
	"flash.pwmismatch":      "Passwords do not match.",
	"flash.pwchanged":       "Password changed.",
	"flash.pwreset":         "Password reset for %s.",
	"flash.totpon":          "Two-factor enabled.",
	"flash.totpoff":         "Two-factor disabled.",
	"flash.pkremoved":       "Passkey removed.",
	"flash.nouser":          "No such user.",
	"flash.memberrole":      "%s is now a %s of %s.",
	"flash.memberremoved":   "Member removed.",
	"flash.userreq":         "Username is required.",
	"flash.userfailed":      "Could not create the user.",
	"flash.usercreated":     "User %q created.",
	"flash.userapproved":    "%s approved.",
	"flash.userdeleted":     "User %s deleted.",
	"flash.delowner":        "A superadmin cannot be deleted.",
	"flash.ownsorgs":        "They own organizations. Delete those first.",
	"flash.delself":         "You cannot delete your own account.",
	"flash.badorgname":      "Invalid organization name.",
	"flash.nametaken":       "That name is taken.",
	"flash.orgready":        "Account %q ready.",
	"flash.orgcreated":      "Organization %q created.",
	"flash.orgdeleted":      "Organization %s deleted.",
	"flash.ownerdelete":     "Only the owner can delete an organization.",
	"flash.deletefailed":    "Could not delete that.",
	"flash.settingssaved":   "Instance settings saved.",
	"flash.plannamereq":     "Plan name is required.",
	"flash.planfailed":      "Could not create the plan.",
	"flash.savefailed":      "Could not save that.",
	// store refusals, mapped from the sentinels in internal/store (storeMsg)
	"flash.badrole":      "The roles you can grant are admin and user.",
	"flash.ownerrole":    "The owner's role cannot be changed.",
	"flash.ownworkspace": "A workspace belongs to its user and is removed with them.",
	"flash.hasowner":     "That account already has an owner.",
	"flash.ownerlocked":  "The owner cannot be removed.",
	"flash.planinuse":    "That plan is in use by accounts.",
	"flash.slugreserved": "That name is reserved.",
	"flash.plancreated":  "Plan %q created.",
	"flash.planupdated":  "Plan %q updated.",
	"flash.plandeleted":  "Plan deleted.",
	"flash.regpending":   "Registered. An administrator must approve your account first.",
	"flash.badlogin":     "Invalid username or password.",
	"flash.awaiting":     "Your account is awaiting approval.",
	"flash.loginexpired": "That sign-in expired. Enter your password again.",
	"flash.bad2fa":       "Invalid two-factor code. Enter your password again.",
	"flash.badcode":      "That code did not match. Try again.",
	"flash.emailsaved":   "Email saved.",
	"flash.emailcleared": "Email cleared.",
	"reg.err.username":   "Use lowercase letters, digits, - and _.",
	"reg.err.plan":       "Pick one of the offered plans.",
	"reg.err.noorgs":     "The selected plan does not include organizations.",
	"reg.err.orgname":    "Use lowercase letters, digits, - and _.",
	"reg.err.failed":     "Could not complete registration.",
	// roles: superadmin is the instance-wide one, owner/admin/user are account roles
	"role.superadmin": "superadmin",
	"role.owner":      "owner",
	"role.admin":      "admin",
	"role.user":       "user",
	"role.pending":    "pending approval",

	// nav / chrome
	"nav.caches":        "Caches",
	"nav.account":       "Account",
	"nav.settings":      "Settings",
	"nav.status":        "Status",
	"nav.instance":      "Instance",
	"console.title":     "Console",
	"console.subtitle":  "Everything this instance holds.",
	"console.orgs":      "Workspaces",
	"console.users":     "Users",
	"console.allcaches": "All caches",
	"nav.audit":         "Activities",
	"nav.context":       "Account",
	"nav.logout":        "Log out",
	"nav.addaccount":    "Add account",
	"nav.theme":         "Theme",
	"footer.tagline":    "self-hosted Nix cache",

	// status dashboard
	"status.title":       "Status",
	"status.past_due":    "read-only",
	"status.suspended":   "suspended",
	"status.readonly":    "Make read-only",
	"status.suspend":     "Suspend",
	"status.restore":     "Restore",
	"flash.statusset":    "%s updated.",
	"flash.badstatus":    "That is not a status this instance knows.",
	"status.subtitle":    "Live health, traffic and storage.",
	"status.settings":    "Settings",
	"status.window":      "Window",
	"status.range":       "Custom",
	"status.from":        "Start date",
	"status.to":          "End date",
	"status.apply":       "Apply",
	"status.autorefresh": "Auto-refresh",
	"status.updated":     "Updated",
	"status.peak":        "peak",
	"status.collecting":  "Collecting…",
	"status.health":      "Health",
	"status.ok":          "Healthy",
	"status.err":         "Degraded",
	"status.uptime":      "Uptime",
	"status.hits":        "Hit rate",
	"status.stored":      "Stored",
	"status.authfail":    "Auth failures",
	"status.pushed":      "Paths pushed",
	"status.adopted":     "Paths adopted",
	"status.chunks":      "Chunks received",
	"status.deduped":     "Chunks deduped",
	"status.nars":        "NARs served",
	"status.reqs":        "Requests",
	"status.req":         "Requests /s",
	"status.lat":         "Latency (ms)",
	"status.thru":        "NAR throughput (MiB/s)",
	"status.storedchart": "Stored (MiB)",

	// activities
	"audit.title":    "Activities",
	"audit.subtitle": "Every admin and API change, newest first.",
	"audit.search":   "Actor, path or IP",
	"audit.time":     "Time",
	"audit.actor":    "Actor",
	"audit.action":   "Action",
	"audit.status":   "Status",
	"audit.system":   "system",
	"audit.token":    "API token",
	"audit.ms":       "ms",
	"audit.total":    "Actions",
	"audit.failed":   "Failed",
	"audit.actors":   "Actors",
	"audit.avgms":    "Avg. duration",
	"audit.method":   "Method",
	"audit.all":      "All",

	// login
	"login.title":      "Sign in",
	"login.subtitle":   "Xilo administration.",
	"login.username":   "Username",
	"login.userph":     "Username",
	"login.password":   "Password",
	"login.submit":     "Sign in",
	"login.nopassword": "Any username creates the first admin account.",
	"login.passkey":    "Use a passkey",
	"login.codetitle":  "Two-factor code",
	"login.twofactor":  "Two-factor",
	"login.codesub":    "From your authenticator app.",
	"login.verify":     "Verify",
	"login.back":       "Back",
	"login.needacct":   "Need an account?",
	"login.register":   "Register",

	// registration
	"reg.title":    "Create your account",
	"reg.subtitle": "Sign up for a Xilo account.",
	"reg.username": "Username",
	"reg.userph":   "Username",
	"reg.email":    "Email",
	"reg.password": "Password",
	"reg.plan":     "Plan",
	"reg.org":      "Organization name",
	"reg.orghint":  "You will administer it.",
	"reg.submit":   "Create account",

	// instance settings + plans
	"set.instance":          "Instance settings",
	"inst.subtitle":         "Policy, plans and users.",
	"inst.subtitle.single":  "Users and maintenance.",
	"inst.policy":           "Registration policy",
	"rules.title":           "Instance rules",
	"rules.cap":             "Storage ceiling",
	"rules.caphint":         "The most this instance will keep across every cache. Empty means no ceiling.",
	"rules.retention":       "Default retention",
	"rules.retentionhint":   "How long a new cache keeps a path when its owner sets nothing.",
	"rules.tokenttl":        "Longest token life",
	"rules.tokenttlhint":    "Caps every token, including ones asked to never expire. Empty allows permanent tokens.",
	"rules.defaultplan":     "Signup plan",
	"rules.defaultplanhint": "The plan a sign-up lands on when it names none.",
	"rules.noplan":          "They choose",
	"inst.regs":             "Allow self-registration",
	"inst.approve":          "Require admin approval",
	"inst.maint":            "Maintenance",
	"inst.gcdesc":           "Remove unreferenced chunks and reclaim disk.",
	"inst.gctitle":          "Run garbage collection?",
	"inst.gcmsg":            "Deleted permanently. Paths still in use are untouched.",
	"inst.gcrun":            "Run GC",
	"set.plans":             "Plans",
	"plan.new":              "New plan",
	"plan.edit":             "Edit plan",
	"plan.name":             "Name",
	"plan.price":            "Price",
	"plan.pricehint":        "Per account, in whole currency units. Empty is free.",
	"plan.free":             "Free",
	"plan.interval":         "Billed",
	"plan.monthly":          "Monthly",
	"plan.yearly":           "Yearly",
	"plan.permonth":         "%s / month",
	"plan.peryear":          "%s / year",
	"plan.external":         "Provider price id",
	"plan.externalhint":     "How this plan is matched to a price at the payment provider. Empty until billing is wired up.",
	"plan.externalph":       "price_…",
	"plan.maxcaches":        "Max caches",
	"plan.maxmembers":       "Max members",
	"plan.maxstorage":       "Max storage",
	"plan.maxretention":     "Max retention",
	"plan.orgs":             "orgs",
	"plan.orgsallowed":      "Can create organizations",
	"plan.public":           "Offered at self-registration",
	"plan.deletetitle":      "Delete plan?",
	"plan.deletemsg":        "Accounts on “%s” keep their data but lose this plan's limits.",
	"plan.hint":             "Limits per account.",
	"plan.unlimited":        "no limits",
	"plan.caches":           "caches",
	"plan.members":          "members",
	"plan.storage":          "storage",
	"plan.retention":        "retention",
	"org.subtitle":          "Share caches with a team.",
	"acct.subtitle":         "Signed in as %s.",
	"acct.email":            "Email",
	"acct.signin":           "Sign-in",
	"acct.emailaddr":        "Email address",
	"acct.emailhint":        "For notifications and sign-in.",
	"acct.appearance":       "Appearance",
	"acct.appearancehint":   "Light/dark follows the top bar.",
	"acct.palette":          "Palette",
	"acct.language":         "Language",
	"acct.langauto":         "Auto",
	"acct.usestored":        "%s stored",
	"acct.useof":            "%s stored of %s",
	"acct.useegress":        "%s served this month",
	"acct.useplan":          "plan %s",
	"users.approve":         "Approve",

	// organizations
	"org.title":         "Workspaces",
	"org.new":           "New workspace",
	"org.newhint":       "A namespace for caches, shared with anyone you add.",
	"org.name":          "Name",
	"org.deletetitle":   "Delete organization?",
	"org.deletemsg":     "“%s” and its caches will be removed.",
	"org.members":       "Members",
	"org.membercount":   "%d member",
	"org.memberscount":  "%d members",
	"org.caches":        "Caches",
	"org.addmember":     "Add member",
	"org.member":        "User",
	"org.addmemberhint": "Gives someone access to this organization's caches.",
	"org.nocandidates":  "Everyone is already a member.",
	"org.pickuser":      "Select",
	"org.removetitle":   "Remove member?",
	"org.removemsg":     "%s will lose access to %s.",

	// dashboard. overview
	"dash.title":    "Overview",
	"dash.subtitle": "Storage and access across your caches.",
	"dash.scopedto": "Scoped to %s.",
	"kpi.caches":    "Caches",
	"kpi.paths":     "Store paths",
	"kpi.disk":      "Disk used",
	"kpi.dedup":     "Dedup ratio",
	"kpi.logical":   "Logical",
	"kpi.physical":  "On disk",
	"kpi.saved":     "Saved",
	"cache.paths":   "paths",
	"cache.nocap":   "no cap",

	// caches
	// first-run checklist
	"onb.title":     "Get your first cache working",
	"onb.subtitle":  "Three steps, about a minute.",
	"onb.step1":     "Create a cache",
	"onb.step1hint": "In your account, or an organization you administer.",
	"onb.step2":     "Create a token for it",
	"onb.step2hint": "From the cache page, so the scope is right.",
	"onb.step3":     "Point Nix at it",
	"onb.step3hint": "The cache page has them, filled in.",

	"caches.title":        "Caches",
	"caches.new":          "New cache",
	"caches.newhint":      "In an account you administer.",
	"caches.name":         "Name",
	"caches.priority":     "Priority",
	"caches.priorityhint": "Lower wins (1–100).",
	"caches.privatehint":  "Require a token to pull.",
	"caches.private":      "Private",

	// tokens
	"tokens.title":     "Tokens",
	"tokens.new":       "New token",
	"tokens.newtitle":  "New token",
	"tokens.newhint":   "For pushing or pulling.",
	"tokens.edittitle": "Edit token",
	"tokens.edithint":  "Rename, rescope or change permissions.",
	"tokens.name":      "Name",
	"tokens.perms":     "Permissions",
	"tokens.scope":     "Scope",
	"tokens.pickcache": "Select",
	"tokens.needcache": "Create a cache first",
	"tokens.expires":   "Expires",
	"tokens.expiry":    "Expiry",
	"tokens.status":    "Status",
	"tokens.root":      "instance root",
	"tokens.roothint":  "Manages every cache, token and account.",
	"tokens.scopehint": "One cache per token.",
	"tokens.push":      "Push",
	"tokens.pull":      "Pull",
	"tokens.manage":    "Manage",
	"tokens.permanent": "Never",
	"ttl.keep":         "Keep current",
	"ttl.7d":           "7 days",
	"ttl.30d":          "30 days",
	"ttl.90d":          "90 days",
	"ttl.1y":           "1 year",
	"tokens.revoke":    "Revoke",
	"tok.revoketitle":  "Revoke token?",
	"tok.revokemsg":    "“%s” will stop working immediately. This cannot be undone.",
	"tok.active":       "active",
	"tok.expired":      "expired",
	"tok.revoked":      "revoked",
	"tok.never":        "never",
	"perm.push":        "push",
	"perm.pull":        "pull",
	"perm.manage":      "manage",
	"perm.admin":       "admin",

	// cache detail
	"cd.tab.nixconf": "nix.conf",
	"cd.tab.flake":   "flake",
	"cd.tab.cli":     "CLI",
	"cd.use":         "Use this cache",
	"cd.usehint":     "Add it as a substituter.",
	"cd.push":        "Push to this cache",
	"cd.pushhint":    "With a token that has push access.",
	"cd.private":     "Pulls need a token with pull access.",
	"cd.netrc":       "Add to ~/.netrc",
	"cd.netrchint":   "Basic auth. `xilo use` writes it for you.",
	"cd.pushtoken":   "Push token",
	"cd.pulltoken":   "Pull token",
	"cd.tokenhint":   "Scoped to this cache. Shown once.",
	"cd.settings":    "Settings",
	"cd.maxsize":     "Max size",
	"cd.retention":   "Retention",
	"cd.rotate":      "Rotate",
	"cd.rotatetitle": "Rotate signing key?",
	"cd.rotatehint":  "Clients must update their trusted public key.",
	"cd.maint":       "Maintenance",
	"cd.deletetitle": "Delete cache?",
	"cd.deletemsg":   "“%s” and every path in it will be removed. This cannot be undone.",
	"cd.deletehint":  "Removes every path in it.",
	"cd.paths":       "Store paths",
	"cd.search":      "Search",
	"paths.path":     "Path",
	"paths.size":     "Size",
	"paths.pulled":   "Last pulled",
	"paths.open":     "Inspect path",

	// store path detail
	"path.narsize":     "NAR size",
	"path.ondisk":      "On disk",
	"path.chunks":      "Chunks",
	"path.refs":        "References",
	"path.narinfo":     "Narinfo",
	"path.narinfohint": "What Nix gets for this path.",
	"path.narhash":     "NAR hash",
	"path.deriver":     "Deriver",
	"path.noderiver":   "not recorded",
	"path.url":         "Narinfo URL",
	"path.refshint":    "Linked ones are in this cache.",
	"path.norefs":      "Depends on nothing.",
	"path.chunkshint":  "NAR order. Shared chunks stored once.",
	"path.chunk":       "Chunk",
	"path.compressed":  "Compressed",
	"path.broken":      "chunks missing",
	"path.brokenhint":  "Pulls will fail. Run xilo fsck.",

	// visibility
	"vis.public":  "public",
	"vis.private": "private",

	// settings
	"set.password":    "Your sign-in password.",
	"set.current":     "Current password",
	"set.new":         "New password",
	"set.new2":        "Confirm",
	"set.pw.short":    "At least 8 characters.",
	"set.pw.weak":     "Weak — add length or variety.",
	"set.pw.strong":   "Strong password.",
	"set.2fa":         "Two-factor authentication",
	"set.2fa.on":      "enabled",
	"set.2fa.off":     "disabled",
	"set.2fa.disable": "Disable",
	"set.2fa.offmsg":  "Password only from then on.",
	"set.2fa.enable":  "Enable",
	"set.enroll":      "Set up two-factor",
	"set.enrollhint":  "Scan it, then enter a code to confirm.",
	"set.manual":      "Or enter it manually:",
	"set.qralt":       "TOTP QR",

	// client-side toasts (toastKinds, helpers.go)
	"copy.done":   "Copied to clipboard.",
	"toast.error": "Something went wrong.",

	// hard errors written straight to the response (uiFail/uiError, admin.go).
	// The underlying Go error is logged, never shown: it names internal paths,
	// SQL and library internals a user can neither read nor act on.
	"err.internal":     "Something went wrong. Try again.",
	"err.crossorigin":  "Cross-origin request rejected.",
	"err.superadmin":   "Only the instance superadmin can do that.",
	"err.session":      "Could not start your session.",
	"err.unauthorized": "Sign in to continue.",
	"err.throttled":    "Too many attempts. Wait a moment and try again.",
	"err.noorgs":       "Your plan does not include organizations.",
	"err.storage":      "Unknown storage backend.",
	"err.tokenscope":   "The scope must be a cache in %s.",
	"err.tokenfailed":  "Could not create the token.",
	"err.pkexpired":    "Registration expired. Try again.",
	"err.pkverify":     "Your passkey must verify you with a PIN or biometric.",
	"err.pknone":       "No passkeys registered.",
	"err.pkloginexp":   "Sign-in expired. Try again.",
	"err.pkunknown":    "Unknown passkey.",
	"err.pkowner":      "That passkey has no owner.",
	"err.pkregister":   "Could not register that passkey.",
	"err.pksignin":     "Could not sign in with that passkey.",

	// plan quotas. These are returned as Go errors, so they surface both in
	// the admin UI and in the CLI's push output.
	"quota.caches":  "Plan %q allows at most %d caches.",
	"quota.members": "Plan %q allows at most %d members.",
	"quota.storage": "Storage quota reached: %s of %s on plan %q. Pushes are paused.",

	// transactional email (subject, then body)
	"mail.pendingsub":   "Registration received",
	"mail.pendingbody":  "Your account %s on %s is waiting for an administrator to approve it. You will get another email when it is.",
	"mail.adminsub":     "New registration awaiting approval",
	"mail.adminbody":    "%s registered on %s and is waiting for approval in Settings.",
	"mail.welcomesub":   "Welcome to %s",
	"mail.welcomebody":  "Your account %s is active. Sign in at %s/admin.",
	"mail.approvedsub":  "Your account was approved",
	"mail.approvedbody": "Your account %s on %s is approved — sign in at %s/admin.",
	"mail.orgsub":       "You were added to %s",
	"mail.orgbody":      "You are now a %s of the organization %s on %s.",

	// units & field hints
	"unit.hours":      "Hours",
	"unit.days":       "Days",
	"unit.months":     "Months",
	"unit.years":      "Years",
	"unit.mib":        "MiB",
	"unit.gib":        "GiB",
	"unit.tib":        "TiB",
	"unit.duration":   "Duration",
	"unit.unlimited":  "unlimited",
	"storage.default": "Default",
	"tokens.actions":  "Actions",

	// placeholders (example values)
	"ph.cachename":     "nixpkgs",
	"ph.tokenname":     "ci-push",
	"ph.email":         "you@example.com",
	"ph.newuser":       "username",
	"ph.newemail":      "email@example.com",
	"ph.planname":      "free",
	"ph.orgname":       "my-org",
	"ph.zerounlimited": "unlimited",

	// relative time
	"time.justnow": "just now",
	"time.mago":    "%dm ago",
	"time.hago":    "%dh ago",
	"time.dago":    "%dd ago",

	// search
	"caches.search": "Search",
	"tokens.search": "Search",

	// pagination
	"pager.prev": "Prev",
	"pager.next": "Next",

	// passkeys
	"set.passkeys":   "Passkeys",
	"pk.removetitle": "Remove passkey?",
	"pk.removemsg":   "“%s” will no longer be able to sign in.",

	// confirmations
	"confirm.rotate": "The old key stops verifying immediately. Update trusted-public-keys everywhere.",
	"confirm.2fa":    "Disable two-factor?",

	// accounts / organizations
	"caches.account":     "Account",
	"caches.pickaccount": "Select",
	"caches.needaccount": "None you administer",
	"caches.storage":     "Storage backend",

	// user management
	"set.users":         "Users",
	"users.new":         "New user",
	"users.name":        "Username",
	"users.newpw":       "Password",
	"users.email":       "Email",
	"users.role":        "Role",
	"users.reset":       "Reset password",
	"users.resetfor":    "Reset password for %s",
	"users.resethint":   "Immediate. No email sent.",
	"users.promote":     "Promote",
	"users.demote":      "Demote",
	"users.deletetitle": "Delete user?",
	"users.deletemsg":   "“%s” and their sessions will be removed.",
	"users.hint":        "A sign-in on this instance.",

	// generic actions
	"action.new":    "New",
	"action.create": "Create",
	"action.add":    "Add",
	"action.reset":  "Reset",
	"empty.none":    "None yet.",
	"empty.nomatch": "No matches.",
	"action.cancel": "Cancel",
	"action.clear":  "Clear",
	"action.save":   "Save",
	"action.delete": "Delete",
	"action.edit":   "Edit",
	"action.remove": "Remove",

	// not found
	"nf.doc":   "Not found",
	"nf.title": "Page not found",
	"nf.sub":   "It does not exist, or you cannot see it.",
	"nf.back":  "Back to caches",
}

// Tf is T with the arguments filled in — for the catalog entries carrying %s
// or %d. Gluing a translated fragment onto a name ("“" + name + "” " + T(…))
// only reads correctly in English: the moment a language puts its words in
// another order the sentence falls apart. Every message with a value in it is
// therefore one format string, and the translator moves the verb.
func Tf(ctx context.Context, id string, args ...any) string {
	return fmt.Sprintf(T(ctx, id), args...)
}
