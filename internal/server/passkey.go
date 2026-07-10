package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/stubbedev/xilo/internal/store"
)

// adminUser adapts the single admin account to webauthn.User.
type adminUser struct {
	creds []webauthn.Credential
	ids   []int64 // store row id per credential, same order
}

func (adminUser) WebAuthnID() []byte                           { return []byte("xilo-admin") }
func (adminUser) WebAuthnName() string                         { return "admin" }
func (adminUser) WebAuthnDisplayName() string                  { return "xilo admin" }
func (u adminUser) WebAuthnCredentials() []webauthn.Credential { return u.creds }

// loadAdminUser builds the webauthn user from stored credentials.
func (s *Server) loadAdminUser() (adminUser, error) {
	rows, err := s.db.ListPasskeys()
	if err != nil {
		return adminUser{}, err
	}
	u := adminUser{}
	for _, p := range rows {
		var c webauthn.Credential
		if err := json.Unmarshal(p.Credential, &c); err != nil {
			continue // skip an unreadable row rather than lock the admin out
		}
		u.creds = append(u.creds, c)
		u.ids = append(u.ids, p.ID)
	}
	return u, nil
}

// webAuthn lazily builds the relying party from the configured base URL.
func (s *Server) webAuthn() (*webauthn.WebAuthn, error) {
	s.wanOnce.Do(func() {
		rpID := hostOf(s.cfg.BaseURL)
		if i := strings.LastIndex(rpID, ":"); i >= 0 {
			rpID = rpID[:i] // RP ID is a domain — no port
		}
		s.wan, s.wanErr = webauthn.New(&webauthn.Config{
			RPDisplayName: "xilo",
			RPID:          rpID,
			RPOrigins:     []string{strings.TrimSuffix(s.cfg.BaseURL, "/")},
		})
	})
	return s.wan, s.wanErr
}

// ceremonies holds the single in-flight registration/login challenge each.
// Single-admin: last begin wins, entries expire after two minutes.
type ceremonies struct {
	mu       sync.Mutex
	reg      *webauthn.SessionData
	regExp   time.Time
	login    *webauthn.SessionData
	loginExp time.Time
}

func (c *ceremonies) putReg(sd *webauthn.SessionData) {
	c.mu.Lock()
	c.reg, c.regExp = sd, time.Now().Add(2*time.Minute)
	c.mu.Unlock()
}

func (c *ceremonies) takeReg() *webauthn.SessionData {
	c.mu.Lock()
	defer c.mu.Unlock()
	sd := c.reg
	c.reg = nil
	if sd == nil || time.Now().After(c.regExp) {
		return nil
	}
	return sd
}

func (c *ceremonies) putLogin(sd *webauthn.SessionData) {
	c.mu.Lock()
	c.login, c.loginExp = sd, time.Now().Add(2*time.Minute)
	c.mu.Unlock()
}

func (c *ceremonies) takeLogin() *webauthn.SessionData {
	c.mu.Lock()
	defer c.mu.Unlock()
	sd := c.login
	c.login = nil
	if sd == nil || time.Now().After(c.loginExp) {
		return nil
	}
	return sd
}

func (s *Server) registerPasskeyRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /admin/passkeys/register/begin", s.handlePasskeyRegisterBegin)
	mux.HandleFunc("POST /admin/passkeys/register/finish", s.handlePasskeyRegisterFinish)
	mux.HandleFunc("POST /admin/passkeys/{id}/delete", s.handlePasskeyDelete)
	mux.HandleFunc("POST /admin/login/passkey/begin", s.handlePasskeyLoginBegin)
	mux.HandleFunc("POST /admin/login/passkey/finish", s.handlePasskeyLoginFinish)
}

// passkeyName is the default label for a new credential.
func (s *Server) passkeyName() string {
	host := hostOf(s.cfg.BaseURL)
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	return host + " - xilo"
}

func jsonOut(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (s *Server) handlePasskeyRegisterBegin(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	wan, err := s.webAuthn()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	user, err := s.loadAdminUser()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	opts, sd, err := wan.BeginRegistration(user)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.ceremony.putReg(sd)
	jsonOut(w, opts)
}

func (s *Server) handlePasskeyRegisterFinish(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	wan, err := s.webAuthn()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sd := s.ceremony.takeReg()
	if sd == nil {
		http.Error(w, "registration expired — try again", http.StatusBadRequest)
		return
	}
	user, err := s.loadAdminUser()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	cred, err := wan.FinishRegistration(user, *sd, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Autoname server-side: "<hostname> - xilo". Client-supplied names went
	// stale-tab wrong once already.
	name := s.passkeyName()
	blob, err := json.Marshal(cred)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.db.AddPasskey(name, blob); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOut(w, map[string]bool{"ok": true})
}

func (s *Server) handlePasskeyDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.db.DeletePasskey(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin/settings", http.StatusSeeOther)
}

func (s *Server) handlePasskeyLoginBegin(w http.ResponseWriter, r *http.Request) {
	wan, err := s.webAuthn()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	user, err := s.loadAdminUser()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(user.creds) == 0 {
		http.Error(w, "no passkeys registered", http.StatusBadRequest)
		return
	}
	opts, sd, err := wan.BeginLogin(user)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.ceremony.putLogin(sd)
	jsonOut(w, opts)
}

// handlePasskeyLoginFinish verifies the assertion; a passkey is
// user-verified multi-factor on its own, so it bypasses password and TOTP.
func (s *Server) handlePasskeyLoginFinish(w http.ResponseWriter, r *http.Request) {
	wan, err := s.webAuthn()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sd := s.ceremony.takeLogin()
	if sd == nil {
		http.Error(w, "sign-in expired — try again", http.StatusBadRequest)
		return
	}
	user, err := s.loadAdminUser()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	cred, err := wan.FinishLogin(user, *sd, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	// Persist the updated sign counter / clone-detection state.
	for i, c := range user.creds {
		if string(c.ID) == string(cred.ID) {
			if blob, err := json.Marshal(cred); err == nil {
				_ = s.db.UpdatePasskeyCredential(user.ids[i], blob)
			}
			break
		}
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
	jsonOut(w, map[string]bool{"ok": true})
}

var _ = store.Passkey{} // keep the import while the adapter stays thin
