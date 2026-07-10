package server

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/stubbedev/xilo/internal/server/views"
	"github.com/stubbedev/xilo/internal/store"
)

const sessionCookie = "xilo_session"

// sessionTTL bounds how long an admin session cookie stays valid.
const sessionTTL = 12 * time.Hour

// sessions is an in-memory map of session ID → expiry. Dropped on restart — the
// admin just logs in again. ponytail: no persistent store needed for a
// single-admin dashboard.
type sessions struct {
	mu      sync.Mutex
	ids     map[string]time.Time
	pending map[string]time.Time // password accepted, awaiting 2FA code
}

func newSessions() *sessions {
	return &sessions{ids: map[string]time.Time{}, pending: map[string]time.Time{}}
}

// pendingTTL bounds how long the 2FA step may take after the password step.
const pendingTTL = 3 * time.Minute

// createPending issues a one-shot pre-auth ticket for the 2FA step.
func (s *sessions) createPending() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	id := base64.RawURLEncoding.EncodeToString(b)
	s.mu.Lock()
	s.pending[id] = time.Now().Add(pendingTTL)
	s.mu.Unlock()
	return id, nil
}

// pendingValid reports whether a pre-auth ticket is still live.
func (s *sessions) pendingValid(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.pending[id]
	if !ok || time.Now().After(exp) {
		delete(s.pending, id)
		return false
	}
	return true
}

// consumePending burns a ticket after a successful code check.
func (s *sessions) consumePending(id string) {
	s.mu.Lock()
	delete(s.pending, id)
	s.mu.Unlock()
}

func (s *sessions) create() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err // never emit a low-entropy session id
	}
	id := base64.RawURLEncoding.EncodeToString(b)
	s.mu.Lock()
	s.ids[id] = time.Now().Add(sessionTTL)
	s.mu.Unlock()
	return id, nil
}

func (s *sessions) valid(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.ids[id]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(s.ids, id)
		return false
	}
	return true
}

func (s *sessions) drop(id string) {
	s.mu.Lock()
	delete(s.ids, id)
	s.mu.Unlock()
}

func (s *Server) loggedIn(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	return err == nil && s.sess.valid(c.Value)
}

// requireAdmin gates the mutating admin endpoints: requires a session AND, for
// POSTs, a same-origin request (CSRF defense-in-depth beyond SameSite=Lax).
func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if !s.loggedIn(r) {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return false
	}
	if r.Method == http.MethodPost && !s.sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
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
	mux.HandleFunc("GET /admin/cache/{name}", s.handleCacheDetail)
	mux.HandleFunc("POST /admin/cache/{name}/configure", s.handleConfigureCache)
	mux.HandleFunc("POST /admin/cache/{name}/rotate", s.handleRotateKey)
	mux.HandleFunc("POST /admin/cache/{name}/delete", s.handleDeleteCache)
	mux.HandleFunc("POST /admin/tokens", s.handleCreateToken)
	mux.HandleFunc("POST /admin/tokens/{id}/edit", s.handleEditToken)
	mux.HandleFunc("POST /admin/tokens/{id}/revoke", s.handleRevokeToken)
	mux.HandleFunc("POST /admin/gc", s.handleGC)
	mux.HandleFunc("GET /admin/settings", s.handleSettings)
	mux.HandleFunc("POST /admin/settings/password", s.handleChangePassword)
	mux.HandleFunc("POST /admin/settings/totp/enroll", s.handleTOTPEnroll)
	mux.HandleFunc("POST /admin/settings/totp/enable", s.handleTOTPEnable)
	mux.HandleFunc("POST /admin/settings/totp/disable", s.handleTOTPDisable)
}

// hasPasskeys reports whether any WebAuthn credential is registered.
func (s *Server) hasPasskeys() bool {
	pks, _ := s.db.ListPasskeys()
	return len(pks) > 0
}

// totpEnabled reports whether admin 2FA is on (ignoring errors → treated as off).
func (s *Server) totpEnabled() bool {
	_, enabled, _ := s.db.TOTP()
	return enabled
}

func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	if !s.loggedIn(r) {
		views.Login(!s.db.AdminExists(), s.hasPasskeys(), views.Flash{}).Render(r.Context(), w)
		return
	}
	s.renderDashboard(w, r, views.Flash{})
}

func (s *Server) renderDashboard(w http.ResponseWriter, r *http.Request, flash views.Flash) {
	caches, err := s.db.ListCaches()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	usages := make([]views.CacheUsage, 0, len(caches))
	for _, c := range caches {
		st, err := s.db.CacheStats(c.ID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		usages = append(usages, views.CacheUsage{Cache: c, Bytes: st.PhysicalBytes, Paths: st.Paths})
	}
	global, err := s.db.GlobalStats()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tokens, err := s.db.ListTokens()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	cq := strings.TrimSpace(r.URL.Query().Get("caches[q]"))
	if cq != "" {
		kept := usages[:0]
		for _, u := range usages {
			if fuzzyMatch(u.Cache.Name, cq) {
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
		Global:     global,
		Caches:     pagedCaches,
		Tokens:     pagedTokens,
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
	hash, err := s.db.AdminPasswordHash()
	if errors.Is(err, store.ErrNotFound) {
		views.Login(true, s.hasPasskeys(), views.Flash{}).Render(r.Context(), w)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(r.FormValue("password"))) != nil {
		views.Login(false, s.hasPasskeys(), views.Flash{Msg: "Invalid password"}).Render(r.Context(), w)
		return
	}
	// Password accepted. With 2FA on, the code is a second step gated by a
	// short-lived pre-auth ticket — the password never rides along again.
	if s.totpEnabled() {
		pid, err := s.sess.createPending()
		if err != nil {
			http.Error(w, "session error", http.StatusInternalServerError)
			return
		}
		views.LoginCode(pid, views.Flash{}).Render(r.Context(), w)
		return
	}
	s.grantSession(w, r)
}

// handleLoginCode is step two: a valid pre-auth ticket plus a TOTP code.
func (s *Server) handleLoginCode(w http.ResponseWriter, r *http.Request) {
	pid := r.FormValue("pending")
	if !s.sess.pendingValid(pid) {
		views.Login(false, s.hasPasskeys(), views.Flash{Msg: "That sign-in attempt expired — enter your password again."}).Render(r.Context(), w)
		return
	}
	secret, on, _ := s.db.TOTP()
	if !on {
		// 2FA turned off mid-flight; the password step already passed.
		s.sess.consumePending(pid)
		s.grantSession(w, r)
		return
	}
	if !totpVerify(secret, r.FormValue("code"), time.Now()) {
		views.LoginCode(pid, views.Flash{Msg: "Invalid 2FA code"}).Render(r.Context(), w)
		return
	}
	s.sess.consumePending(pid)
	s.grantSession(w, r)
}

// grantSession issues the session cookie and lands on the dashboard.
func (s *Server) grantSession(w http.ResponseWriter, r *http.Request) {
	id, err := s.sess.create()
	if err != nil {
		http.Error(w, "session error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: id, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.secureCookies(),
	})
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	s.renderSettings(w, r, views.Flash{})
}

// renderSettings gathers the settings page inputs (2FA state, passkeys).
func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request, flash views.Flash) {
	pks, err := s.db.ListPasskeys()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	views.Settings(s.totpEnabled(), pks, flash).Render(r.Context(), w)
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	hash, _ := s.db.AdminPasswordHash()
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(r.FormValue("current"))) != nil {
		s.renderSettings(w, r, views.Flash{Msg: "Current password is incorrect"})
		return
	}
	next := r.FormValue("new")
	if len(next) < 8 {
		s.renderSettings(w, r, views.Flash{Msg: "New password must be at least 8 characters"})
		return
	}
	nh, err := bcrypt.GenerateFromPassword([]byte(next), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.db.SetAdminPassword(string(nh)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderSettings(w, r, views.Flash{Msg: "Password changed."})
}

// handleTOTPEnroll generates a fresh secret, stores it (not yet enabled), and
// shows the QR + a confirm-code form.
func (s *Server) handleTOTPEnroll(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	secret, err := newTOTPSecret()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.db.SetTOTPSecret(secret); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	uri := totpURI(secret, "xilo", hostOf(s.cfg.BaseURL))
	qr, err := totpQRDataURI(uri)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	views.TOTPEnroll(qr, secretB32(secret)).Render(r.Context(), w)
}

func (s *Server) handleTOTPEnable(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	secret, _, _ := s.db.TOTP()
	if len(secret) == 0 || !totpVerify(secret, r.FormValue("code"), time.Now()) {
		uri := totpURI(secret, "xilo", hostOf(s.cfg.BaseURL))
		qr, _ := totpQRDataURI(uri)
		views.TOTPEnrollErr(qr, secretB32(secret), "That code didn't match — try again.").Render(r.Context(), w)
		return
	}
	if err := s.db.SetTOTPEnabled(true); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderSettings(w, r, views.Flash{Msg: "Two-factor authentication enabled."})
}

func (s *Server) handleTOTPDisable(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if err := s.db.SetTOTPEnabled(false); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderSettings(w, r, views.Flash{Msg: "Two-factor authentication disabled."})
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
	if err != nil || n < 0 {
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
	if err != nil || n < 0 {
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
	if !s.requireAdmin(w, r) {
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	priority := clampPriority(r.FormValue("priority"), 40)
	public := r.FormValue("private") == ""
	if _, err := s.db.CreateCache(name, public, priority); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// notFound renders the styled 404 page (browser routes only).
func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNotFound)
	views.NotFound(s.loggedIn(r)).Render(r.Context(), w)
}

func (s *Server) handleCacheDetail(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	c, err := s.db.GetCache(r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
		Cache:     *c,
		Stats:     st,
		Dedup:     dedup,
		BaseURL:   s.cfg.BaseURL,
		Host:      hostOf(s.cfg.BaseURL),
		Bytes:     humanBytes,
		Paths:     paths,
		PathQuery: q,
		PathTotal: total,
		PathPager: makePager("/admin/cache/"+c.Name, r.URL.Query(), "page", page, pages),
		PathSort: views.SortCtx{
			Path: "/admin/cache/" + c.Name, Query: r.URL.Query(),
			SortParam: "sort", DirParam: "dir", PageParam: "page[number]",
			Key: skey, Dir: sdir,
		},
	}).Render(r.Context(), w)
}

// cacheByName resolves the {name} path value or writes 404/500.
func (s *Server) cacheByName(w http.ResponseWriter, r *http.Request) (*store.Cache, bool) {
	c, err := s.db.GetCache(r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return nil, false
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return nil, false
	}
	return c, true
}

func (s *Server) handleConfigureCache(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	c, ok := s.cacheByName(w, r)
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
	http.Redirect(w, r, "/admin/cache/"+c.Name, http.StatusSeeOther)
}

func (s *Server) handleRotateKey(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	c, ok := s.cacheByName(w, r)
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
	if !s.requireAdmin(w, r) {
		return
	}
	c, ok := s.cacheByName(w, r)
	if !ok {
		return
	}
	if err := s.db.DeleteCache(c.ID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	var caches []string
	if c := r.FormValue("cache"); c != "" && c != "*" {
		caches = []string{c}
	}
	var perms []string
	if r.FormValue("push") != "" {
		perms = append(perms, "push")
	}
	if r.FormValue("pull") != "" {
		perms = append(perms, "pull")
	}
	if len(perms) == 0 {
		perms = []string{"pull"}
	}
	var expires int64
	if secs, ok := formSeconds(r, "ttl"); ok && secs > 0 {
		expires = time.Now().Unix() + secs
	}
	secret, t, err := s.db.CreateToken(name, caches, perms, expires)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.renderDashboard(w, r, views.Flash{
		Msg:  fmt.Sprintf("Token %q created — copy it now, it will not be shown again:", t.Name),
		Code: secret,
	})
}

// handleEditToken rewrites a token's metadata. Expiry: the permanent switch
// clears it, a TTL value re-sets it counting from now, and leaving the TTL
// empty keeps the stored expiry.
func (s *Server) handleEditToken(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	t, err := s.db.GetToken(id)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		name = t.Name
	}
	var caches []string
	if c := r.FormValue("cache"); c != "" && c != "*" {
		caches = []string{c}
	}
	var perms []string
	if r.FormValue("push") != "" {
		perms = append(perms, "push")
	}
	if r.FormValue("pull") != "" {
		perms = append(perms, "pull")
	}
	expires := t.Expires
	if r.FormValue("permanent") != "" {
		expires = 0
	} else if secs, ok := formSeconds(r, "ttl"); ok && secs > 0 {
		expires = time.Now().Unix() + secs
	}
	if err := s.db.UpdateToken(id, name, caches, perms, expires); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.db.RevokeToken(id); err != nil {
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

func hostOf(baseURL string) string {
	h := baseURL
	if i := strings.Index(h, "://"); i >= 0 {
		h = h[i+3:]
	}
	return strings.TrimSuffix(h, "/")
}
