package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/stubbedev/xilo/internal/api"
	"github.com/stubbedev/xilo/internal/store"
)

// registerAdminAPI wires the JSON management API (/api/v1/…). Every endpoint
// requires a token carrying the "admin" perm — the remote twin of the local
// `xilo cache`/`xilo token` commands and the dashboard.
func (s *Server) registerAdminAPI(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/caches", s.apiAdmin(s.apiListCaches))
	mux.HandleFunc("POST /api/v1/caches", s.apiCreateCache)
	mux.HandleFunc("GET /api/v1/caches/{ns}/{name}", s.apiNS("configure-cache", s.apiGetCache))
	mux.HandleFunc("PATCH /api/v1/caches/{ns}/{name}", s.apiNS("configure-cache", s.apiConfigureCache))
	mux.HandleFunc("POST /api/v1/caches/{ns}/{name}/rotate", s.apiNS("configure-cache", s.apiRotateKey))
	mux.HandleFunc("DELETE /api/v1/caches/{ns}/{name}", s.apiNS("destroy-cache", s.apiDeleteCache))
	mux.HandleFunc("GET /api/v1/namespaces", s.apiAdmin(s.apiListNamespaces))
	mux.HandleFunc("POST /api/v1/namespaces", s.apiAdmin(s.apiCreateNamespace))
	mux.HandleFunc("DELETE /api/v1/namespaces/{ns}", s.apiAdmin(s.apiDeleteNamespace))
	mux.HandleFunc("GET /api/v1/tokens", s.apiAdmin(s.apiListTokens))
	mux.HandleFunc("POST /api/v1/tokens", s.apiAdmin(s.apiCreateToken))
	mux.HandleFunc("POST /api/v1/tokens/{id}/revoke", s.apiAdmin(s.apiRevokeToken))
	mux.HandleFunc("POST /api/v1/gc", s.apiAdmin(s.apiGC))
}

// apiAdmin gates a handler behind an instance-admin token.
func (s *Server) apiAdmin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.db.AuthorizeAdmin(extractToken(r), time.Now().Unix()) {
			s.metrics.authFailures.Add(1)
			apiError(w, http.StatusUnauthorized, "admin token required")
			return
		}
		h(w, r)
	}
}

// apiNS gates a per-cache handler behind the given management perm scoped to
// the {ns}/{name} path (admin tokens always pass).
func (s *Server) apiNS(perm string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.db.AuthorizeNS(extractToken(r), r.PathValue("ns"), r.PathValue("name"), perm, time.Now().Unix()) {
			s.metrics.authFailures.Add(1)
			apiError(w, http.StatusUnauthorized, perm+" token required")
			return
		}
		h(w, r)
	}
}

// apiError writes a JSON error body so CLI clients can show a clean message.
func apiError(w http.ResponseWriter, code int, msg string) {
	jsonStatus(w, code, map[string]string{"error": msg})
}

// jsonStatus is jsonOut with an explicit status code (headers must precede it).
func jsonStatus(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func apiCache(c *store.Cache) api.Cache {
	return api.Cache{
		Namespace: c.NS, Name: c.Name, Storage: c.Storage, Public: c.Public, Priority: c.Priority,
		Retention: c.Retention, MaxBytes: c.MaxBytes,
		PubKey: c.PubKey, Created: c.Created,
	}
}

func apiToken(t store.Token) api.Token {
	return api.Token{
		ID: t.ID, Namespace: t.Namespace, Name: t.Name, Caches: t.Caches, Perms: t.Perms,
		Revoked: t.Revoked, Expires: t.Expires, Created: t.Created,
	}
}

// apiCacheByName resolves {ns}/{name}, writing a JSON 404 when unknown.
func (s *Server) apiCacheByName(w http.ResponseWriter, r *http.Request) (*store.Cache, bool) {
	c, err := s.db.GetCache(r.PathValue("ns"), r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		apiError(w, http.StatusNotFound, "no such cache")
		return nil, false
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return nil, false
	}
	return c, true
}

func (s *Server) apiListCaches(w http.ResponseWriter, r *http.Request) {
	caches, err := s.db.ListCaches()
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]api.Cache, 0, len(caches))
	for i := range caches {
		out = append(out, apiCache(&caches[i]))
	}
	jsonOut(w, out)
}

func (s *Server) apiCreateCache(w http.ResponseWriter, r *http.Request) {
	var req api.CreateCacheReq
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Namespace = strings.TrimSpace(req.Namespace)
	if req.Namespace == "" {
		req.Namespace = "default"
	}
	if strings.Contains(req.Name, "/") || strings.Contains(req.Namespace, "/") {
		apiError(w, http.StatusBadRequest, "names cannot contain '/'")
		return
	}
	// create-cache tokens work within their scope; admin tokens anywhere.
	if !s.db.AuthorizeNS(extractToken(r), req.Namespace, req.Name, "create-cache", time.Now().Unix()) {
		s.metrics.authFailures.Add(1)
		apiError(w, http.StatusUnauthorized, "create-cache token required")
		return
	}
	if req.Priority == 0 {
		req.Priority = 40
	}
	stName, err := s.resolveStorage(req.Storage)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	c, err := s.db.CreateCache(req.Namespace, req.Name, req.Public, req.Priority)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.assignStorage(c, stName); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonStatus(w, http.StatusCreated, apiCache(c))
}

func (s *Server) apiListNamespaces(w http.ResponseWriter, r *http.Request) {
	nss, err := s.db.ListNamespaces()
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]api.NamespaceResp, 0, len(nss))
	for _, ns := range nss {
		out = append(out, api.NamespaceResp{Name: ns.Name, Created: ns.Created})
	}
	jsonOut(w, out)
}

func (s *Server) apiCreateNamespace(w http.ResponseWriter, r *http.Request) {
	var req api.CreateNamespaceReq
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || strings.ContainsAny(req.Name, "/ ") {
		apiError(w, http.StatusBadRequest, "invalid namespace name")
		return
	}
	ns, err := s.db.EnsureNamespace(req.Name)
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonStatus(w, http.StatusCreated, api.NamespaceResp{Name: ns.Name, Created: ns.Created})
}

func (s *Server) apiDeleteNamespace(w http.ResponseWriter, r *http.Request) {
	ns, err := s.db.GetNamespace(r.PathValue("ns"))
	if errors.Is(err, store.ErrNotFound) {
		apiError(w, http.StatusNotFound, "no such namespace")
		return
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.db.DeleteNamespace(ns.ID); err != nil {
		apiError(w, http.StatusConflict, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) apiGetCache(w http.ResponseWriter, r *http.Request) {
	c, ok := s.apiCacheByName(w, r)
	if !ok {
		return
	}
	st, err := s.db.CacheStats(c.ID)
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOut(w, api.CacheDetail{
		Cache: apiCache(c),
		Paths: st.Paths, Chunks: st.Chunks,
		LogicalBytes: st.LogicalBytes, PhysicalBytes: st.PhysicalBytes,
	})
}

func (s *Server) apiConfigureCache(w http.ResponseWriter, r *http.Request) {
	c, ok := s.apiCacheByName(w, r)
	if !ok {
		return
	}
	var req api.ConfigureCacheReq
	if !decodeJSON(w, r, &req) {
		return
	}
	public, priority, retention, maxBytes := c.Public, c.Priority, c.Retention, c.MaxBytes
	if req.Public != nil {
		public = *req.Public
	}
	if req.Priority != nil {
		priority = *req.Priority
	}
	if req.Retention != nil {
		retention = *req.Retention
	}
	if req.MaxBytes != nil {
		maxBytes = *req.MaxBytes
	}
	if err := s.db.UpdateCache(c.ID, public, priority, retention, maxBytes); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	c, err := s.db.GetCacheByID(c.ID)
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOut(w, apiCache(c))
}

func (s *Server) apiRotateKey(w http.ResponseWriter, r *http.Request) {
	c, ok := s.apiCacheByName(w, r)
	if !ok {
		return
	}
	nc, err := s.db.RotateKey(c.ID, c.Name)
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOut(w, apiCache(nc))
}

func (s *Server) apiDeleteCache(w http.ResponseWriter, r *http.Request) {
	c, ok := s.apiCacheByName(w, r)
	if !ok {
		return
	}
	if err := s.db.DeleteCache(c.ID); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) apiListTokens(w http.ResponseWriter, r *http.Request) {
	toks, err := s.db.ListTokens()
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]api.Token, 0, len(toks))
	for _, t := range toks {
		out = append(out, apiToken(t))
	}
	jsonOut(w, out)
}

func (s *Server) apiCreateToken(w http.ResponseWriter, r *http.Request) {
	var req api.CreateTokenReq
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if len(req.Perms) == 0 {
		apiError(w, http.StatusBadRequest, "perms required")
		return
	}
	var nsID int64
	if req.Namespace != "" {
		ns, err := s.db.GetNamespace(req.Namespace)
		if errors.Is(err, store.ErrNotFound) {
			apiError(w, http.StatusBadRequest, "no such namespace")
			return
		}
		if err != nil {
			apiError(w, http.StatusInternalServerError, err.Error())
			return
		}
		nsID = ns.ID
	}
	secret, t, err := s.db.CreateToken(nsID, req.Name, req.Caches, req.Perms, req.Expires)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	jsonStatus(w, http.StatusCreated, api.CreateTokenResp{Secret: secret, Token: apiToken(*t)})
}

func (s *Server) apiRevokeToken(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		apiError(w, http.StatusBadRequest, "id must be a number")
		return
	}
	if err := s.db.RevokeToken(id); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) apiGC(w http.ResponseWriter, r *http.Request) {
	var req api.GCReq
	if r.ContentLength != 0 && !decodeJSON(w, r, &req) {
		return
	}
	var evicted int64
	if req.EvictOlderThan > 0 {
		cutoff := time.Now().Unix() - req.EvictOlderThan
		n, err := s.db.EvictPathsOlderThan(cutoff)
		if err != nil {
			apiError(w, http.StatusInternalServerError, err.Error())
			return
		}
		evicted = n
	}
	deleted, freed, err := s.runGC(r.Context())
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOut(w, api.GCResp{Evicted: evicted, Deleted: deleted, FreedBytes: freed})
}
