package server

import (
	"encoding/base64"
	"net/http"
	"strings"
	"time"

	"github.com/stubbedev/xilo/internal/store"
)

// extractToken pulls a secret from the request. Accepts:
//   - Authorization: Bearer <token>   (xilo push client)
//   - Authorization: Basic <user:tok> (Nix netrc pull → token is the password)
func extractToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	if tok, ok := strings.CutPrefix(h, "Bearer "); ok {
		return strings.TrimSpace(tok)
	}
	if enc, ok := strings.CutPrefix(h, "Basic "); ok {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(enc))
		if err != nil {
			return ""
		}
		if _, pass, ok := strings.Cut(string(raw), ":"); ok {
			return pass
		}
	}
	return ""
}

// requirePush enforces a push-scoped token for the cache. With
// security.allow_open_bootstrap, push is open until the first token exists.
func (s *Server) requirePush(w http.ResponseWriter, r *http.Request, c *store.Cache) bool {
	if s.openMode() {
		return true
	}
	if s.db.Authorize(extractToken(r), c.Name, "push", time.Now().Unix()) {
		return true
	}
	s.metrics.authFailures.Add(1)
	unauthorized(w)
	return false
}

// requirePull allows public caches freely; private caches need a pull-scoped
// token.
func (s *Server) requirePull(w http.ResponseWriter, r *http.Request, c *store.Cache) bool {
	if c.Public {
		return true
	}
	if s.db.Authorize(extractToken(r), c.Name, "pull", time.Now().Unix()) {
		return true
	}
	s.metrics.authFailures.Add(1)
	unauthorized(w)
	return false
}

// openMode is true only when open bootstrap is explicitly enabled AND no tokens
// exist yet. Default deployments require a token for every push.
func (s *Server) openMode() bool {
	if !s.cfg.Security.AllowOpenBootstrap {
		return false
	}
	toks, err := s.db.ListTokens()
	return err == nil && len(toks) == 0
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="xilo"`)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}
