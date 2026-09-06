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

	"github.com/stubbedev/xilo/internal/config"
	"github.com/stubbedev/xilo/internal/server/views"
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
	pu := passkeyUser{id: fmt.Appendf(nil, "xilo-user-%d", u.ID), name: u.Name}
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

// newWebAuthn builds the memoized relying-party constructor stored in
// Server.webAuthn: the base URL is fixed for the process, so the RP is built
// once, on first passkey use.
func newWebAuthn(cfg *config.Config) func() (*webauthn.WebAuthn, error) {
	return sync.OnceValues(func() (*webauthn.WebAuthn, error) {
		rpID := hostOf(cfg.BaseURL)
		if i := strings.LastIndex(rpID, ":"); i >= 0 {
			rpID = rpID[:i] // RP ID is a domain — no port
		}
		return webauthn.New(&webauthn.Config{
			RPDisplayName: "xilo",
			RPID:          rpID,
			RPOrigins:     []string{strings.TrimSuffix(cfg.BaseURL, "/")},
			// A passkey replaces username+password, so it must itself be strong:
			// require user verification (PIN/biometric) at registration so the
			// credential is possession + inherence, not possession alone.
			AuthenticatorSelection: protocol.AuthenticatorSelection{
				UserVerification: protocol.VerificationRequired,
			},
		})
	})
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
		uiError(w, r, err)
		return
	}
	user, err := s.loadUserPasskeys(u)
	if err != nil {
		uiError(w, r, err)
		return
	}
	opts, sd, err := wan.BeginRegistration(user)
	if err != nil {
		uiError(w, r, err)
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
		uiError(w, r, err)
		return
	}
	sd, regUser := s.ceremony.takeReg()
	if sd == nil || regUser != u.ID {
		uiFail(w, r, http.StatusBadRequest, views.T(r.Context(), "err.pkexpired"), nil)
		return
	}
	user, err := s.loadUserPasskeys(u)
	if err != nil {
		uiError(w, r, err)
		return
	}
	cred, err := wan.FinishRegistration(user, *sd, r)
	if err != nil {
		uiFail(w, r, http.StatusBadRequest, views.T(r.Context(), "err.pkregister"), err)
		return
	}
	if !cred.Flags.UserVerified {
		uiFail(w, r, http.StatusBadRequest, views.T(r.Context(), "err.pkverify"), nil)
		return
	}
	// Autoname server-side: "<user>@<hostname>". Client-supplied names went
	// stale-tab wrong once already.
	name := s.passkeyName(u)
	blob, err := json.Marshal(cred)
	if err != nil {
		uiError(w, r, err)
		return
	}
	if err := s.db.AddPasskey(u.ID, name, blob); err != nil {
		uiError(w, r, err)
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
		uiError(w, r, err)
		return
	}
	s.accountFlash(w, r, views.T(r.Context(), "flash.pkremoved"))
}

func (s *Server) handlePasskeyLoginBegin(w http.ResponseWriter, r *http.Request) {
	wan, err := s.webAuthn()
	if err != nil {
		uiError(w, r, err)
		return
	}
	user, err := s.loadAllPasskeys()
	if err != nil {
		uiError(w, r, err)
		return
	}
	if len(user.creds) == 0 {
		uiFail(w, r, http.StatusBadRequest, views.T(r.Context(), "err.pknone"), nil)
		return
	}
	opts, sd, err := wan.BeginLogin(user, webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		uiError(w, r, err)
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
		uiError(w, r, err)
		return
	}
	sd := s.ceremony.takeLogin()
	if sd == nil {
		uiFail(w, r, http.StatusBadRequest, views.T(r.Context(), "err.pkloginexp"), nil)
		return
	}
	parsed, err := protocol.ParseCredentialRequestResponseBody(r.Body)
	if err != nil {
		uiFail(w, r, http.StatusBadRequest, views.T(r.Context(), "err.pksignin"), err)
		return
	}
	all, err := s.loadAllPasskeys()
	if err != nil {
		uiError(w, r, err)
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
		uiFail(w, r, http.StatusUnauthorized, views.T(r.Context(), "err.pkunknown"), nil)
		return
	}
	u, err := s.db.GetUser(ownerID)
	if err != nil {
		uiFail(w, r, http.StatusUnauthorized, views.T(r.Context(), "err.pkowner"), nil)
		return
	}
	if owner, err = s.loadUserPasskeys(u); err != nil {
		uiError(w, r, err)
		return
	}
	if h := parsed.Response.UserHandle; len(h) > 0 {
		owner.id = h
	}
	sd.UserID = owner.id
	cred, err := wan.ValidateLogin(owner, *sd, parsed)
	if err != nil {
		uiFail(w, r, http.StatusUnauthorized, views.T(r.Context(), "err.pksignin"), err)
		return
	}
	// The passkey stands in for password + TOTP, so possession alone is not
	// enough — demand the user-verified flag the assertion carries.
	if !cred.Flags.UserVerified {
		uiFail(w, r, http.StatusUnauthorized, views.T(r.Context(), "err.pkverify"), nil)
		return
	}
	// Persist the updated sign counter / clone-detection state.
	if blob, err := json.Marshal(cred); err == nil {
		_ = s.db.UpdatePasskeyCredential(rowID, blob)
	}
	id, err := s.sess.create(ownerID)
	if err != nil {
		uiFail(w, r, http.StatusInternalServerError, views.T(r.Context(), "err.session"), err)
		return
	}
	s.setSessionCookie(w, id)
	jsonOut(w, map[string]bool{"ok": true})
}
