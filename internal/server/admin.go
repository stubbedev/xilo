package server

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"maps"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/crypto/bcrypt"

	"github.com/stubbedev/xilo/internal/mail"
	"github.com/stubbedev/xilo/internal/narinfo"
	"github.com/stubbedev/xilo/internal/server/views"
	"github.com/stubbedev/xilo/internal/store"
)

const sessionCookie = "xilo_session"

// sessionTTL bounds how long an admin session cookie stays valid.
const sessionTTL = 12 * time.Hour

// sessions persists login sessions in the store (hashed, so a DB read never
// yields a usable cookie value) — logins survive server restarts. The 2FA
// pending tickets stay in memory: they live 3 minutes and a restart mid-login
// just means retyping the password.
type sessions struct {
	mu      sync.Mutex
	db      *store.DB
	pending map[string]pendingLogin // password accepted, awaiting 2FA code
}

// pendingLogin remembers who passed the password step while their 2FA code is
// outstanding.
type pendingLogin struct {
	exp    time.Time
	userID int64
}

func newSessions(db *store.DB) *sessions {
	return &sessions{db: db, pending: map[string]pendingLogin{}}
}

// hashSession derives the storage key for a session id.
func hashSession(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

// pendingTTL bounds how long the 2FA step may take after the password step.
const pendingTTL = 3 * time.Minute

// createPending issues a one-shot pre-auth ticket for the 2FA step.
func (s *sessions) createPending(userID int64) (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	id := base64.RawURLEncoding.EncodeToString(b)
	s.mu.Lock()
	s.pending[id] = pendingLogin{exp: time.Now().Add(pendingTTL), userID: userID}
	s.mu.Unlock()
	return id, nil
}

// pendingUser returns the user behind a live pre-auth ticket, or ok=false.
func (s *sessions) pendingUser(id string) (int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pending[id]
	if !ok || time.Now().After(p.exp) {
		delete(s.pending, id)
		return 0, false
	}
	return p.userID, true
}

// consumePending burns a ticket after a successful code check.
func (s *sessions) consumePending(id string) {
	s.mu.Lock()
	delete(s.pending, id)
	s.mu.Unlock()
}

func (s *sessions) create(userID int64) (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err // never emit a low-entropy session id
	}
	id := base64.RawURLEncoding.EncodeToString(b)
	if err := s.db.CreateSession(hashSession(id), userID, time.Now().Add(sessionTTL)); err != nil {
		return "", err
	}
	return id, nil
}

func (s *sessions) user(id string) (int64, bool) {
	return s.db.SessionUser(hashSession(id))
}

func (s *sessions) drop(id string) {
	_ = s.db.DropSession(hashSession(id))
}

// currentUser resolves the session cookie to its account, nil when signed out.
func (s *Server) currentUser(r *http.Request) *store.User {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil
	}
	uid, ok := s.sess.user(c.Value)
	if !ok {
		return nil
	}
	u, err := s.db.GetUser(uid)
	if err != nil {
		return nil
	}
	return u
}

func (s *Server) loggedIn(r *http.Request) bool { return s.currentUser(r) != nil }

const ctxCookie = "xilo_ctx"

// activeContext is the one account the admin is looking at: the slug from the
// context cookie when it still names an account this user may act in, and
// otherwise their default. There is no "everything" context — an overview of
// several accounts at once answers nobody's question about any of them, and it
// made every figure on the page mean something different depending on a
// setting two clicks away. The cookie may still be empty or stale (cleared on
// a bad pick, or written before an account was deleted); that resolves here,
// so callers can rely on getting an account.
func (s *Server) activeContext(r *http.Request, u *store.User) string {
	if u == nil {
		return ""
	}
	if c, err := r.Cookie(ctxCookie); err == nil && c.Value != "" {
		if acc, err := s.db.GetAccount(c.Value); err == nil && s.db.MemberRole(acc.ID, u.ID) != "" {
			return acc.Slug
		}
	}
	return s.defaultContext(u)
}

// defaultContext is where a user starts before they have picked anything:
// their own account, unless everything they can see lives somewhere else.
// Landing an instance admin in an empty personal account while the instance's
// caches sit in organizations reads as data gone missing, not as a scope.
//
// Only reached while the context cookie is absent or stale, so the extra
// lookup costs nothing once someone has switched accounts once.
func (s *Server) defaultContext(u *store.User) string {
	accs, err := s.db.UserAccounts(u.ID)
	if err != nil || len(accs) == 0 {
		return ""
	}
	personal := ""
	for _, a := range accs {
		if a.Slug == u.Name {
			personal = a.Slug
		}
	}
	// Start somewhere with something in it, but only among the accounts they
	// are actually in: an instance admin is not a member of every account just
	// because they can administer it. Their own account wins when it has
	// caches; otherwise the first that does.
	first := ""
	for _, a := range accs {
		cs, err := s.db.ListAccountCaches(a.ID)
		if err != nil || len(cs) == 0 {
			continue
		}
		if a.Slug == personal {
			return personal
		}
		if first == "" {
			first = a.Slug
		}
	}
	if first != "" {
		return first
	}
	if personal != "" {
		return personal
	}
	return accs[0].Slug
}

// nav builds the header state for a signed-in user (zero Nav when signed out).
func (s *Server) nav(r *http.Request, u *store.User) views.Nav {
	if u == nil {
		return views.Nav{}
	}
	n := views.Nav{LoggedIn: true, UserName: u.Name, IsAdmin: u.Superadmin(), Active: s.activeContext(r, u), Theme: u.Theme}
	accs, err := s.db.UserAccounts(u.ID)
	if err == nil {
		n.Contexts = accs
	}
	n.Orgs = s.cfg.SelfService && (s.userCanCreateOrg(u) || slices.ContainsFunc(n.Contexts, func(a store.Account) bool {
		return a.Kind == "org"
	}))
	if c, err := r.Cookie(sessionCookie); err == nil {
		n.Sessions = s.walletAccounts(r, c.Value)
	}
	// templui's sidebar script writes this cookie but never reads it back, so
	// the rail is only ever collapsed until the next render. Render it.
	if c, err := r.Cookie("sidebar_state"); err == nil {
		n.RailCollapsed = c.Value == "false"
	}
	return n
}

// handleOrgsPage lists the organizations the viewer can act in — every one on
// the instance for an admin, their own memberships for everyone else.
func (s *Server) handleOrgsPage(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	d := views.OrgsData{
		Nav: s.nav(r, u), Flash: s.popFlash(w, r),
		IsAdmin:   u.Superadmin(),
		CanCreate: s.cfg.SelfService && s.userCanCreateOrg(u),
	}
	var accounts []store.Account
	var err error
	if d.IsAdmin {
		accounts, err = s.db.ListAccounts()
	} else {
		accounts, err = s.db.UserAccounts(u.ID)
	}
	if err != nil {
		uiError(w, r, err)
		return
	}
	month := time.Now().UTC().Format("2006-01")
	for _, acct := range accounts {
		if acct.Kind != "org" { // personal accounts are not organizations
			continue
		}
		info, err := s.orgInfo(acct, month)
		if err != nil {
			uiError(w, r, err)
			return
		}
		d.Orgs = append(d.Orgs, info)
	}
	views.OrgsPage(d).Render(r.Context(), w)
}

// handleContext persists the account-context choice.
func (s *Server) handleContext(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	val := strings.TrimSpace(r.FormValue("ctx"))
	if val != "" {
		acc, err := s.db.GetAccount(val)
		if err != nil || s.db.MemberRole(acc.ID, u.ID) == "" {
			val = ""
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name: ctxCookie, Value: val, Path: "/",
		MaxAge:   int((30 * 24 * time.Hour).Seconds()),
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.secureCookies(),
	})
	http.Redirect(w, r, refererPath(r), http.StatusSeeOther)
}

// refererPath is the same-origin path the request came from ("/admin" when
// absent or cross-origin) — POST-and-return for controls that live on every
// page, like the context switcher.
func refererPath(r *http.Request) string {
	ref, err := url.Parse(r.Header.Get("Referer"))
	if err != nil || ref.Host != r.Host || !strings.HasPrefix(ref.Path, "/admin") {
		return "/admin"
	}
	return ref.Path
}

// requireUser gates per-account endpoints: any signed-in user, plus a
// same-origin check on POSTs (CSRF defense-in-depth beyond SameSite=Lax).
func (s *Server) requireUser(w http.ResponseWriter, r *http.Request) *store.User {
	u := s.currentUser(r)
	if u == nil {
		// htmx fragment requests must not receive the login page (the swap
		// would splice it into the open page) — send a client-side redirect.
		if r.Header.Get("HX-Request") == "true" {
			w.Header().Set("HX-Redirect", "/admin")
			w.WriteHeader(http.StatusUnauthorized)
			return nil
		}
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return nil
	}
	if r.Method == http.MethodPost && !s.sameOrigin(r) {
		uiFail(w, r, http.StatusForbidden, views.T(r.Context(), "err.crossorigin"), nil)
		return nil
	}
	return u
}

// requireAdmin additionally demands the admin role — the gate for every
// cache/token/user mutation and instance-wide view.
func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	u := s.requireUser(w, r)
	if u == nil {
		return false
	}
	if !u.Superadmin() {
		uiFail(w, r, http.StatusForbidden, views.T(r.Context(), "err.superadmin"), nil)
		return false
	}
	return true
}

// sameOrigin checks the Origin (or Referer) host matches the request host.
func (s *Server) sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		origin = r.Header.Get("Referer")
	}
	if origin == "" {
		return true // non-browser client (curl) with a valid session cookie
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host == r.Host
}

func (s *Server) registerAdmin(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin", s.handleAdmin)
	mux.HandleFunc("POST /admin/login", s.handleLogin)
	mux.HandleFunc("POST /admin/login/code", s.handleLoginCode)
	mux.HandleFunc("POST /admin/logout", s.handleLogout)
	mux.HandleFunc("GET /admin/signin", s.handleSignInPage)
	mux.HandleFunc("POST /admin/session/switch", s.handleSwitchAccount)
	mux.HandleFunc("POST /admin/caches", s.handleCreateCache)
	mux.HandleFunc("GET /admin/cache/{account}/{name}", s.handleCacheDetail)
	mux.HandleFunc("GET /admin/cache/{account}/{name}/path/{hash}", s.handlePathDetail)
	mux.HandleFunc("POST /admin/cache/{account}/{name}/configure", s.handleConfigureCache)
	mux.HandleFunc("POST /admin/cache/{account}/{name}/rotate", s.handleRotateKey)
	mux.HandleFunc("POST /admin/cache/{account}/{name}/delete", s.handleDeleteCache)
	mux.HandleFunc("POST /admin/cache/{account}/{name}/tokens", s.handleCacheToken)
	mux.HandleFunc("POST /admin/orgs", s.handleCreateOrg)
	mux.HandleFunc("POST /admin/org/{slug}/delete", s.handleDeleteOrg)
	mux.HandleFunc("POST /admin/org/{slug}/members", s.handleSetMember)
	mux.HandleFunc("POST /admin/org/{slug}/members/{uid}/remove", s.handleRemoveMember)
	mux.HandleFunc("POST /admin/tokens", s.handleCreateToken)
	mux.HandleFunc("POST /admin/tokens/{id}/edit", s.handleEditToken)
	mux.HandleFunc("POST /admin/tokens/{id}/revoke", s.handleRevokeToken)
	mux.HandleFunc("POST /admin/gc", s.handleGC)
	mux.HandleFunc("GET /admin/settings", s.handleInstancePage)
	mux.HandleFunc("GET /admin/account", s.handleAccountPage)
	mux.HandleFunc("POST /admin/account/email", s.handleAccountEmail)
	mux.HandleFunc("POST /admin/account/appearance", s.handleAppearance)
	mux.HandleFunc("POST /admin/context", s.handleContext)
	// Instance rules are not part of the self-service surface: a hobbyist
	// instance has ceilings and defaults too.
	mux.HandleFunc("POST /admin/settings/rules", s.handleInstanceRules)
	mux.HandleFunc("GET /admin/console", s.handleConsole)
	mux.HandleFunc("POST /admin/org/{slug}/status", s.handleAccountStatus)
	mux.HandleFunc("GET /admin/orgs", s.handleOrgsPage)
	mux.HandleFunc("GET /admin/org/{slug}", s.handleOrgPage)
	mux.HandleFunc("GET /admin/status", s.handleStatus)
	mux.HandleFunc("GET /admin/status/data", s.handleStatusData)
	mux.HandleFunc("GET /admin/audit", s.handleAudit)
	mux.HandleFunc("POST /admin/account/password", s.handleChangePassword)
	mux.HandleFunc("POST /admin/account/password/check", s.handlePasswordCheck)
	mux.HandleFunc("POST /admin/account/totp/enroll", s.handleTOTPEnroll)
	mux.HandleFunc("POST /admin/account/totp/enable", s.handleTOTPEnable)
	mux.HandleFunc("POST /admin/account/totp/disable", s.handleTOTPDisable)
	mux.HandleFunc("POST /admin/users", s.handleCreateUser)
	mux.HandleFunc("POST /admin/users/{id}/reset", s.handleUserReset)
	mux.HandleFunc("POST /admin/users/{id}/delete", s.handleUserDelete)
}

// hasPasskeys reports whether any WebAuthn credential is registered.
func (s *Server) hasPasskeys() bool {
	pks, _ := s.db.ListPasskeys()
	return len(pks) > 0
}

func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	if !s.loggedIn(r) {
		views.Login(!s.db.UsersExist(), s.hasPasskeys(), s.registrationOpen(), views.Flash{}).Render(r.Context(), w)
		return
	}
	s.renderDashboard(w, r, s.popFlash(w, r))
}

// canManage reports whether u may mutate resources in an account: the
// instance owner always, otherwise the account's owner or admins.
func (s *Server) canManage(u *store.User, nsID int64) bool {
	if u == nil {
		return false
	}
	if u.Superadmin() {
		return true
	}
	mr := s.db.MemberRole(nsID, u.ID)
	return mr == "owner" || mr == "admin"
}

// visibleCaches lists the caches u may see: all for instance admins, their
// own accounts' for everyone else.
func (s *Server) visibleCaches(u *store.User) ([]store.Cache, error) {
	if u.Superadmin() {
		return s.db.ListCaches()
	}
	nss, err := s.db.UserAccounts(u.ID)
	if err != nil {
		return nil, err
	}
	var out []store.Cache
	for _, ns := range nss {
		cs, err := s.db.ListAccountCaches(ns.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, cs...)
	}
	return out, nil
}

// visibleTokens lists tokens u may see: all for admins, else the tokens of
// accounts they administer.
func (s *Server) visibleTokens(u *store.User) ([]store.Token, error) {
	if u.Superadmin() {
		return s.db.ListTokens()
	}
	nss, err := s.db.UserAccounts(u.ID)
	if err != nil {
		return nil, err
	}
	var out []store.Token
	for _, ns := range nss {
		if mr := s.db.MemberRole(ns.ID, u.ID); mr != "owner" && mr != "admin" {
			continue
		}
		ts, err := s.db.ListAccountTokens(ns.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, ts...)
	}
	return out, nil
}

// ownedAccounts returns the accounts u may create caches/tokens in.
func (s *Server) ownedAccounts(u *store.User) ([]store.Account, error) {
	if u.Superadmin() {
		return s.db.ListAccounts()
	}
	nss, err := s.db.UserAccounts(u.ID)
	if err != nil {
		return nil, err
	}
	var out []store.Account
	for _, ns := range nss {
		if mr := s.db.MemberRole(ns.ID, u.ID); mr == "owner" || mr == "admin" {
			out = append(out, ns)
		}
	}
	return out, nil
}

func (s *Server) renderDashboard(w http.ResponseWriter, r *http.Request, flash views.Flash) {
	u := s.currentUser(r)
	if u == nil {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	caches, err := s.visibleCaches(u)
	if err != nil {
		uiError(w, r, err)
		return
	}
	// Context switcher: scope to the chosen account.
	if ctx := s.activeContext(r, u); ctx != "" {
		kept := caches[:0]
		for _, c := range caches {
			if c.Account == ctx {
				kept = append(kept, c)
			}
		}
		caches = kept
	}
	usages := make([]views.CacheUsage, 0, len(caches))
	for _, c := range caches {
		st, err := s.db.StatsFor(c.ID)
		if err != nil {
			uiError(w, r, err)
			return
		}
		usages = append(usages, views.CacheUsage{Cache: c, Bytes: st.PhysicalBytes, Logical: st.LogicalBytes, Paths: st.Paths})
	}
	global, err := s.db.GlobalStatsFor()
	if err != nil {
		uiError(w, r, err)
		return
	}
	// The page is always one account's, so the figures above it are too.
	global = store.Global{Caches: int64(len(usages))}
	for _, us := range usages {
		global.Paths += us.Paths
		global.StoredBytes += us.Bytes
		global.LogicalBytes += us.Logical
	}
	tokens, err := s.visibleTokens(u)
	if err != nil {
		uiError(w, r, err)
		return
	}
	// Scope tokens to the chosen account, same as caches and the KPIs.
	if ctx := s.activeContext(r, u); ctx != "" {
		kept := tokens[:0]
		for _, t := range tokens {
			if t.Account == ctx {
				kept = append(kept, t)
			}
		}
		tokens = kept
	}
	owned, err := s.ownedAccounts(u)
	if err != nil {
		uiError(w, r, err)
		return
	}
	// The full visible list, captured before search/paging mutate `usages`
	// in place — the token dialog's scope picker must see every cache.
	allCaches := append([]views.CacheUsage(nil), usages...)
	cq := strings.TrimSpace(r.URL.Query().Get("caches[q]"))
	if cq != "" {
		kept := usages[:0]
		for _, u := range usages {
			if fuzzyMatch(u.Cache.Ref(), cq) {
				kept = append(kept, u)
			}
		}
		usages = kept
	}
	tq := strings.TrimSpace(r.URL.Query().Get("tokens[q]"))
	if tq != "" {
		kept := tokens[:0]
		for _, t := range tokens {
			if fuzzyMatch(t.Name+" "+strings.Join(t.Caches, " "), tq) {
				kept = append(kept, t)
			}
		}
		tokens = kept
	}
	tkey, tdir := sortParams(r, "tokens[sort]", "tokens[dir]", "name", "perms", "scope", "expires", "status")
	sortTokens(tokens, tkey, tdir)
	cnum, csize := pageParams(r, "caches", 25)
	tnum, tsize := pageParams(r, "tokens", 25)
	pagedCaches, cpage, cpages := views.PageOf(usages, cnum, csize)
	pagedTokens, tpage, tpages := views.PageOf(tokens, tnum, tsize)
	q := r.URL.Query()
	views.Dashboard(views.DashboardData{
		Nav:        s.nav(r, u),
		Global:     global,
		Caches:     pagedCaches,
		AllCaches:  allCaches,
		Tokens:     pagedTokens,
		Accounts:   owned,
		Storages:   s.storageNames(),
		IsAdmin:    u.Superadmin(),
		Flash:      flash,
		ServerCap:  s.instanceCap(),
		Bytes:      humanBytes,
		CacheQuery: cq,
		TokenQuery: tq,
		CachePager: withTarget(makePager("/admin", q, "caches", cpage, cpages), "#cache-list"),
		TokenPager: withTarget(makePager("/admin", q, "tokens", tpage, tpages), "#token-list"),
		TokenSort: views.SortCtx{
			Path: "/admin", Query: q,
			SortParam: "tokens[sort]", DirParam: "tokens[dir]", PageParam: "tokens[number]",
			Key: tkey, Dir: tdir, Target: "#token-list",
		},
	}).Render(r.Context(), w)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.logins.allow(s.clientIP(r)) {
		s.metrics.authFailures.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		views.Login(false, s.hasPasskeys(), s.registrationOpen(), views.Flash{Msg: views.T(r.Context(), "err.throttled")}).Render(r.Context(), w)
		return
	}
	u, err := s.db.GetUserByLogin(strings.TrimSpace(r.FormValue("username")))
	if errors.Is(err, store.ErrNotFound) {
		if !s.db.UsersExist() {
			views.Login(true, s.hasPasskeys(), s.registrationOpen(), views.Flash{}).Render(r.Context(), w)
			return
		}
		// Burn a bcrypt anyway so unknown usernames cost the same as wrong
		// passwords (no user-enumeration timing signal).
		bcrypt.CompareHashAndPassword([]byte("$2a$10$0000000000000000000000000000000000000000000000000000"), []byte(r.FormValue("password")))
		views.Login(false, s.hasPasskeys(), s.registrationOpen(), views.Flash{Msg: views.T(r.Context(), "flash.badlogin")}).Render(r.Context(), w)
		return
	}
	if err != nil {
		uiError(w, r, err)
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PassHash), []byte(r.FormValue("password"))) != nil {
		views.Login(false, s.hasPasskeys(), s.registrationOpen(), views.Flash{Msg: views.T(r.Context(), "flash.badlogin")}).Render(r.Context(), w)
		return
	}
	if u.Status == "pending" {
		views.Login(false, s.hasPasskeys(), s.registrationOpen(), views.Flash{Msg: views.T(r.Context(), "flash.awaiting")}).Render(r.Context(), w)
		return
	}
	// Password accepted. With 2FA on, the code is a second step gated by a
	// short-lived pre-auth ticket — the password never rides along again.
	if u.TOTPEnabled {
		pid, err := s.sess.createPending(u.ID)
		if err != nil {
			uiFail(w, r, http.StatusInternalServerError, views.T(r.Context(), "err.session"), err)
			return
		}
		views.LoginCode(pid, views.Flash{}).Render(r.Context(), w)
		return
	}
	s.grantSession(w, r, u.ID)
}

// handleLoginCode is step two: a valid pre-auth ticket plus a TOTP code.
func (s *Server) handleLoginCode(w http.ResponseWriter, r *http.Request) {
	// Same bucket as passwords: a 6-digit TOTP is brute-forceable without it.
	if !s.logins.allow(s.clientIP(r)) {
		s.metrics.authFailures.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		views.LoginCode(r.FormValue("pending"), views.Flash{Msg: views.T(r.Context(), "err.throttled")}).Render(r.Context(), w)
		return
	}
	pid := r.FormValue("pending")
	uid, ok := s.sess.pendingUser(pid)
	if !ok {
		views.Login(false, s.hasPasskeys(), s.registrationOpen(), views.Flash{Msg: views.T(r.Context(), "flash.loginexpired")}).Render(r.Context(), w)
		return
	}
	secret, on, _ := s.db.UserTOTP(uid)
	if !on {
		// 2FA turned off mid-flight; the password step already passed.
		s.sess.consumePending(pid)
		s.grantSession(w, r, uid)
		return
	}
	if !totpVerify(secret, r.FormValue("code"), time.Now()) {
		// Burn the ticket on any wrong code so one pre-auth ticket can't be
		// retried against the ±1-step window for its whole 3-min lifetime;
		// the user re-does the password step, which the limiter throttles.
		s.sess.consumePending(pid)
		views.Login(false, s.hasPasskeys(), s.registrationOpen(), views.Flash{Msg: views.T(r.Context(), "flash.bad2fa")}).Render(r.Context(), w)
		return
	}
	s.sess.consumePending(pid)
	s.grantSession(w, r, uid)
}

// grantSession issues the session cookie and lands on the dashboard.
func (s *Server) grantSession(w http.ResponseWriter, r *http.Request, userID int64) {
	id, err := s.sess.create(userID)
	if err != nil {
		uiFail(w, r, http.StatusInternalServerError, views.T(r.Context(), "err.session"), err)
		return
	}
	s.addToWallet(w, r, id, userID)
	s.setSessionCookie(w, id)
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// reissueSession swaps a freshly minted session in for the current one, in
// the cookie and in the wallet both.
func (s *Server) reissueSession(w http.ResponseWriter, r *http.Request, id string) {
	old := ""
	if c, err := r.Cookie(sessionCookie); err == nil {
		old = c.Value
	}
	s.replaceInWallet(w, r, old, id)
	s.setSessionCookie(w, id)
}

// setSessionCookie sets the session cookie with Max-Age matching the
// server-side TTL, so the login survives browser restarts until it expires.
func (s *Server) setSessionCookie(w http.ResponseWriter, id string) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: id, Path: "/",
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.secureCookies(),
	})
}

func (s *Server) handleAccountPage(w http.ResponseWriter, r *http.Request) {
	if u := s.requireUser(w, r); u != nil {
		s.renderAccount(w, r, u, s.popFlash(w, r))
	}
}

// renderAccount is the personal page: identity + credentials.
func (s *Server) renderAccount(w http.ResponseWriter, r *http.Request, u *store.User, flash views.Flash) {
	pks, err := s.db.ListUserPasskeys(u.ID)
	if err != nil {
		uiError(w, r, err)
		return
	}
	views.Account(views.AccountData{
		Nav: s.nav(r, u), User: u, TOTPEnabled: u.TOTPEnabled, Passkeys: pks,
		CanCreateOrg: s.cfg.SelfService && s.userCanCreateOrg(u),
		Flash:        flash,
	}).Render(r.Context(), w)
}

// handleAccountEmail updates the sign-in alias.
func (s *Server) handleAccountEmail(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	email := strings.TrimSpace(r.FormValue("email"))
	if s.cfg.SelfService && !validEmail(email) {
		s.accountFlash(w, r, views.T(r.Context(), "flash.emailreq"))
		return
	}
	if err := s.db.SetUserEmail(u.ID, email); err != nil {
		s.flashErr(w, r, "/admin/account", views.T(r.Context(), "flash.emailfailed"), err)
		return
	}
	u.Email = email
	msg := views.T(r.Context(), "flash.emailsaved")
	if email == "" {
		msg = views.T(r.Context(), "flash.emailcleared")
	}
	s.accountFlash(w, r, msg)
}

// handleAppearance saves the user's palette and UI language. Unknown ids fall
// back to the default rather than erroring: the only way to send one is to
// edit the form. An empty locale means "follow the browser".
func (s *Server) handleAppearance(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	theme := r.FormValue("theme")
	if !views.ValidPalette(theme) {
		theme = ""
	}
	locale := r.FormValue("locale")
	if locale != "" && !views.ValidLocale(locale) {
		locale = ""
	}
	if err := s.db.SetUserTheme(u.ID, theme); err != nil {
		uiError(w, r, err)
		return
	}
	if err := s.db.SetUserLocale(u.ID, locale); err != nil {
		uiError(w, r, err)
		return
	}
	// Flash in the language just chosen, not the one the page was rendered in.
	msg := views.T(views.WithLocale(r.Context(), locale), "flash.appearancesaved")
	s.setFlash(w, msg, "")
	// The palette and the language are attributes of <html>, which a boosted
	// body swap leaves untouched: this one costs a reload.
	if hxRefresh(w, r) {
		return
	}
	http.Redirect(w, r, "/admin/account", http.StatusSeeOther)
}

func (s *Server) handleInstancePage(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	s.renderInstance(w, r, s.popFlash(w, r))
}

// orgInfo assembles one account's membership + usage.
func (s *Server) orgInfo(acct store.Account, month string) (views.OrgInfo, error) {
	members, err := s.db.ListMembers(acct.ID)
	if err != nil {
		return views.OrgInfo{}, err
	}
	info := views.OrgInfo{Account: acct, Members: members}
	info.Plan, _ = s.db.AccountPlan(&acct)
	info.Used, _ = s.db.AccountLogicalBytes(acct.ID)
	info.Egress = s.db.AccountEgress(acct.ID, month)
	return info, nil
}

// renderInstance is the admin-only general settings page.
func (s *Server) renderInstance(w http.ResponseWriter, r *http.Request, flash views.Flash) {
	u := s.currentUser(r)
	if u == nil {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	if !u.Superadmin() { // defense in depth: never leak the instance page
		uiFail(w, r, http.StatusForbidden, views.T(r.Context(), "err.superadmin"), nil)
		return
	}
	d := views.InstanceData{
		Nav: s.nav(r, u), Flash: flash,
		SelfService:   s.cfg.SelfService,
		InstanceCap:   s.settingInt(settingInstanceCap),
		DefaultRetain: s.defaultRetention(),
		MaxTokenTTL:   s.settingInt(settingMaxTokenTTL),
		DefaultPlan:   s.defaultPlan(),
	}
	var err error
	if d.Users, err = s.db.ListUsers(); err != nil {
		uiError(w, r, err)
		return
	}
	if s.cfg.SelfService {
		if d.Plans, err = s.db.ListPlans(); err != nil {
			uiError(w, r, err)
			return
		}
		d.AllowRegs = s.db.SettingBool("allow_registrations", false)
		d.RequireOK = s.db.SettingBool("require_approval", true)
	}
	views.Instance(d).Render(r.Context(), w)
}

// handleOrgPage renders one organization: members, caches, usage. Org members
// see it; org admins and instance admins manage it.
func (s *Server) handleOrgPage(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	s.renderOrg(w, r, u, r.PathValue("slug"), s.popFlash(w, r))
}

func (s *Server) renderOrg(w http.ResponseWriter, r *http.Request, u *store.User, slug string, flash views.Flash) {
	acct, err := s.db.GetAccount(slug)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		uiError(w, r, err)
		return
	}
	if !u.Superadmin() && s.db.MemberRole(acct.ID, u.ID) == "" {
		s.notFound(w, r) // no existence oracle
		return
	}
	info, err := s.orgInfo(*acct, time.Now().UTC().Format("2006-01"))
	if err != nil {
		uiError(w, r, err)
		return
	}
	caches, err := s.db.ListAccountCaches(acct.ID)
	if err != nil {
		uiError(w, r, err)
		return
	}
	d := views.OrgPageData{
		Nav: s.nav(r, u), Info: info, CanManage: s.canManage(u, acct.ID),
		Bytes: humanBytes, Flash: flash,
	}
	for _, c := range caches {
		st, err := s.db.StatsFor(c.ID)
		if err != nil {
			uiError(w, r, err)
			return
		}
		d.Caches = append(d.Caches, views.CacheUsage{Cache: c, Bytes: st.PhysicalBytes, Paths: st.Paths})
	}
	views.Org(d).Render(r.Context(), w)
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PassHash), []byte(r.FormValue("current"))) != nil {
		s.accountFlash(w, r, views.T(r.Context(), "flash.pwwrong"))
		return
	}
	next := r.FormValue("new")
	if len(next) > 72 {
		// bcrypt rejects inputs past 72 bytes; catch it as a validation error
		// rather than a 500 from GenerateFromPassword below.
		s.accountFlash(w, r, views.T(r.Context(), "flash.pwlong"))
		return
	}
	switch pwState(next, r.FormValue("confirm")) {
	case "short", "":
		s.accountFlash(w, r, views.T(r.Context(), "flash.pwshort"))
		return
	case "mismatch":
		s.accountFlash(w, r, views.T(r.Context(), "flash.pwmismatch"))
		return
	}
	nh, err := bcrypt.GenerateFromPassword([]byte(next), bcrypt.DefaultCost)
	if err != nil {
		uiError(w, r, err)
		return
	}
	if err := s.db.SetUserPassword(u.ID, string(nh)); err != nil {
		uiError(w, r, err)
		return
	}
	// Invalidate every existing session (a stolen cookie must not outlive the
	// change), then re-issue one for this browser so the user stays signed in.
	if err := s.db.DropUserSessions(u.ID); err != nil {
		uiError(w, r, err)
		return
	}
	if id, err := s.sess.create(u.ID); err == nil {
		s.reissueSession(w, r, id)
	}
	s.accountFlash(w, r, views.T(r.Context(), "flash.pwchanged"))
}

// pwState is the single source of truth for new-password validation: the
// debounced hint endpoint and the final submit both go through it.
// "" (empty), "short", "mismatch" reject; "weak" and "strong" pass.
func pwState(pw, confirm string) string {
	if pw == "" {
		return ""
	}
	if len(pw) < 8 {
		return "short"
	}
	if confirm != "" && confirm != pw {
		return "mismatch"
	}
	var lower, upper, digit, other bool
	for _, r := range pw {
		switch {
		case unicode.IsLower(r):
			lower = true
		case unicode.IsUpper(r):
			upper = true
		case unicode.IsDigit(r):
			digit = true
		default:
			other = true
		}
	}
	classes := 0
	for _, b := range []bool{lower, upper, digit, other} {
		if b {
			classes++
		}
	}
	if (len(pw) >= 12 && classes >= 3) || len(pw) >= 16 {
		return "strong"
	}
	return "weak"
}

// handlePasswordCheck renders the live hint for the settings form. Read-only:
// it never mutates, so a session (no same-origin dance) is enough.
func (s *Server) handlePasswordCheck(w http.ResponseWriter, r *http.Request) {
	if !s.loggedIn(r) {
		uiFail(w, r, http.StatusUnauthorized, views.T(r.Context(), "err.unauthorized"), nil)
		return
	}
	views.PwHint(pwState(r.FormValue("new"), r.FormValue("confirm"))).Render(r.Context(), w)
}

// handleTOTPEnroll generates a fresh secret, stores it (not yet enabled), and
// shows the QR + a confirm-code form.
func (s *Server) handleTOTPEnroll(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	secret, err := newTOTPSecret()
	if err != nil {
		uiError(w, r, err)
		return
	}
	if err := s.db.SetUserTOTPSecret(u.ID, secret); err != nil {
		uiError(w, r, err)
		return
	}
	uri := totpURI(secret, "xilo", u.Name+"@"+hostOf(s.cfg.BaseURL))
	qr, err := totpQRDataURI(uri)
	if err != nil {
		uiError(w, r, err)
		return
	}
	views.TOTPEnrollBody(qr, secretB32(secret), "").Render(r.Context(), w)
}

func (s *Server) handleTOTPEnable(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	secret, _, _ := s.db.UserTOTP(u.ID)
	if len(secret) == 0 || !totpVerify(secret, r.FormValue("code"), time.Now()) {
		uri := totpURI(secret, "xilo", u.Name+"@"+hostOf(s.cfg.BaseURL))
		qr, _ := totpQRDataURI(uri)
		views.TOTPEnrollBody(qr, secretB32(secret), views.T(r.Context(), "flash.badcode")).Render(r.Context(), w)
		return
	}
	if err := s.db.SetUserTOTPEnabled(u.ID, true); err != nil {
		uiError(w, r, err)
		return
	}
	u.TOTPEnabled = true
	s.accountFlash(w, r, views.T(r.Context(), "flash.totpon"))
}

func (s *Server) handleTOTPDisable(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	// Step-up: dropping the second factor is a security downgrade, so require
	// the current password (as changing it does). A borrowed/hijacked session
	// alone must not be able to strip 2FA.
	if bcrypt.CompareHashAndPassword([]byte(u.PassHash), []byte(r.FormValue("current"))) != nil {
		s.accountFlash(w, r, views.T(r.Context(), "flash.pwwrong"))
		return
	}
	if err := s.db.SetUserTOTPEnabled(u.ID, false); err != nil {
		uiError(w, r, err)
		return
	}
	u.TOTPEnabled = false
	// Dropping a second factor lowers assurance; invalidate other sessions and
	// re-issue this one so a stolen cookie can't ride the weakened account.
	if err := s.db.DropUserSessions(u.ID); err == nil {
		if id, err := s.sess.create(u.ID); err == nil {
			s.reissueSession(w, r, id)
		}
	}
	s.accountFlash(w, r, views.T(r.Context(), "flash.totpoff"))
}

// secureCookies marks session cookies Secure when the public base URL is HTTPS
// (the standard TLS-terminating-proxy deployment).
func (s *Server) secureCookies() bool {
	return strings.HasPrefix(s.cfg.BaseURL, "https://")
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	active := ""
	if c, err := r.Cookie(sessionCookie); err == nil {
		active = c.Value
		s.sess.drop(active)
	}
	// Only this account leaves; the browser keeps the others, and lands on
	// whichever it still holds.
	var kept []string
	for _, id := range walletIDs(r) {
		if id == active {
			continue
		}
		if _, ok := s.sess.user(id); ok {
			kept = append(kept, id)
		}
	}
	s.setWallet(w, kept)
	if len(kept) > 0 {
		s.setSessionCookie(w, kept[len(kept)-1])
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// fuzzyMatch reports whether every whitespace-separated term of q matches s
// as a case-insensitive subsequence — the in-memory twin of the SQL search.
func fuzzyMatch(s, q string) bool {
	s = strings.ToLower(s)
	for term := range strings.FieldsSeq(strings.ToLower(q)) {
		si := 0
		for _, r := range term {
			idx := strings.IndexRune(s[si:], r)
			if idx < 0 {
				return false
			}
			si += idx + 1
		}
	}
	return true
}

// sortParams reads and whitelists a table's sort key + direction.
func sortParams(r *http.Request, keyParam, dirParam string, allowed ...string) (key, dir string) {
	q := r.URL.Query()
	k := q.Get(keyParam)
	if slices.Contains(allowed, k) {
		key = k
	}
	dir = "desc"
	if q.Get(dirParam) == "asc" {
		dir = "asc"
	}
	return key, dir
}

// sortTokens orders tokens by a column key, in place.
func sortTokens(tokens []store.Token, key, dir string) {
	if key == "" {
		return
	}
	order := func(a, b store.Token) int {
		switch key {
		case "perms":
			return cmp.Compare(strings.Join(a.Perms, ","), strings.Join(b.Perms, ","))
		case "scope":
			return cmp.Compare(strings.Join(a.Caches, ","), strings.Join(b.Caches, ","))
		case "expires":
			// never (0) sorts after every real date
			ae, be := a.Expires, b.Expires
			if ae == 0 {
				ae = math.MaxInt64
			}
			if be == 0 {
				be = math.MaxInt64
			}
			return cmp.Compare(ae, be)
		case "status":
			return cmp.Compare(views.TokenStatus(a), views.TokenStatus(b))
		default: // name
			return cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
		}
	}
	slices.SortStableFunc(tokens, func(a, b store.Token) int {
		if dir == "asc" {
			return order(a, b)
		}
		return order(b, a)
	})
}

// withTarget scopes a pager's htmx swaps to one region.
func withTarget(p views.Pager, target string) views.Pager {
	p.Target = target
	return p
}

// pageParams reads a listing's "<group>[number]" and "<group>[size]" query
// params (JSON:API style). number is clamped to >= 1; size falls back to
// defSize and is capped at 200 so a URL can't request unbounded pages.
func pageParams(r *http.Request, group string, defSize int) (number, size int) {
	q := r.URL.Query()
	number, _ = strconv.Atoi(q.Get(group + "[number]"))
	if number < 1 {
		number = 1
	}
	size, _ = strconv.Atoi(q.Get(group + "[size]"))
	if size < 1 {
		size = defSize
	}
	if size > 200 {
		size = 200
	}
	return number, size
}

// makePager builds prev/next URLs for a listing, preserving other params
// (including the group's [size], which rides along untouched).
func makePager(path string, params url.Values, group string, page, pages int) views.Pager {
	mk := func(n int) string {
		v := url.Values{}
		maps.Copy(v, params)
		v.Set(group+"[number]", strconv.Itoa(n))
		return path + "?" + v.Encode()
	}
	pg := views.Pager{Page: page, Pages: pages}
	if page > 1 {
		pg.Prev = mk(page - 1)
	}
	if page < pages {
		pg.Next = mk(page + 1)
	}
	return pg
}

// formSeconds reads a "<name>_value" + "<name>_unit" (h|d) pair. ok is false
// when the value is empty or unparsable — callers keep their current setting.
// blank reports whether a form carried `name` and left it empty — an explicit
// "no value" from a form that always posts the field, as opposed to a request
// that never mentioned it (which keeps whatever is stored).
func blank(r *http.Request, name string) bool {
	vs, ok := r.Form[name]
	return ok && strings.TrimSpace(vs[0]) == ""
}

func formSeconds(r *http.Request, name string) (secs int64, ok bool) {
	v := strings.TrimSpace(r.FormValue(name + "_value"))
	if v == "" {
		return 0, false
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || n < 0 || math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, false
	}
	var mult float64
	switch r.FormValue(name + "_unit") {
	case "y":
		mult = 31536000 // 365 d
	case "mo":
		mult = 2592000 // 30 d
	case "d":
		mult = 86400
	default:
		mult = 3600 // h
	}
	return int64(n * mult), true
}

// formBytes reads a "<name>_value" + "<name>_unit" (MiB|GiB|TiB) pair with the
// same empty-keeps-current contract as formSeconds.
func formBytes(r *http.Request, name string) (bytes int64, ok bool) {
	v := strings.TrimSpace(r.FormValue(name + "_value"))
	if v == "" {
		return 0, false
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || n < 0 || math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, false
	}
	var shift uint
	switch r.FormValue(name + "_unit") {
	case "TiB":
		shift = 40
	case "MiB":
		shift = 20
	default:
		shift = 30 // GiB
	}
	return int64(n * float64(int64(1)<<shift)), true
}

// clampPriority bounds a form priority to [1,100]; fallback for absent/invalid.
func clampPriority(v string, fallback int) int {
	p, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || p == 0 {
		return fallback
	}
	if p < 1 {
		return 1
	}
	if p > 100 {
		return 100
	}
	return p
}

func (s *Server) handleCreateCache(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	ns := strings.TrimSpace(r.FormValue("namespace"))
	if ns == "" {
		// No silent "default" account: the picker always offers one, and
		// inventing a name here created an organization nobody asked for.
		s.flashRedirect(w, r, "/admin", views.T(r.Context(), "flash.pickaccount"))
		return
	}
	if strings.Contains(name, "/") || strings.Contains(ns, "/") {
		s.flashRedirect(w, r, "/admin", views.T(r.Context(), "flash.badname"))
		return
	}
	// Instance admins may create caches anywhere (minting the account on the
	// fly); everyone else only inside accounts they administer, within plan
	// quota.
	if !u.Superadmin() {
		acc, err := s.db.GetAccount(ns)
		if err != nil || !s.canManage(u, acc.ID) {
			s.flashRedirect(w, r, "/admin", views.T(r.Context(), "flash.notadmin"))
			return
		}
		if err := s.checkCacheQuota(r.Context(), acc); err != nil {
			// The quota message is itself a catalog string (quota.caches).
			s.flashRedirect(w, r, "/admin", err.Error())
			return
		}
	}
	priority := clampPriority(r.FormValue("priority"), 40)
	public := r.FormValue("private") == ""
	stName, err := s.resolveStorage(r.FormValue("storage"))
	if err != nil {
		uiFail(w, r, http.StatusBadRequest, views.T(r.Context(), "err.storage"), err)
		return
	}
	c, err := s.db.CreateCache(ns, name, public, priority)
	if err == nil {
		if retain := s.defaultRetention(); retain > 0 {
			if err := s.db.UpdateCache(c.ID, c.Public, c.Priority, retain, c.MaxBytes); err == nil {
				c.Retention = retain
			}
		}
	}
	if err != nil {
		s.flashStore(w, r, "/admin", err)
		return
	}
	if err := s.assignStorage(c, stName); err != nil {
		uiError(w, r, err)
		return
	}
	s.flashRedirect(w, r, "/admin/cache/"+c.Ref(), views.Tf(r.Context(), "flash.cachecreated", c.Ref()))
}

// notFound renders the styled 404 page (browser routes only).
func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNotFound)
	views.NotFound(s.nav(r, s.currentUser(r))).Render(r.Context(), w)
}

func (s *Server) handleCacheDetail(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	c, ok := s.cacheForUser(w, r, u)
	if !ok {
		return
	}
	s.renderCache(w, r, u, c, s.popFlash(w, r), "")
}

// cacheForUser resolves {account}/{name} and enforces visibility: members see
// their accounts' caches; outsiders get the same 404 as a nonexistent cache
// (no existence oracle).
func (s *Server) cacheForUser(w http.ResponseWriter, r *http.Request, u *store.User) (*store.Cache, bool) {
	c, err := s.db.GetCache(r.PathValue("account"), r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return nil, false
	}
	if err != nil {
		uiError(w, r, err)
		return nil, false
	}
	if !u.Superadmin() && s.db.MemberRole(c.AccountID, u.ID) == "" {
		s.notFound(w, r)
		return nil, false
	}
	return c, true
}

// renderCache draws the cache page. `secret` is a freshly minted token, shown
// exactly once: it is spliced into the setup snippets so the reader copies a
// working command instead of one with a <token> placeholder in it.
func (s *Server) renderCache(w http.ResponseWriter, r *http.Request, u *store.User, c *store.Cache, flash views.Flash, secret string) {
	st, err := s.db.StatsFor(c.ID)
	if err != nil {
		uiError(w, r, err)
		return
	}
	dedup := "1.00"
	if st.PhysicalBytes > 0 {
		dedup = fmt.Sprintf("%.2f", float64(st.LogicalBytes)/float64(st.PhysicalBytes))
	}
	page, perPage := pageParams(r, "page", 25)
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	skey, sdir := sortParams(r, "sort", "dir", "path", "size", "pulled")
	paths, total, err := s.db.SearchPaths(c.ID, q, perPage, (page-1)*perPage, skey, sdir)
	if err != nil {
		uiError(w, r, err)
		return
	}
	pages := max(int((total+int64(perPage)-1)/int64(perPage)), 1)
	if page > pages && total > 0 {
		// Past the end (e.g. stale link): show the last page instead of nothing.
		page = pages
		paths, total, err = s.db.SearchPaths(c.ID, q, perPage, (page-1)*perPage, skey, sdir)
		if err != nil {
			uiError(w, r, err)
			return
		}
	}
	views.CacheView(views.CacheData{
		Nav:       s.nav(r, u),
		Flash:     flash,
		Secret:    secret,
		CanManage: s.canManage(u, c.AccountID),
		Cache:     *c,
		Stats:     st,
		Dedup:     dedup,
		BaseURL:   s.cfg.BaseURL,
		Host:      hostOf(s.cfg.BaseURL),
		Bytes:     humanBytes,
		Paths:     paths,
		PathQuery: q,
		PathTotal: total,
		PathPager: makePager("/admin/cache/"+c.Ref(), r.URL.Query(), "page", page, pages),
		PathSort: views.SortCtx{
			Path: "/admin/cache/" + c.Ref(), Query: r.URL.Query(),
			SortParam: "sort", DirParam: "dir", PageParam: "page[number]",
			Key: skey, Dir: sdir,
		},
	}).Render(r.Context(), w)
}

// auditMethods / auditStatuses are the filter chips the activities page
// offers; anything else in the query is ignored.
var (
	auditMethods  = []string{"GET", "POST", "PUT", "PATCH", "DELETE"}
	auditStatuses = []string{"2xx", "3xx", "4xx", "5xx"}
)

// handleAudit renders the instance-wide activities page: summary tiles plus a
// searchable, filterable, sortable, paginated table. Admin-only, like the
// status page.
func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	page, perPage := pageParams(r, "page", 50)
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	method := r.URL.Query().Get("method")
	if !slices.Contains(auditMethods, method) {
		method = ""
	}
	status := r.URL.Query().Get("status")
	if !slices.Contains(auditStatuses, status) {
		status = ""
	}
	skey, sdir := sortParams(r, "sort", "dir", "time", "actor", "method", "path", "status")
	entries, total, err := s.db.SearchAudit(q, method, status, perPage, (page-1)*perPage, skey, sdir)
	if err != nil {
		uiError(w, r, err)
		return
	}
	pages := max(int((total+int64(perPage)-1)/int64(perPage)), 1)
	if page > pages && total > 0 {
		page = pages
		entries, total, err = s.db.SearchAudit(q, method, status, perPage, (page-1)*perPage, skey, sdir)
		if err != nil {
			uiError(w, r, err)
			return
		}
	}
	stats, err := s.db.AuditStats()
	if err != nil {
		uiError(w, r, err)
		return
	}
	views.AuditPage(views.AuditData{
		Nav:     s.nav(r, s.currentUser(r)),
		Entries: entries,
		Query:   q,
		Method:  method,
		Status:  status,
		Methods: auditMethods,
		Classes: auditStatuses,
		Stats:   stats,
		Total:   total,
		Pager:   makePager("/admin/audit", r.URL.Query(), "page", page, pages),
		Sort: views.SortCtx{
			Path: "/admin/audit", Query: r.URL.Query(),
			SortParam: "sort", DirParam: "dir", PageParam: "page[number]",
			Key: skey, Dir: sdir,
		},
	}).Render(r.Context(), w)
}

// handlePathDetail renders one store path: its narinfo fields, references and
// chunk composition. Visibility follows the cache page (members only, 404
// otherwise); an unknown hash is a 404 too.
func (s *Server) handlePathDetail(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	c, ok := s.cacheForUser(w, r, u)
	if !ok {
		return
	}
	p, err := s.db.GetPath(c.ID, r.PathValue("hash"))
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		uiError(w, r, err)
		return
	}
	// A path whose chunks were lost (what fsck reports) still gets a page: the
	// hashes render without sizes and the header says so.
	chunks, err := s.db.ChunkKeys(c.Storage, p.Chunks)
	broken := errors.Is(err, store.ErrNotFound)
	if err != nil && !broken {
		uiError(w, r, err)
		return
	}
	if broken {
		chunks = make([]store.ChunkRef, len(p.Chunks))
		for i, h := range p.Chunks {
			chunks[i] = store.ChunkRef{Hash: h}
		}
	}
	var csize int64
	for _, ch := range chunks {
		csize += ch.CSize
	}
	// References that live in this cache too become links.
	refHashes := make([]string, len(p.Refs))
	for i, ref := range p.Refs {
		refHashes[i] = narinfo.StoreHash(ref)
	}
	missing, err := s.db.MissingPaths(c.ID, refHashes)
	if err != nil {
		uiError(w, r, err)
		return
	}
	present := make(map[string]bool, len(refHashes))
	for _, h := range refHashes {
		present[h] = true
	}
	for _, h := range missing {
		delete(present, h)
	}
	page, perPage := pageParams(r, "chunks", 50)
	pageChunks, page, pages := views.PageOf(chunks, page, perPage)
	views.PathDetail(views.PathData{
		Nav:        s.nav(r, u),
		Cache:      *c,
		Path:       *p,
		Broken:     broken,
		CSize:      csize,
		Present:    present,
		Chunks:     pageChunks,
		ChunkPager: makePager(r.URL.Path+"#chunks", r.URL.Query(), "chunks", page, pages),
		BaseURL:    s.cfg.BaseURL,
		Bytes:      humanBytes,
	}).Render(r.Context(), w)
}

// manageCache resolves {ns}/{name} and enforces mutate rights (instance admin
// or account owner). Outsiders get 404, not 403 — no existence oracle.
func (s *Server) manageCache(w http.ResponseWriter, r *http.Request) (*store.Cache, bool) {
	u := s.requireUser(w, r)
	if u == nil {
		return nil, false
	}
	c, err := s.db.GetCache(r.PathValue("account"), r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return nil, false
	}
	if err != nil {
		uiError(w, r, err)
		return nil, false
	}
	if !s.canManage(u, c.AccountID) {
		http.NotFound(w, r)
		return nil, false
	}
	return c, true
}

func (s *Server) handleConfigureCache(w http.ResponseWriter, r *http.Request) {
	c, ok := s.manageCache(w, r)
	if !ok {
		return
	}
	priority := clampPriority(r.FormValue("priority"), c.Priority)
	public := r.FormValue("private") == ""
	// The settings list posts every field whenever one of them changes (it
	// saves as you go), so an emptied box is a decision — keep forever, no cap
	// — and not an omission. Anything unparseable still keeps what is stored.
	retention := c.Retention
	if secs, ok := formSeconds(r, "retention"); ok {
		retention = secs
	} else if blank(r, "retention_value") {
		retention = 0
	}
	maxBytes := c.MaxBytes
	if b, ok := formBytes(r, "max"); ok {
		maxBytes = b
	} else if blank(r, "max_value") {
		maxBytes = 0
	}
	if err := s.db.UpdateCache(c.ID, public, priority, retention, maxBytes); err != nil {
		uiError(w, r, err)
		return
	}
	// The settings list saves itself as the reader works, so this runs many
	// times from one page view and htmx swaps the answer in place. It gets the
	// fresh page directly instead of a 303, because the GET a redirect sends
	// the browser back for can be served out of its own five-second admin
	// cache — with the copy taken *before* this write. What makes that
	// impossible everywhere else is the flash cookie an action sets, and a
	// save nobody asked to be told about sets none. Without htmx it is still a
	// redirect, so a refresh can never repeat the action.
	if r.Header.Get("HX-Request") == "true" {
		if u := s.currentUser(r); u != nil {
			if fresh, err := s.db.GetCache(r.PathValue("account"), r.PathValue("name")); err == nil {
				// This page is already the one in the address bar; htmx must
				// not push the URL it posted to onto it.
				w.Header().Set("HX-Push-Url", "false")
				s.renderCache(w, r, u, fresh, views.Flash{}, "")
				return
			}
		}
	}
	http.Redirect(w, r, "/admin/cache/"+c.Ref(), http.StatusSeeOther)
}

func (s *Server) handleRotateKey(w http.ResponseWriter, r *http.Request) {
	c, ok := s.manageCache(w, r)
	if !ok {
		return
	}
	nc, err := s.db.RotateKey(c.ID, c.Name)
	if err != nil {
		uiError(w, r, err)
		return
	}
	s.flashRedirectCode(w, r, "/admin/cache/"+c.Ref(), views.T(r.Context(), "flash.rotated"), nc.PubKey)
}

func (s *Server) handleDeleteCache(w http.ResponseWriter, r *http.Request) {
	c, ok := s.manageCache(w, r)
	if !ok {
		return
	}
	if err := s.db.DeleteCache(c.ID); err != nil {
		uiError(w, r, err)
		return
	}
	s.flashRedirect(w, r, "/admin", views.Tf(r.Context(), "flash.cachedeleted", c.Ref()))
}

// tokenScope resolves the account a new token belongs to — always the acting
// user's viewing context, falling back to their personal account — and
// validates the cache scope within it. Instance-wide tokens are a CLI/API
// concept, never minted from the dashboard.
func (s *Server) tokenScope(u *store.User, r *http.Request) (nsID int64, nsName string, caches []string, err error) {
	// The submitted cache decides the owning account. It used to come from the
	// sidebar's viewing-context cookie instead — an invisible input that
	// silently minted the token against the wrong account whenever the
	// switcher happened to point elsewhere.
	ref := strings.TrimSpace(r.FormValue("cache"))
	nsName, bare, qualified := strings.Cut(ref, "/")
	if !qualified {
		// Unqualified: fall back to the viewing context, then the personal
		// account — the shape older forms posted.
		bare, nsName = ref, s.activeContext(r, u)
		if nsName == "" {
			nsName = u.Name
		}
	}
	if bare == "" {
		return 0, "", nil, errors.New("pick a cache")
	}
	ns, gerr := s.db.GetAccount(nsName)
	if gerr != nil {
		return 0, "", nil, errors.New("no such account")
	}
	if !s.canManage(u, ns.ID) {
		return 0, "", nil, errors.New("you do not administer " + nsName)
	}
	// A token is valid for exactly one cache, which must exist in that
	// account — a scope naming nothing can only ever 401.
	if _, gerr := s.db.GetCache(nsName, bare); gerr != nil {
		return 0, "", nil, errors.New("no cache " + nsName + "/" + bare)
	}
	// Account tokens store the bare name within their account.
	return ns.ID, nsName, []string{bare}, nil
}

func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	nsID, _, caches, err := s.tokenScope(u, r)
	if err != nil {
		uiFail(w, r, http.StatusForbidden, views.T(r.Context(), "flash.notadmin"), err)
		return
	}
	perms := formPerms(r)
	if len(perms) == 0 {
		perms = []string{"pull"}
	}
	var expires int64
	if r.FormValue("permanent") == "" {
		if secs, _ := strconv.ParseInt(r.FormValue("ttl"), 10, 64); secs > 0 {
			expires = time.Now().Unix() + secs
		}
	}
	expires = s.capTokenExpiry(expires)
	secret, t, err := s.db.CreateToken(nsID, name, caches, perms, expires)
	if err != nil {
		uiFail(w, r, http.StatusBadRequest, views.T(r.Context(), "err.tokenfailed"), err)
		return
	}
	s.renderDashboard(w, r, views.Flash{
		Msg:  views.Tf(r.Context(), "flash.tokencreated", t.Name),
		Code: secret,
	})
}

// handleCacheToken mints a token for the cache whose page the form was
// submitted from. Creating a token from the thing it grants makes its scope
// and owning account implicit — and correct — instead of depending on where
// the sidebar's account switcher happened to be pointing.
func (s *Server) handleCacheToken(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	c, ok := s.cacheForUser(w, r, u)
	if !ok {
		return
	}
	if !s.canManage(u, c.AccountID) {
		s.flashRedirect(w, r, "/admin/cache/"+c.Ref(), views.T(r.Context(), "flash.notadmin"))
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		name = c.Name
	}
	perms := formPerms(r)
	if len(perms) == 0 {
		perms = []string{"pull"}
	}
	var expires int64
	if r.FormValue("permanent") == "" {
		if secs, _ := strconv.ParseInt(r.FormValue("ttl"), 10, 64); secs > 0 {
			expires = time.Now().Unix() + secs
		}
	}
	secret, t, err := s.db.CreateToken(c.AccountID, name, []string{c.Name}, perms, expires)
	if err != nil {
		s.flashErr(w, r, "/admin/cache/"+c.Ref(), views.T(r.Context(), "err.tokenfailed"), err)
		return
	}
	// Rendered, not redirected: the secret exists only in this response.
	s.renderCache(w, r, u, c, views.Flash{
		Msg: views.Tf(r.Context(), "flash.tokencreated", t.Name), Code: secret,
	}, secret)
}

// manageToken resolves {id} and enforces mutate rights: admins for any token,
// owners for their account's tokens.
func (s *Server) manageToken(w http.ResponseWriter, r *http.Request) (*store.Token, *store.User, bool) {
	u := s.requireUser(w, r)
	if u == nil {
		return nil, nil, false
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	t, err := s.db.GetToken(id)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return nil, nil, false
	}
	if err != nil {
		uiError(w, r, err)
		return nil, nil, false
	}
	if t.AccountID == 0 && !u.Superadmin() {
		s.notFound(w, r)
		return nil, nil, false
	}
	if t.AccountID != 0 && !s.canManage(u, t.AccountID) {
		s.notFound(w, r)
		return nil, nil, false
	}
	return t, u, true
}

// handleEditToken rewrites a token's metadata. Expiry: the permanent switch
// clears it, a TTL value re-sets it counting from now, and leaving the TTL
// empty keeps the stored expiry.
func (s *Server) handleEditToken(w http.ResponseWriter, r *http.Request) {
	t, _, ok := s.manageToken(w, r)
	if !ok {
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		name = t.Name
	}
	var caches []string
	if c := r.FormValue("cache"); c != "" && c != "*" {
		if t.AccountID != 0 {
			bare, cut := strings.CutPrefix(c, t.Account+"/")
			if !cut {
				uiFail(w, r, http.StatusBadRequest, views.Tf(r.Context(), "err.tokenscope", t.Account), nil)
				return
			}
			caches = []string{bare}
		} else {
			caches = []string{c}
		}
	}
	// The single-cache scope rule itself is enforced in store.UpdateToken.
	perms := formPerms(r)
	// The dashboard edits push/pull/manage. Anything it cannot express —
	// today just the instance-wide "admin" grant — survives an edit untouched
	// rather than being silently dropped by a form that never showed it.
	for _, p := range t.Perms {
		if !slices.Contains(store.ManagePerms, p) && p != "push" && p != "pull" && !slices.Contains(perms, p) {
			perms = append(perms, p)
		}
	}
	expires := t.Expires
	if r.FormValue("permanent") != "" {
		expires = 0
	} else if secs, _ := strconv.ParseInt(r.FormValue("ttl"), 10, 64); secs > 0 {
		expires = time.Now().Unix() + secs
	}
	expires = s.capTokenExpiry(expires)
	if err := s.db.UpdateToken(t.ID, name, caches, perms, expires); err != nil {
		uiError(w, r, err)
		return
	}
	s.flashRedirect(w, r, "/admin", views.T(r.Context(), "flash.tokenupdated"))
}

func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	t, _, ok := s.manageToken(w, r)
	if !ok {
		return
	}
	if err := s.db.RevokeToken(t.ID); err != nil {
		uiError(w, r, err)
		return
	}
	s.flashRedirect(w, r, "/admin", views.T(r.Context(), "flash.tokenrevoked"))
}

func (s *Server) handleGC(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	deleted, freed, err := s.runGC(r.Context())
	if err != nil {
		uiError(w, r, err)
		return
	}
	s.instanceFlash(w, r, views.Tf(r.Context(), "flash.gcdone", deleted, humanBytes(freed)))
}

// ---- user management (admin role only) ----

const flashCookie = "xilo_flash"

// uiFail ends a browser request with a translated sentence and logs whatever
// really went wrong. Raw Go errors (library text, SQL, filesystem paths) are
// for the operator's log, never for the page: the user cannot act on them and
// they cannot be translated. Every hard error on an /admin route goes through
// here or through uiError; the API surfaces keep their own wire-contract text.
func uiFail(w http.ResponseWriter, r *http.Request, status int, msg string, err error) {
	if err != nil {
		log.Printf("admin: %s %s: %v", r.Method, r.URL.Path, err)
	}
	http.Error(w, msg, status)
}

// uiError is uiFail for the failures a user can do nothing about.
func uiError(w http.ResponseWriter, r *http.Request, err error) {
	uiFail(w, r, http.StatusInternalServerError, views.T(r.Context(), "err.internal"), err)
}

// flashErr is flashRedirect for a failed action: the user gets a translated
// sentence, the log gets the Go error. Use it wherever the old code flashed
// err.Error() straight onto the page.
func (s *Server) flashErr(w http.ResponseWriter, r *http.Request, path, msg string, err error) {
	log.Printf("admin: %s %s: %v", r.Method, r.URL.Path, err)
	s.flashRedirect(w, r, path, msg)
}

// storeMsg is the store's user-facing refusals and the catalog key each one
// reads as. The sentinel carries English for the CLI and the log; the page
// gets the translation.
var storeMsg = []struct {
	err error
	key string
}{
	{store.ErrOwnWorkspace, "flash.ownworkspace"},
	{store.ErrBadStatus, "flash.badstatus"},
	{store.ErrBadRole, "flash.badrole"},
	{store.ErrOwnerRole, "flash.ownerrole"},
	{store.ErrHasOwner, "flash.hasowner"},
	{store.ErrOwnerLocked, "flash.ownerlocked"},
	{store.ErrPlanInUse, "flash.planinuse"},
	{store.ErrNameTaken, "flash.nametaken"},
	{store.ErrSlugReserved, "flash.slugreserved"},
	{store.ErrNotFound, "flash.nouser"},
}

// flashStore lands a failed write back on path. A refusal the user can act on
// gets its own sentence; anything else is an internal failure, so it gets the
// generic one and the detail goes to the log.
func (s *Server) flashStore(w http.ResponseWriter, r *http.Request, path string, err error) {
	for _, m := range storeMsg {
		if errors.Is(err, m.err) {
			s.flashRedirect(w, r, path, views.T(r.Context(), m.key))
			return
		}
	}
	s.flashErr(w, r, path, views.T(r.Context(), "flash.savefailed"), err)
}

// flashRedirect stores a one-shot flash in a cookie and 303-redirects (PRG:
// a refresh re-fetches the page instead of re-executing the action). Never
// used for secrets — those render directly and are shown exactly once.
// setFlash arms the one-shot flash cookie the next GET pops. Only the redirect
// helpers and the handlers that answer a boosted request with a reload use it
// directly — everything else goes through flashRedirect.
func (s *Server) setFlash(w http.ResponseWriter, msg, code string) {
	v := url.QueryEscape(msg)
	if code != "" {
		v += "|" + url.QueryEscape(code)
	}
	http.SetCookie(w, &http.Cookie{
		Name: flashCookie, Value: v, Path: "/admin",
		MaxAge: 60, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.secureCookies(),
	})
}

// hxRefresh answers a boosted request with a full page load instead of a body
// swap, for the few changes a swap cannot carry: <html> holds the palette and
// the language, and hx-boost never replaces it. A redirect would not do — the
// browser follows the 303 itself, so htmx only ever sees the headers of the
// page that comes back, never the ones on the redirect.
func hxRefresh(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("HX-Boosted") != "true" {
		return false
	}
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusNoContent)
	return true
}

func (s *Server) flashRedirect(w http.ResponseWriter, r *http.Request, path, msg string) {
	s.flashRedirectCode(w, r, path, msg, "")
}

// flashRedirectCode is flashRedirect with a copyable code box (e.g. a rotated
// public key). Not for secrets.
func (s *Server) flashRedirectCode(w http.ResponseWriter, r *http.Request, path, msg, code string) {
	s.setFlash(w, msg, code)
	// A boosted navigation is a whole-page swap, so it can follow the plain 303
	// itself and land on the target without a reload. Only a fragment request
	// needs the client-side redirect: its target is one region of the page, and
	// splicing a full page into it would nest the app inside itself.
	if r.Header.Get("HX-Request") == "true" && r.Header.Get("HX-Boosted") != "true" {
		w.Header().Set("HX-Redirect", path)
		return
	}
	http.Redirect(w, r, path, http.StatusSeeOther)
}

// popFlash reads and clears the one-shot flash cookie set by flashRedirect.
func (s *Server) popFlash(w http.ResponseWriter, r *http.Request) views.Flash {
	c, err := r.Cookie(flashCookie)
	if err != nil || c.Value == "" {
		return views.Flash{}
	}
	http.SetCookie(w, &http.Cookie{Name: flashCookie, Path: "/admin", MaxAge: -1})
	raw, code := c.Value, ""
	if i := strings.IndexByte(raw, '|'); i >= 0 {
		raw, code = raw[:i], raw[i+1:]
	}
	msg, _ := url.QueryUnescape(raw)
	codeVal, _ := url.QueryUnescape(code)
	return views.Flash{Msg: msg, Code: codeVal}
}

// instanceFlash lands the user back on the settings page with a message.
func (s *Server) instanceFlash(w http.ResponseWriter, r *http.Request, msg string) {
	s.flashRedirect(w, r, "/admin/settings", msg)
}

// orgsFlash lands the user back on the organizations page with a message.
func (s *Server) orgsFlash(w http.ResponseWriter, r *http.Request, msg string) {
	s.flashRedirect(w, r, "/admin/orgs", msg)
}

// accountFlash lands the user back on their account page with a message.
func (s *Server) accountFlash(w http.ResponseWriter, r *http.Request, msg string) {
	s.flashRedirect(w, r, "/admin/account", msg)
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	name := strings.TrimSpace(r.FormValue("username"))
	email := strings.TrimSpace(r.FormValue("email"))
	pw := r.FormValue("password")
	if name == "" {
		s.instanceFlash(w, r, views.T(r.Context(), "flash.userreq"))
		return
	}
	if s.cfg.SelfService && !validEmail(email) {
		s.instanceFlash(w, r, views.T(r.Context(), "flash.emailreq"))
		return
	}
	if len(pw) < 8 {
		s.instanceFlash(w, r, views.T(r.Context(), "flash.pwshort"))
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		uiError(w, r, err)
		return
	}
	if _, err := s.db.CreateUser(name, email, string(hash), "user"); err != nil {
		s.flashStore(w, r, "/admin/settings", err)
		return
	}
	s.instanceFlash(w, r, views.Tf(r.Context(), "flash.usercreated", name))
}

// userByPath resolves the {id} path value to a user, or writes 404.
func (s *Server) userByPath(w http.ResponseWriter, r *http.Request) (*store.User, bool) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	u, err := s.db.GetUser(id)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return nil, false
	}
	if err != nil {
		uiError(w, r, err)
		return nil, false
	}
	return u, true
}

func (s *Server) handleUserReset(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	u, ok := s.userByPath(w, r)
	if !ok {
		return
	}
	pw := r.FormValue("password")
	if len(pw) < 8 {
		s.instanceFlash(w, r, views.T(r.Context(), "flash.pwshort"))
		return
	}
	if len(pw) > 72 {
		s.instanceFlash(w, r, views.T(r.Context(), "flash.pwlong"))
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		uiError(w, r, err)
		return
	}
	if err := s.db.SetUserPassword(u.ID, string(hash)); err != nil {
		uiError(w, r, err)
		return
	}
	// Admin reset targets another user: log them out everywhere so any live
	// (possibly attacker-held) session dies with the old password.
	if err := s.db.DropUserSessions(u.ID); err != nil {
		uiError(w, r, err)
		return
	}
	s.instanceFlash(w, r, views.Tf(r.Context(), "flash.pwreset", u.Name))
}

func (s *Server) handleUserDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	u, ok := s.userByPath(w, r)
	if !ok {
		return
	}
	if acting := s.currentUser(r); acting != nil && acting.ID == u.ID {
		s.instanceFlash(w, r, views.T(r.Context(), "flash.delself"))
		return
	}
	if u.Superadmin() {
		s.instanceFlash(w, r, views.T(r.Context(), "flash.delowner"))
		return
	}
	if s.db.OwnsOrgs(u.ID) {
		s.instanceFlash(w, r, views.T(r.Context(), "flash.ownsorgs"))
		return
	}
	if err := s.db.DeleteUser(u.ID); err != nil {
		uiError(w, r, err)
		return
	}
	s.instanceFlash(w, r, views.Tf(r.Context(), "flash.userdeleted", u.Name))
}

// ---- account management ----

func (s *Server) handleCreateOrg(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" || strings.ContainsAny(name, "/ ") {
		s.orgsFlash(w, r, views.T(r.Context(), "flash.badorgname"))
		return
	}
	org, err := s.db.EnsureAccount(name, "org")
	if errors.Is(err, store.ErrSlugReserved) {
		s.orgsFlash(w, r, views.T(r.Context(), "flash.nametaken"))
		return
	}
	if err != nil {
		uiError(w, r, err)
		return
	}
	if u := s.currentUser(r); u != nil {
		_ = s.db.MakeOwner(org.ID, u.ID)
	}
	s.orgsFlash(w, r, views.Tf(r.Context(), "flash.orgready", name))
}

// orgByPath resolves the {slug} path value to an ORG the acting user may
// manage (org admin or instance admin). 404s hide existence.
func (s *Server) orgByPath(w http.ResponseWriter, r *http.Request) (*store.Account, *store.User, bool) {
	u := s.requireUser(w, r)
	if u == nil {
		return nil, nil, false
	}
	ns, err := s.db.GetAccount(r.PathValue("slug"))
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return nil, nil, false
	}
	if err != nil {
		uiError(w, r, err)
		return nil, nil, false
	}
	if !s.canManage(u, ns.ID) {
		s.notFound(w, r)
		return nil, nil, false
	}
	return ns, u, true
}

// handleDeleteOrg cascades an organization away — memberships, tokens,
// caches, paths; orphaned chunk blobs leave disk on the GC sweep kicked
// below. Only the org's owner (or the instance owner) may do this: admins
// manage an org, owners destroy it.
func (s *Server) handleDeleteOrg(w http.ResponseWriter, r *http.Request) {
	ns, u, ok := s.orgByPath(w, r)
	if !ok {
		return
	}
	if !u.Superadmin() && s.db.MemberRole(ns.ID, u.ID) != "owner" {
		s.flashRedirect(w, r, "/admin/org/"+ns.Slug, views.T(r.Context(), "flash.ownerdelete"))
		return
	}
	if err := s.db.DeleteOrg(ns.ID); err != nil {
		s.flashStore(w, r, "/admin/org/"+ns.Slug, err)
		return
	}
	go s.runGC(context.Background())
	s.orgsFlash(w, r, views.Tf(r.Context(), "flash.orgdeleted", ns.Slug))
}

// memberTarget resolves who a membership post is about: the typed username or
// email from the add dialog, or the user id a member row's own form carries.
func (s *Server) memberTarget(r *http.Request) (*store.User, error) {
	if login := strings.TrimSpace(r.FormValue("user")); login != "" {
		return s.db.GetUserByLogin(login)
	}
	uid, err := strconv.ParseInt(r.FormValue("user_id"), 10, 64)
	if err != nil {
		return nil, store.ErrNotFound
	}
	return s.db.GetUser(uid)
}

// handleSetMember adds a user (named in the dialog, or identified by id from a
// member row) to an org, or changes their role. Org admins and instance admins
// may do this.
func (s *Server) handleSetMember(w http.ResponseWriter, r *http.Request) {
	ns, _, ok := s.orgByPath(w, r)
	if !ok {
		return
	}
	// A name to add someone new, an id to change a member's role (the row's own
	// form knows who it is about). Naming a person who does not exist says so:
	// an org admin who has to type the name already knows who they are adding,
	// and a silent success would leave them wondering. What they no longer get
	// is the list — see addMemberDialog.
	target, err := s.memberTarget(r)
	if errors.Is(err, store.ErrNotFound) {
		s.flashRedirect(w, r, "/admin/org/"+ns.Slug, views.T(r.Context(), "flash.nouser"))
		return
	}
	if err != nil {
		uiError(w, r, err)
		return
	}
	role := "user"
	if r.FormValue("role") == "admin" {
		role = "admin"
	}
	if s.db.MemberRole(ns.ID, target.ID) == "" { // adding, not editing
		if err := s.checkMemberQuota(r.Context(), ns); err != nil {
			// The quota message is itself a catalog string (quota.members).
			s.flashRedirect(w, r, "/admin/org/"+ns.Slug, err.Error())
			return
		}
	}
	if err := s.db.SetMember(ns.ID, target.ID, role); err != nil {
		s.flashStore(w, r, "/admin/org/"+ns.Slug, err)
		return
	}
	s.notifyOrgMembership(target, ns.Slug, role)
	s.flashRedirect(w, r, "/admin/org/"+ns.Slug, views.Tf(r.Context(), "flash.memberrole", target.Name, views.T(r.Context(), "role."+role), ns.Slug))
}

func (s *Server) handleRemoveMember(w http.ResponseWriter, r *http.Request) {
	ns, _, ok := s.orgByPath(w, r)
	if !ok {
		return
	}
	uid, _ := strconv.ParseInt(r.PathValue("uid"), 10, 64)
	if err := s.db.RemoveMember(ns.ID, uid); err != nil {
		s.flashStore(w, r, "/admin/org/"+ns.Slug, err)
		return
	}
	s.flashRedirect(w, r, "/admin/org/"+ns.Slug, views.T(r.Context(), "flash.memberremoved"))
}

// mailUser sends a transactional notice to one user (no-op without email or
// SMTP config).
func (s *Server) mailUser(u *store.User, subject, body string) {
	if u != nil {
		mail.Go(s.cfg.SMTP.Mail(), u.Email, subject, body)
	}
}

// mailAdmins notifies every instance admin that has an email address, each
// one in their own language — hence the callback rather than a rendered pair.
func (s *Server) mailAdmins(render func(ctx context.Context) (subject, body string)) {
	users, err := s.db.ListUsers()
	if err != nil {
		return
	}
	for _, u := range users {
		if u.Superadmin() && u.Email != "" {
			subject, body := render(mailCtx(&u))
			mail.Go(s.cfg.SMTP.Mail(), u.Email, subject, body)
		}
	}
}

// mailCtx is the context a transactional email renders in: the recipient's
// language, never the language of whoever triggered the send.
func mailCtx(u *store.User) context.Context {
	if u == nil {
		return context.Background()
	}
	return views.WithLocale(context.Background(), u.Locale)
}

// notifyOrgMembership emails a user about being added to an organization.
func (s *Server) notifyOrgMembership(u *store.User, org, role string) {
	ctx := mailCtx(u)
	s.mailUser(u, views.Tf(ctx, "mail.orgsub", org),
		views.Tf(ctx, "mail.orgbody", views.T(ctx, "role."+role), org, s.cfg.BaseURL))
}

// formPerms reads the token permission checkboxes shared by the create and
// edit forms.
// formPerms reads the permission switches. "manage" is one switch standing for
// the three per-cache management perms the API enforces — before it existed
// they could not be minted from anywhere, so delegating cache administration
// meant handing out an instance-root token.
func formPerms(r *http.Request) []string {
	var perms []string
	for _, p := range []string{"push", "pull", "manage"} {
		if r.FormValue(p) != "" {
			perms = append(perms, p)
		}
	}
	return store.ExpandPerms(perms)
}

func hostOf(baseURL string) string {
	h := baseURL
	if i := strings.Index(h, "://"); i >= 0 {
		h = h[i+3:]
	}
	return strings.TrimSuffix(h, "/")
}
