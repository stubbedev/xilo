package server

import (
	"fmt"
	"net/http"
	"time"

	"github.com/stubbedev/xilo/internal/server/views"
)

// handleAccountStatus moves one workspace through the lifecycle — read-only,
// closed, or back to normal. A sysadmin's lever, so it is gated on that and
// not on membership: the point is to act on a tenancy you are not part of.
func (s *Server) handleAccountStatus(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	acct, err := s.db.GetAccount(r.PathValue("slug"))
	if err != nil {
		s.notFound(w, r)
		return
	}
	if err := s.db.SetAccountStatus(acct.ID, r.FormValue("status")); err != nil {
		s.flashStore(w, r, "/admin/console", err)
		return
	}
	s.flashRedirect(w, r, "/admin/console", views.Tf(r.Context(), "flash.statusset", acct.Slug))
}

// handleConsole renders the instance overview: what the whole service holds,
// and every organization holding it. This is the superadmin's home — the
// tenant Overview is about one account, and a superadmin is in none.
func (s *Server) handleConsole(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	u := s.currentUser(r)
	d := views.ConsoleData{Nav: s.nav(r, u), Bytes: humanBytes, Flash: s.popFlash(w, r)}
	global, err := s.db.GlobalStatsFor()
	if err != nil {
		uiError(w, r, err)
		return
	}
	d.Global = global
	d.Dedup = "1.00"
	if global.StoredBytes > 0 {
		d.Dedup = fmt.Sprintf("%.2f", float64(global.LogicalBytes)/float64(global.StoredBytes))
	}
	users, err := s.db.ListUsers()
	if err != nil {
		uiError(w, r, err)
		return
	}
	d.Users = int64(len(users))
	accounts, err := s.db.ListAccounts()
	if err != nil {
		uiError(w, r, err)
		return
	}
	month := time.Now().UTC().Format("2006-01")
	for _, acct := range accounts {
		if acct.Kind != "org" {
			continue
		}
		info, err := s.orgInfo(acct, month)
		if err != nil {
			uiError(w, r, err)
			return
		}
		info.Status = s.db.AccountStatus(acct.ID)
		d.Orgs = append(d.Orgs, info)
	}
	caches, err := s.db.ListCaches()
	if err != nil {
		uiError(w, r, err)
		return
	}
	for _, c := range caches {
		st, err := s.db.StatsFor(c.ID)
		if err != nil {
			uiError(w, r, err)
			return
		}
		d.Caches = append(d.Caches, views.CacheUsage{Cache: c, Bytes: st.PhysicalBytes, Logical: st.LogicalBytes, Paths: st.Paths})
	}
	views.Console(d).Render(r.Context(), w)
}
