package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/stubbedev/xilo/internal/store"
)

// passkeyUser adapts one account (or, for login, the union of all accounts)
// to webauthn.User.
type passkeyUser struct {
	id      []byte
	name    string
	creds   []webauthn.Credential
	rowIDs  []int64 // store row id per credential, same order
	userIDs []int64 // owning user per credential, same order
}

func (u passkeyUser) WebAuthnID() []byte                         { return u.id }
func (u passkeyUser) WebAuthnName() string                       { return u.name }
func (u passkeyUser) WebAuthnDisplayName() string                { return u.name }
func (u passkeyUser) WebAuthnCredentials() []webauthn.Credential { return u.creds }

// fillCreds decodes stored credential rows into the adapter.
func (u *passkeyUser) fillCreds(rows []store.Passkey) {
	for _, p := range rows {
		var c webauthn.Credential
		if err := json.Unmarshal(p.Credential, &c); err != nil {
			continue // skip an unreadable row rather than lock the user out
		}
		u.creds = append(u.creds, c)
		u.rowIDs = append(u.rowIDs, p.ID)
		u.userIDs = append(u.userIDs, p.UserID)
	}
}

// loadUserPasskeys builds the webauthn user for one account (registration).
func (s *Server) loadUserPasskeys(u *store.User) (passkeyUser, error) {
	rows, err := s.db.ListUserPasskeys(u.ID)
	if err != nil {
		return passkeyUser{}, err
	}
	pu := passkeyUser{id: []byte(fmt.Sprintf("xilo-user-%d", u.ID)), name: u.Name}
	pu.fillCreds(rows)
	return pu, nil
}

// loadAllPasskeys builds a synthetic user holding every account's credentials.
// Passkey sign-in has no username field, so the assertion is verified against
// the union and the matching credential identifies the owner.
// ponytail: fine at self-hosted user counts; per-user allow-lists if ever needed.
func (s *Server) loadAllPasskeys() (passkeyUser, error) {
	rows, err := s.db.ListPasskeys()
	if err != nil {
		return passkeyUser{}, err
	}
	pu := passkeyUser{id: []byte("xilo-admin"), name: "xilo"}
	pu.fillCreds(rows)
	return pu, nil
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
// Last begin wins; entries expire after two minutes.
type ceremonies struct {
	mu       sync.Mutex
	reg      *webauthn.SessionData
	regUser  int64 // account the registration belongs to
	regExp   time.Time
	login    *webauthn.SessionData
	loginExp time.Time
}

func (c *ceremonies) putReg(sd *webauthn.SessionData, userID int64) {
	c.mu.Lock()
	c.reg, c.regUser, c.regExp = sd, userID, time.Now().Add(2*time.Minute)
	c.mu.Unlock()
}

func (c *ceremonies) takeReg() (*webauthn.SessionData, int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	sd, uid := c.reg, c.regUser
	c.reg = nil
	if sd == nil || time.Now().After(c.regExp) {
		return nil, 0
	}
	return sd, uid
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
func (s *Server) passkeyName(u *store.User) string {
	host := hostOf(s.cfg.BaseURL)
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	return u.Name + "@" + host
}

func jsonOut(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (s *Server) handlePasskeyRegisterBegin(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	wan, err := s.webAuthn()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	user, err := s.loadUserPasskeys(u)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	opts, sd, err := wan.BeginRegistration(user)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.ceremony.putReg(sd, u.ID)
	jsonOut(w, opts)
}

func (s *Server) handlePasskeyRegisterFinish(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	wan, err := s.webAuthn()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sd, regUser := s.ceremony.takeReg()
	if sd == nil || regUser != u.ID {
		http.Error(w, "registration expired — try again", http.StatusBadRequest)
		return
	}
	user, err := s.loadUserPasskeys(u)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	cred, err := wan.FinishRegistration(user, *sd, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Autoname server-side: "<user>@<hostname>". Client-supplied names went
	// stale-tab wrong once already.
	name := s.passkeyName(u)
	blob, err := json.Marshal(cred)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.db.AddPasskey(u.ID, name, blob); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOut(w, map[string]bool{"ok": true})
}

func (s *Server) handlePasskeyDelete(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.db.DeletePasskey(u.ID, id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin/account", http.StatusSeeOther)
}

func (s *Server) handlePasskeyLoginBegin(w http.ResponseWriter, r *http.Request) {
	wan, err := s.webAuthn()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	user, err := s.loadAllPasskeys()
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
//
// The asserted credential ID identifies which account signs in. The adapter's
// WebAuthnID (and the session's) are aligned to the authenticator-reported
// user handle before validation, because resident keys echo the handle they
// were registered under — "xilo-admin" for pre-users credentials,
// "xilo-user-N" since — and go-webauthn requires all three to agree. Identity
// still rests on the signature over the credential we looked up, not the
// handle.
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
	parsed, err := protocol.ParseCredentialRequestResponseBody(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	all, err := s.loadAllPasskeys()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var owner passkeyUser
	var ownerID, rowID int64
	for i, c := range all.creds {
		if string(c.ID) == string(parsed.RawID) {
			ownerID, rowID = all.userIDs[i], all.rowIDs[i]
			break
		}
	}
	if ownerID == 0 {
		http.Error(w, "unknown credential", http.StatusUnauthorized)
		return
	}
	u, err := s.db.GetUser(ownerID)
	if err != nil {
		http.Error(w, "credential has no owner", http.StatusUnauthorized)
		return
	}
	if owner, err = s.loadUserPasskeys(u); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if h := parsed.Response.UserHandle; len(h) > 0 {
		owner.id = h
	}
	sd.UserID = owner.id
	cred, err := wan.ValidateLogin(owner, *sd, parsed)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	// Persist the updated sign counter / clone-detection state.
	if blob, err := json.Marshal(cred); err == nil {
		_ = s.db.UpdatePasskeyCredential(rowID, blob)
	}
	id, err := s.sess.create(ownerID)
	if err != nil {
		http.Error(w, "session error", http.StatusInternalServerError)
		return
	}
	s.setSessionCookie(w, id)
	jsonOut(w, map[string]bool{"ok": true})
}
