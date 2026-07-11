package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/crypto/bcrypt"

	"github.com/stubbedev/xilo/internal/mail"
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

// activeContext resolves the account-context cookie for u: the slug of a
// context they belong to (or any account for instance admins); "" = all.
func (s *Server) activeContext(r *http.Request, u *store.User) string {
	c, err := r.Cookie(ctxCookie)
	if err != nil || c.Value == "" || u == nil {
		return ""
	}
	acc, err := s.db.GetAccount(c.Value)
	if err != nil {
		return ""
	}
	if u.Role == "admin" || s.db.MemberRole(acc.ID, u.ID) != "" {
		return acc.Slug
	}
	return ""
}

// nav builds the header state for a signed-in user (zero Nav when signed out).
func (s *Server) nav(r *http.Request, u *store.User) views.Nav {
	if u == nil {
		return views.Nav{}
	}
	n := views.Nav{LoggedIn: true, UserName: u.Name, IsAdmin: u.Role == "admin", Active: s.activeContext(r, u)}
	var err error
	if u.Role == "admin" {
		n.Contexts, err = s.db.ListAccounts()
	} else {
		n.Contexts, err = s.db.UserAccounts(u.ID)
	}
	if err != nil {
		n.Contexts = nil
	}
	return n
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
		if err != nil || (u.Role != "admin" && s.db.MemberRole(acc.ID, u.ID) == "") {
			val = ""
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name: ctxCookie, Value: val, Path: "/",
		MaxAge:   int((30 * 24 * time.Hour).Seconds()),
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.secureCookies(),
	})
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// requireUser gates per-account endpoints: any signed-in user, plus a
// same-origin check on POSTs (CSRF defense-in-depth beyond SameSite=Lax).
func (s *Server) requireUser(w http.ResponseWriter, r *http.Request) *store.User {
	u := s.currentUser(r)
	if u == nil {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return nil
	}
	if r.Method == http.MethodPost && !s.sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
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
	if u.Role != "admin" {
		http.Error(w, "admin role required", http.StatusForbidden)
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
	mux.HandleFunc("POST /admin/caches", s.handleCreateCache)
	mux.HandleFunc("GET /admin/cache/{account}/{name}", s.handleCacheDetail)
	mux.HandleFunc("POST /admin/cache/{account}/{name}/configure", s.handleConfigureCache)
	mux.HandleFunc("POST /admin/cache/{account}/{name}/rotate", s.handleRotateKey)
	mux.HandleFunc("POST /admin/cache/{account}/{name}/delete", s.handleDeleteCache)
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
	mux.HandleFunc("POST /admin/context", s.handleContext)
	mux.HandleFunc("GET /admin/org/{slug}", s.handleOrgPage)
	mux.HandleFunc("GET /admin/status", s.handleStatus)
	mux.HandleFunc("GET /admin/status/data", s.handleStatusData)
	mux.HandleFunc("POST /admin/account/password", s.handleChangePassword)
	mux.HandleFunc("POST /admin/account/password/check", s.handlePasswordCheck)
	mux.HandleFunc("POST /admin/account/totp/enroll", s.handleTOTPEnroll)
	mux.HandleFunc("POST /admin/account/totp/enable", s.handleTOTPEnable)
	mux.HandleFunc("POST /admin/account/totp/disable", s.handleTOTPDisable)
	mux.HandleFunc("POST /admin/users", s.handleCreateUser)
	mux.HandleFunc("POST /admin/users/{id}/role", s.handleUserRole)
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
	s.renderDashboard(w, r, views.Flash{})
}

// canManage reports whether u may mutate resources in a namespace: instance
// admins always, otherwise namespace owners.
func (s *Server) canManage(u *store.User, nsID int64) bool {
	return u != nil && (u.Role == "admin" || s.db.MemberRole(nsID, u.ID) == "admin")
}

// visibleCaches lists the caches u may see: all for admins, their namespaces'
// for everyone else.
func (s *Server) visibleCaches(u *store.User) ([]store.Cache, error) {
	if u.Role == "admin" {
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
// namespaces they own.
func (s *Server) visibleTokens(u *store.User) ([]store.Token, error) {
	if u.Role == "admin" {
		return s.db.ListTokens()
	}
	nss, err := s.db.UserAccounts(u.ID)
	if err != nil {
		return nil, err
	}
	var out []store.Token
	for _, ns := range nss {
		if s.db.MemberRole(ns.ID, u.ID) != "admin" {
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

// ownedNamespaces returns the namespaces u may create caches/tokens in.
func (s *Server) ownedNamespaces(u *store.User) ([]store.Account, error) {
	if u.Role == "admin" {
		return s.db.ListAccounts()
	}
	nss, err := s.db.UserAccounts(u.ID)
	if err != nil {
		return nil, err
	}
	var out []store.Account
	for _, ns := range nss {
		if s.db.MemberRole(ns.ID, u.ID) == "admin" {
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
		st, err := s.db.CacheStats(c.ID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		usages = append(usages, views.CacheUsage{Cache: c, Bytes: st.PhysicalBytes, Logical: st.LogicalBytes, Paths: st.Paths})
	}
	global, err := s.db.GlobalStats()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if u.Role != "admin" || s.activeContext(r, u) != "" {
		// Tenants — and any scoped context — see that footprint, not the
		// instance's.
		global = store.Global{Caches: int64(len(usages))}
		for _, us := range usages {
			global.Paths += us.Paths
			global.StoredBytes += us.Bytes
			global.LogicalBytes += us.Logical
		}
	}
	tokens, err := s.visibleTokens(u)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	owned, err := s.ownedNamespaces(u)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
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
		Tokens:     pagedTokens,
		Accounts:   owned,
		Storages:   s.storageNames(),
		IsAdmin:    u.Role == "admin",
		Flash:      flash,
		ServerCap:  s.cfg.Limits.TotalBytes(),
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
	if !s.logins.allow(clientIP(r)) {
		s.metrics.authFailures.Add(1)
		http.Error(w, "too many attempts — wait a moment", http.StatusTooManyRequests)
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
		views.Login(false, s.hasPasskeys(), s.registrationOpen(), views.Flash{Msg: "Invalid username or password"}).Render(r.Context(), w)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PassHash), []byte(r.FormValue("password"))) != nil {
		views.Login(false, s.hasPasskeys(), s.registrationOpen(), views.Flash{Msg: "Invalid username or password"}).Render(r.Context(), w)
		return
	}
	if u.Status == "pending" {
		views.Login(false, s.hasPasskeys(), s.registrationOpen(), views.Flash{Msg: "Your account is awaiting approval."}).Render(r.Context(), w)
		return
	}
	// Password accepted. With 2FA on, the code is a second step gated by a
	// short-lived pre-auth ticket — the password never rides along again.
	if u.TOTPEnabled {
		pid, err := s.sess.createPending(u.ID)
		if err != nil {
			http.Error(w, "session error", http.StatusInternalServerError)
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
	if !s.logins.allow(clientIP(r)) {
		s.metrics.authFailures.Add(1)
		http.Error(w, "too many attempts — wait a moment", http.StatusTooManyRequests)
		return
	}
	pid := r.FormValue("pending")
	uid, ok := s.sess.pendingUser(pid)
	if !ok {
		views.Login(false, s.hasPasskeys(), s.registrationOpen(), views.Flash{Msg: "That sign-in attempt expired — enter your password again."}).Render(r.Context(), w)
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
		views.LoginCode(pid, views.Flash{Msg: "Invalid 2FA code"}).Render(r.Context(), w)
		return
	}
	s.sess.consumePending(pid)
	s.grantSession(w, r, uid)
}

// grantSession issues the session cookie and lands on the dashboard.
func (s *Server) grantSession(w http.ResponseWriter, r *http.Request, userID int64) {
	id, err := s.sess.create(userID)
	if err != nil {
		http.Error(w, "session error", http.StatusInternalServerError)
		return
	}
	s.setSessionCookie(w, id)
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
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
		s.renderAccount(w, r, u, views.Flash{})
	}
}

// renderAccount is the personal page: identity + credentials.
func (s *Server) renderAccount(w http.ResponseWriter, r *http.Request, u *store.User, flash views.Flash) {
	pks, err := s.db.ListUserPasskeys(u.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	views.Account(views.AccountData{
		Nav: s.nav(r, u), User: u, TOTPEnabled: u.TOTPEnabled, Passkeys: pks, Flash: flash,
	}).Render(r.Context(), w)
}

// handleAccountEmail updates the sign-in alias.
func (s *Server) handleAccountEmail(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	email := strings.TrimSpace(r.FormValue("email"))
	if err := s.db.SetUserEmail(u.ID, email); err != nil {
		s.renderAccount(w, r, u, views.Flash{Msg: "Could not save email: " + err.Error()})
		return
	}
	u.Email = email
	msg := "Email saved."
	if email == "" {
		msg = "Email cleared."
	}
	s.renderAccount(w, r, u, views.Flash{Msg: msg})
}

func (s *Server) handleInstancePage(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	s.renderInstance(w, r, views.Flash{})
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
	d := views.InstanceData{
		Nav: s.nav(r, u), Flash: flash,
		MultiTenant: s.cfg.MultiTenant,
	}
	var err error
	if d.Users, err = s.db.ListUsers(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if s.cfg.MultiTenant {
		if d.Plans, err = s.db.ListPlans(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		d.AllowRegs = s.db.SettingBool("allow_registrations", false)
		d.RequireOK = s.db.SettingBool("require_approval", true)
	}
	accounts, err := s.db.ListAccounts()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	month := time.Now().UTC().Format("2006-01")
	for _, acct := range accounts {
		if acct.Kind != "org" {
			continue // personal accounts are not organizations
		}
		info, err := s.orgInfo(acct, month)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		d.Orgs = append(d.Orgs, info)
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
	s.renderOrg(w, r, u, r.PathValue("slug"), views.Flash{})
}

func (s *Server) renderOrg(w http.ResponseWriter, r *http.Request, u *store.User, slug string, flash views.Flash) {
	acct, err := s.db.GetAccount(slug)
	if errors.Is(err, store.ErrNotFound) || (err == nil && acct.Kind != "org") {
		s.notFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if u.Role != "admin" && s.db.MemberRole(acct.ID, u.ID) == "" {
		s.notFound(w, r) // no existence oracle
		return
	}
	info, err := s.orgInfo(*acct, time.Now().UTC().Format("2006-01"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	caches, err := s.db.ListAccountCaches(acct.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	d := views.OrgPageData{
		Nav: s.nav(r, u), Info: info, CanManage: s.canManage(u, acct.ID),
		Bytes: humanBytes, Flash: flash,
	}
	for _, c := range caches {
		st, err := s.db.CacheStats(c.ID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		d.Caches = append(d.Caches, views.CacheUsage{Cache: c, Bytes: st.PhysicalBytes, Paths: st.Paths})
	}
	// Picker candidates: users not yet members.
	if d.CanManage {
		users, err := s.db.ListUsers()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		member := map[int64]bool{}
		for _, m := range info.Members {
			member[m.UserID] = true
		}
		for _, cand := range users {
			if !member[cand.ID] && cand.Status == "active" {
				d.AllUsers = append(d.AllUsers, cand)
			}
		}
	}
	views.Org(d).Render(r.Context(), w)
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PassHash), []byte(r.FormValue("current"))) != nil {
		s.renderAccount(w, r, u, views.Flash{Msg: "Current password is incorrect"})
		return
	}
	next := r.FormValue("new")
	switch pwState(next, r.FormValue("confirm")) {
	case "short", "":
		s.renderAccount(w, r, u, views.Flash{Msg: "New password must be at least 8 characters"})
		return
	case "mismatch":
		s.renderAccount(w, r, u, views.Flash{Msg: "Passwords do not match"})
		return
	}
	nh, err := bcrypt.GenerateFromPassword([]byte(next), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.db.SetUserPassword(u.ID, string(nh)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderAccount(w, r, u, views.Flash{Msg: "Password changed."})
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
		http.Error(w, "unauthorized", http.StatusUnauthorized)
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.db.SetUserTOTPSecret(u.ID, secret); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	uri := totpURI(secret, "xilo", u.Name+"@"+hostOf(s.cfg.BaseURL))
	qr, err := totpQRDataURI(uri)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	views.TOTPEnroll(s.nav(r, u), qr, secretB32(secret)).Render(r.Context(), w)
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
		views.TOTPEnrollErr(s.nav(r, u), qr, secretB32(secret), "That code didn't match — try again.").Render(r.Context(), w)
		return
	}
	if err := s.db.SetUserTOTPEnabled(u.ID, true); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	u.TOTPEnabled = true
	s.renderAccount(w, r, u, views.Flash{Msg: "Two-factor authentication enabled."})
}

func (s *Server) handleTOTPDisable(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	if err := s.db.SetUserTOTPEnabled(u.ID, false); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	u.TOTPEnabled = false
	s.renderAccount(w, r, u, views.Flash{Msg: "Two-factor authentication disabled."})
}

// secureCookies marks session cookies Secure when the public base URL is HTTPS
// (the standard TLS-terminating-proxy deployment).
func (s *Server) secureCookies() bool {
	return strings.HasPrefix(s.cfg.BaseURL, "https://")
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.sess.drop(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// fuzzyMatch reports whether every whitespace-separated term of q matches s
// as a case-insensitive subsequence — the in-memory twin of the SQL search.
func fuzzyMatch(s, q string) bool {
	s = strings.ToLower(s)
	for _, term := range strings.Fields(strings.ToLower(q)) {
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
	for _, a := range allowed {
		if k == a {
			key = k
			break
		}
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
	less := func(a, b store.Token) bool {
		switch key {
		case "perms":
			return strings.Join(a.Perms, ",") < strings.Join(b.Perms, ",")
		case "scope":
			return strings.Join(a.Caches, ",") < strings.Join(b.Caches, ",")
		case "expires":
			// never (0) sorts after every real date
			ae, be := a.Expires, b.Expires
			if ae == 0 {
				ae = math.MaxInt64
			}
			if be == 0 {
				be = math.MaxInt64
			}
			return ae < be
		case "status":
			return views.TokenStatus(a) < views.TokenStatus(b)
		default: // name
			return strings.ToLower(a.Name) < strings.ToLower(b.Name)
		}
	}
	sort.SliceStable(tokens, func(i, j int) bool {
		if dir == "asc" {
			return less(tokens[i], tokens[j])
		}
		return less(tokens[j], tokens[i])
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
		for k, vs := range params {
			v[k] = vs
		}
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
		ns = "default"
	}
	if strings.Contains(name, "/") || strings.Contains(ns, "/") {
		http.Error(w, "names cannot contain '/'", http.StatusBadRequest)
		return
	}
	// Instance admins may create caches anywhere (minting the account on the
	// fly); everyone else only inside accounts they administer, within plan
	// quota.
	if u.Role != "admin" {
		acc, err := s.db.GetAccount(ns)
		if err != nil || s.db.MemberRole(acc.ID, u.ID) != "admin" {
			http.Error(w, "you do not administer this account", http.StatusForbidden)
			return
		}
		if err := s.checkCacheQuota(acc); err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
	}
	priority := clampPriority(r.FormValue("priority"), 40)
	public := r.FormValue("private") == ""
	stName, err := s.resolveStorage(r.FormValue("storage"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	c, err := s.db.CreateCache(ns, name, public, priority)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.assignStorage(c, stName); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
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
	c, err := s.db.GetCache(r.PathValue("account"), r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Members see their namespaces' caches; outsiders get the same 404 as a
	// nonexistent cache (no existence oracle).
	if u.Role != "admin" && s.db.MemberRole(c.AccountID, u.ID) == "" {
		s.notFound(w, r)
		return
	}
	st, err := s.db.CacheStats(c.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	pages := int((total + int64(perPage) - 1) / int64(perPage))
	if pages < 1 {
		pages = 1
	}
	if page > pages && total > 0 {
		// Past the end (e.g. stale link): show the last page instead of nothing.
		page = pages
		paths, total, err = s.db.SearchPaths(c.ID, q, perPage, (page-1)*perPage, skey, sdir)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	views.CacheView(views.CacheData{
		Nav:       s.nav(r, u),
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

// manageCache resolves {ns}/{name} and enforces mutate rights (instance admin
// or namespace owner). Outsiders get 404, not 403 — no existence oracle.
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
	// Empty keeps the current value; an explicit 0 clears the setting.
	retention := c.Retention
	if secs, ok := formSeconds(r, "retention"); ok {
		retention = secs
	}
	maxBytes := c.MaxBytes
	if b, ok := formBytes(r, "max"); ok {
		maxBytes = b
	}
	if err := s.db.UpdateCache(c.ID, public, priority, retention, maxBytes); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderDashboard(w, r, views.Flash{
		Msg:  "Signing key rotated. Update trusted-public-keys everywhere — the old key no longer verifies:",
		Code: nc.PubKey,
	})
}

func (s *Server) handleDeleteCache(w http.ResponseWriter, r *http.Request) {
	c, ok := s.manageCache(w, r)
	if !ok {
		return
	}
	if err := s.db.DeleteCache(c.ID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// tokenScope validates the form's namespace + cache-scope pair for the acting
// user. Returns the owning namespace id (0 = instance token) and the scope
// patterns to store.
func (s *Server) tokenScope(u *store.User, r *http.Request) (nsID int64, nsName string, caches []string, err error) {
	nsName = strings.TrimSpace(r.FormValue("namespace"))
	if nsName == "" {
		if u.Role != "admin" {
			return 0, "", nil, errors.New("only admins can mint instance-wide tokens")
		}
	} else {
		ns, gerr := s.db.GetAccount(nsName)
		if gerr != nil {
			return 0, "", nil, errors.New("no such namespace")
		}
		if !s.canManage(u, ns.ID) {
			return 0, "", nil, errors.New("you do not own this namespace")
		}
		nsID = ns.ID
	}
	if c := r.FormValue("cache"); c != "" && c != "*" {
		if nsID != 0 {
			// Namespace tokens store bare cache names within their namespace.
			bare, ok := strings.CutPrefix(c, nsName+"/")
			if !ok {
				return 0, "", nil, errors.New("scope must be a cache in " + nsName)
			}
			caches = []string{bare}
		} else {
			caches = []string{c}
		}
	}
	return nsID, nsName, caches, nil
}

func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	nsID, _, caches, err := s.tokenScope(u, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	perms := formPerms(r)
	if len(perms) == 0 {
		perms = []string{"pull"}
	}
	if slices.Contains(perms, "admin") && (nsID != 0 || u.Role != "admin") {
		http.Error(w, "admin tokens are instance-wide and admin-only", http.StatusForbidden)
		return
	}
	var expires int64
	if secs, ok := formSeconds(r, "ttl"); ok && secs > 0 {
		expires = time.Now().Unix() + secs
	}
	secret, t, err := s.db.CreateToken(nsID, name, caches, perms, expires)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.renderDashboard(w, r, views.Flash{
		Msg:  fmt.Sprintf("Token %q created — copy it now, it will not be shown again:", t.Name),
		Code: secret,
	})
}

// manageToken resolves {id} and enforces mutate rights: admins for any token,
// owners for their namespace's tokens.
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return nil, nil, false
	}
	if t.AccountID == 0 && u.Role != "admin" {
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
	t, u, ok := s.manageToken(w, r)
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
				http.Error(w, "scope must be a cache in "+t.Account, http.StatusBadRequest)
				return
			}
			caches = []string{bare}
		} else {
			caches = []string{c}
		}
	}
	perms := formPerms(r)
	if slices.Contains(perms, "admin") && (t.AccountID != 0 || u.Role != "admin") {
		http.Error(w, "admin tokens are instance-wide and admin-only", http.StatusForbidden)
		return
	}
	expires := t.Expires
	if r.FormValue("permanent") != "" {
		expires = 0
	} else if secs, ok := formSeconds(r, "ttl"); ok && secs > 0 {
		expires = time.Now().Unix() + secs
	}
	if err := s.db.UpdateToken(t.ID, name, caches, perms, expires); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	t, _, ok := s.manageToken(w, r)
	if !ok {
		return
	}
	if err := s.db.RevokeToken(t.ID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) handleGC(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	deleted, freed, err := s.runGC(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderDashboard(w, r, views.Flash{Msg: fmt.Sprintf("GC done: removed %d chunks, freed %s", deleted, humanBytes(freed))})
}

// ---- user management (admin role only) ----

// instanceFlash re-renders the general settings page with a message.
func (s *Server) instanceFlash(w http.ResponseWriter, r *http.Request, msg string) {
	s.renderInstance(w, r, views.Flash{Msg: msg})
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	name := strings.TrimSpace(r.FormValue("username"))
	pw := r.FormValue("password")
	role := formRole(r)
	if name == "" {
		s.instanceFlash(w, r, "Username is required")
		return
	}
	if len(pw) < 8 {
		s.instanceFlash(w, r, "Password must be at least 8 characters")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := s.db.CreateUser(name, strings.TrimSpace(r.FormValue("email")), string(hash), role); err != nil {
		s.instanceFlash(w, r, "Could not create user: "+err.Error())
		return
	}
	s.instanceFlash(w, r, fmt.Sprintf("User %q created.", name))
}

// formRole whitelists the role field.
func formRole(r *http.Request) string {
	if r.FormValue("role") == "admin" {
		return "admin"
	}
	return "member"
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return nil, false
	}
	return u, true
}

// lastAdmin reports whether u is the only admin left — the one account that
// can never be demoted or deleted.
func (s *Server) lastAdmin(u *store.User) bool {
	if u.Role != "admin" {
		return false
	}
	n, err := s.db.CountAdmins()
	return err != nil || n <= 1
}

func (s *Server) handleUserRole(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	u, ok := s.userByPath(w, r)
	if !ok {
		return
	}
	role := formRole(r)
	if role != "admin" && s.lastAdmin(u) {
		s.instanceFlash(w, r, "Cannot demote the last admin.")
		return
	}
	if err := s.db.SetUserRole(u.ID, role); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.instanceFlash(w, r, fmt.Sprintf("%s is now a %s.", u.Name, role))
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
		s.instanceFlash(w, r, "Password must be at least 8 characters")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.db.SetUserPassword(u.ID, string(hash)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.instanceFlash(w, r, fmt.Sprintf("Password reset for %s.", u.Name))
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
		s.instanceFlash(w, r, "You cannot delete your own account.")
		return
	}
	if s.lastAdmin(u) {
		s.instanceFlash(w, r, "Cannot delete the last admin.")
		return
	}
	if err := s.db.DeleteUser(u.ID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.instanceFlash(w, r, fmt.Sprintf("User %s deleted.", u.Name))
}

// ---- namespace management ----

func (s *Server) handleCreateOrg(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" || strings.ContainsAny(name, "/ ") {
		s.instanceFlash(w, r, "Namespace names cannot be empty or contain '/' or spaces.")
		return
	}
	if _, err := s.db.EnsureAccount(name, "org"); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.instanceFlash(w, r, fmt.Sprintf("Namespace %q ready.", name))
}

// orgByPath resolves the {slug} path value to an ORG the acting user may
// manage (org admin or instance admin). 404s hide existence.
func (s *Server) orgByPath(w http.ResponseWriter, r *http.Request) (*store.Account, *store.User, bool) {
	u := s.requireUser(w, r)
	if u == nil {
		return nil, nil, false
	}
	ns, err := s.db.GetAccount(r.PathValue("slug"))
	if errors.Is(err, store.ErrNotFound) || (err == nil && ns.Kind != "org") {
		s.notFound(w, r)
		return nil, nil, false
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return nil, nil, false
	}
	if !s.canManage(u, ns.ID) {
		s.notFound(w, r)
		return nil, nil, false
	}
	return ns, u, true
}

func (s *Server) handleDeleteOrg(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	ns, _, ok := s.orgByPath(w, r)
	if !ok {
		return
	}
	if err := s.db.DeleteAccount(ns.ID); err != nil {
		s.instanceFlash(w, r, "Could not delete: "+err.Error())
		return
	}
	s.instanceFlash(w, r, fmt.Sprintf("Organization %s deleted.", ns.Slug))
}

// handleSetMember adds a user (picked by id) to an org or changes their role.
// Org admins and instance admins may do this.
func (s *Server) handleSetMember(w http.ResponseWriter, r *http.Request) {
	ns, actor, ok := s.orgByPath(w, r)
	if !ok {
		return
	}
	uid, _ := strconv.ParseInt(r.FormValue("user_id"), 10, 64)
	target, err := s.db.GetUser(uid)
	if errors.Is(err, store.ErrNotFound) {
		s.renderOrg(w, r, actor, ns.Slug, views.Flash{Msg: "No such user."})
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	role := "member"
	if r.FormValue("role") == "admin" {
		role = "admin"
	}
	if s.db.MemberRole(ns.ID, target.ID) == "" { // adding, not editing
		if err := s.checkMemberQuota(ns); err != nil {
			s.renderOrg(w, r, actor, ns.Slug, views.Flash{Msg: err.Error()})
			return
		}
	}
	if err := s.db.SetMember(ns.ID, target.ID, role); err != nil {
		s.renderOrg(w, r, actor, ns.Slug, views.Flash{Msg: err.Error()})
		return
	}
	s.notifyOrgMembership(target, ns.Slug, role)
	s.renderOrg(w, r, actor, ns.Slug, views.Flash{Msg: fmt.Sprintf("%s is now a %s of %s.", target.Name, role, ns.Slug)})
}

func (s *Server) handleRemoveMember(w http.ResponseWriter, r *http.Request) {
	ns, actor, ok := s.orgByPath(w, r)
	if !ok {
		return
	}
	uid, _ := strconv.ParseInt(r.PathValue("uid"), 10, 64)
	if err := s.db.RemoveMember(ns.ID, uid); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderOrg(w, r, actor, ns.Slug, views.Flash{Msg: "Member removed."})
}

// mailUser sends a transactional notice to one user (no-op without email or
// SMTP config).
func (s *Server) mailUser(u *store.User, subject, body string) {
	if u != nil {
		mail.Go(s.cfg.SMTP.Mail(), u.Email, subject, body)
	}
}

// mailAdmins notifies every instance admin that has an email address.
func (s *Server) mailAdmins(subject, body string) {
	users, err := s.db.ListUsers()
	if err != nil {
		return
	}
	for _, u := range users {
		if u.Role == "admin" && u.Email != "" {
			mail.Go(s.cfg.SMTP.Mail(), u.Email, subject, body)
		}
	}
}

// notifyOrgMembership emails a user about being added to an organization.
func (s *Server) notifyOrgMembership(u *store.User, org, role string) {
	s.mailUser(u, "You were added to "+org,
		"You are now a "+role+" of the organization "+org+" on "+s.cfg.BaseURL+".")
}

// formPerms reads the token permission checkboxes shared by the create and
// edit forms.
func formPerms(r *http.Request) []string {
	var perms []string
	for _, p := range []string{"push", "pull", "admin"} {
		if r.FormValue(p) != "" {
			perms = append(perms, p)
		}
	}
	return perms
}

func hostOf(baseURL string) string {
	h := baseURL
	if i := strings.Index(h, "://"); i >= 0 {
		h = h[i+3:]
	}
	return strings.TrimSuffix(h, "/")
}
