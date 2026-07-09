package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

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
	mu  sync.Mutex
	ids map[string]time.Time
}

func newSessions() *sessions { return &sessions{ids: map[string]time.Time{}} }

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
	mux.HandleFunc("POST /admin/logout", s.handleLogout)
	mux.HandleFunc("POST /admin/caches", s.handleCreateCache)
	mux.HandleFunc("GET /admin/cache/{name}", s.handleCacheDetail)
	mux.HandleFunc("POST /admin/cache/{name}/configure", s.handleConfigureCache)
	mux.HandleFunc("POST /admin/cache/{name}/rotate", s.handleRotateKey)
	mux.HandleFunc("POST /admin/cache/{name}/delete", s.handleDeleteCache)
	mux.HandleFunc("POST /admin/tokens", s.handleCreateToken)
	mux.HandleFunc("POST /admin/tokens/{id}/revoke", s.handleRevokeToken)
	mux.HandleFunc("POST /admin/gc", s.handleGC)
}

func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	if !s.loggedIn(r) {
		views.Login(s.cfg.Admin.Password == "", views.Flash{}).Render(r.Context(), w)
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
	tokens, err := s.db.ListTokens()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	views.Dashboard(caches, tokens, flash).Render(r.Context(), w)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	pass := r.FormValue("password")
	want := s.cfg.Admin.Password
	if want == "" || subtle.ConstantTimeCompare([]byte(pass), []byte(want)) != 1 {
		views.Login(want == "", views.Flash{Msg: "Invalid password"}).Render(r.Context(), w)
		return
	}
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

func (s *Server) handleCreateCache(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	priority, _ := strconv.Atoi(r.FormValue("priority"))
	if priority == 0 {
		priority = 40
	}
	public := r.FormValue("private") == ""
	if _, err := s.db.CreateCache(name, public, priority); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) handleCacheDetail(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	c, err := s.db.GetCache(r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
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
	views.CacheView(views.CacheData{
		Cache:   *c,
		Stats:   st,
		Dedup:   dedup,
		BaseURL: s.cfg.BaseURL,
		Host:    hostOf(s.cfg.BaseURL),
		Bytes:   humanBytes,
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
	priority, _ := strconv.Atoi(r.FormValue("priority"))
	if priority == 0 {
		priority = c.Priority
	}
	public := r.FormValue("private") == ""
	var retention int64
	if rt := strings.TrimSpace(r.FormValue("retention")); rt != "" {
		if d, err := time.ParseDuration(rt); err == nil {
			retention = int64(d.Seconds())
		}
	}
	if err := s.db.UpdateCache(c.ID, public, priority, retention); err != nil {
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
	if ttl := strings.TrimSpace(r.FormValue("ttl")); ttl != "" {
		d, err := time.ParseDuration(ttl)
		if err != nil {
			http.Error(w, "bad ttl: "+err.Error(), http.StatusBadRequest)
			return
		}
		expires = time.Now().Add(d).Unix()
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
