package server

import (
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/stubbedev/xilo/internal/server/views"
)

// The account wallet: the session ids this browser has authenticated, so one
// person can stay signed in as several users and move between them without
// logging out of any. Holding the id is the proof of a past login — exactly
// what the active session cookie is — so the wallet is one cookie carrying N
// session cookies and wears the same flags. Switching only ever activates an
// id already in it: nothing here grants a session, it only re-selects one.
const walletCookie = "xilo_accounts"

// walletSep joins ids in the cookie. Session ids are base64url (A–Z a–z 0–9
// - _), so a dot can never occur inside one.
const walletSep = "."

// maxWallet bounds how many accounts a browser keeps signed in at once, which
// bounds the cookie and the per-render lookups behind the account menu.
const maxWallet = 5

// walletIDs is the raw session ids the browser is holding, oldest first.
func walletIDs(r *http.Request) []string {
	c, err := r.Cookie(walletCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	var out []string
	for id := range strings.SplitSeq(c.Value, walletSep) {
		if id != "" {
			out = append(out, id)
		}
	}
	return out
}

// setWallet writes the wallet cookie (or clears it when empty).
func (s *Server) setWallet(w http.ResponseWriter, ids []string) {
	if len(ids) == 0 {
		http.SetCookie(w, &http.Cookie{Name: walletCookie, Value: "", Path: "/", MaxAge: -1})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: walletCookie, Value: strings.Join(ids, walletSep), Path: "/",
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.secureCookies(),
	})
}

// addToWallet records a freshly issued session. Signing in again as someone
// already held replaces their entry — two sessions for one person is not two
// accounts — and the oldest is evicted (and dropped server-side, so nothing
// dangles) once the wallet is full.
func (s *Server) addToWallet(w http.ResponseWriter, r *http.Request, id string, userID int64) {
	held := walletIDs(r)
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" && !slices.Contains(held, c.Value) {
		held = append([]string{c.Value}, held...)
	}
	var kept []string
	for _, old := range held {
		if old == id {
			continue
		}
		uid, ok := s.sess.user(old)
		if !ok {
			continue // expired or revoked elsewhere
		}
		if uid == userID {
			s.sess.drop(old)
			continue
		}
		kept = append(kept, old)
	}
	for len(kept) >= maxWallet {
		s.sess.drop(kept[0])
		kept = kept[1:]
	}
	s.setWallet(w, append(kept, id))
}

// replaceInWallet swaps a re-issued session id in for the one it replaces —
// after a password change or a 2FA drop, where the old session is deliberately
// invalidated and this browser is handed a new one.
func (s *Server) replaceInWallet(w http.ResponseWriter, r *http.Request, oldID, newID string) {
	ids := walletIDs(r)
	for i, id := range ids {
		if id == oldID {
			ids[i] = newID
			s.setWallet(w, ids)
			return
		}
	}
	s.addToWallet(w, r, newID, 0)
}

// walletAccounts resolves the wallet to the accounts behind it for the account
// menu, marking the one in use. Ids that no longer resolve are simply left
// out; the cookie is tidied the next time it is written.
func (s *Server) walletAccounts(r *http.Request, active string) []views.NavSession {
	var out []views.NavSession
	for _, id := range walletIDs(r) {
		uid, ok := s.sess.user(id)
		if !ok {
			continue
		}
		u, err := s.db.GetUser(uid)
		if err != nil {
			continue
		}
		out = append(out, views.NavSession{UserID: u.ID, Name: u.Name, Active: id == active})
	}
	return out
}

// sessionIDFor finds the wallet entry belonging to a user. The menu posts a
// user id, never a session id: the ids are the secret, and a page that renders
// them puts them in caches, history and screenshots.
func (s *Server) sessionIDFor(r *http.Request, userID int64) (string, bool) {
	for _, id := range walletIDs(r) {
		if uid, ok := s.sess.user(id); ok && uid == userID {
			return id, true
		}
	}
	return "", false
}

// handleSwitchAccount activates another account this browser is already signed
// in as. It never authenticates: an id absent from the wallet is simply not
// found, so a forged post can only name a user this browser already proved.
func (s *Server) handleSwitchAccount(w http.ResponseWriter, r *http.Request) {
	if s.requireUser(w, r) == nil {
		return
	}
	uid, _ := strconv.ParseInt(r.FormValue("user"), 10, 64)
	id, ok := s.sessionIDFor(r, uid)
	if !ok {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	s.setSessionCookie(w, id)
	// The viewing context belongs to the browser, not the person; activeContext
	// re-checks membership for whoever is now signed in and falls back to their
	// own default, so a stale slug cannot leak another account's name.
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// handleSignInPage renders the sign-in form to someone already signed in, so
// they can add a second account without dropping the first.
func (s *Server) handleSignInPage(w http.ResponseWriter, r *http.Request) {
	views.Login(!s.db.UsersExist(), s.hasPasskeys(), s.registrationOpen(), views.Flash{}).Render(r.Context(), w)
}
