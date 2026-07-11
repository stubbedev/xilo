package server

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"github.com/stubbedev/xilo/internal/server/views"
	"github.com/stubbedev/xilo/internal/store"
)

// Multi-tenant surface: self-registration with plan selection, instance
// policy toggles, plan CRUD, org creation, and pending-user approval. All of
// it exists only when multi_tenant is on — single-tenant deployments keep
// zero signup surface.

func (s *Server) registerTenancy(mux *http.ServeMux) {
	mux.HandleFunc("GET /register", s.handleRegisterForm)
	mux.HandleFunc("POST /register", s.handleRegister)
	mux.HandleFunc("POST /admin/settings/instance", s.handleInstanceSettings)
	mux.HandleFunc("POST /admin/plans", s.handleCreatePlan)
	mux.HandleFunc("POST /admin/plans/{id}/edit", s.handleEditPlan)
	mux.HandleFunc("POST /admin/plans/{id}/delete", s.handleDeletePlan)
	mux.HandleFunc("POST /admin/users/{id}/approve", s.handleApproveUser)
	mux.HandleFunc("POST /admin/neworg", s.handleUserCreateOrg)
}

// registrationOpen reports whether self-registration is currently possible.
func (s *Server) registrationOpen() bool {
	return s.cfg.MultiTenant && s.db.SettingBool("allow_registrations", false)
}

// requireApproval reports whether new registrations start pending.
func (s *Server) requireApproval() bool {
	return s.db.SettingBool("require_approval", true)
}

func (s *Server) handleRegisterForm(w http.ResponseWriter, r *http.Request) {
	if !s.registrationOpen() {
		s.notFoundNegotiated(w, r)
		return
	}
	plans, err := s.db.PublicPlans()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	views.Register(plans, views.Flash{}).Render(r.Context(), w)
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if !s.registrationOpen() {
		s.notFoundNegotiated(w, r)
		return
	}
	// Registrations share the login limiter bucket: same bcrypt cost, same
	// abuse profile.
	if !s.logins.allow(clientIP(r)) {
		http.Error(w, "too many attempts — wait a moment", http.StatusTooManyRequests)
		return
	}
	plans, err := s.db.PublicPlans()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	fail := func(msg string) {
		views.Register(plans, views.Flash{Msg: msg}).Render(r.Context(), w)
	}

	username := strings.TrimSpace(r.FormValue("username"))
	email := strings.TrimSpace(r.FormValue("email"))
	password := r.FormValue("password")
	if !store.ValidSlug(username) {
		fail("Invalid username: lowercase letters, digits, - and _.")
		return
	}
	if len(password) < 8 {
		fail("Password must be at least 8 characters.")
		return
	}

	var plan *store.Plan
	if pid, _ := strconv.ParseInt(r.FormValue("plan"), 10, 64); pid != 0 {
		p, err := s.db.GetPlan(pid)
		if err != nil || !p.Public {
			fail("Pick one of the offered plans.")
			return
		}
		plan = p
	} else if len(plans) > 0 {
		fail("Pick one of the offered plans.")
		return
	}

	orgName := strings.TrimSpace(r.FormValue("org"))
	if orgName != "" {
		if plan == nil || !plan.OrgsAllowed {
			fail("The selected plan does not include organizations.")
			return
		}
		if !store.ValidSlug(orgName) {
			fail("Invalid organization name: lowercase letters, digits, - and _.")
			return
		}
		if _, err := s.db.GetAccount(orgName); err == nil {
			fail("That organization name is taken.")
			return
		}
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var u *store.User
	if s.requireApproval() {
		u, err = s.db.CreatePendingUser(username, email, string(hash))
	} else {
		u, err = s.db.CreateUser(username, email, string(hash), "member")
	}
	if err != nil {
		fail("Could not register: " + err.Error())
		return
	}
	// Plan lands on the personal account (and the org, if any).
	if personal, err := s.db.GetAccount(username); err == nil && plan != nil {
		_ = s.db.SetAccountPlan(personal.ID, plan.ID)
	}
	if orgName != "" {
		if org, err := s.db.EnsureAccount(orgName, "org"); err == nil {
			if plan != nil {
				_ = s.db.SetAccountPlan(org.ID, plan.ID)
			}
			_ = s.db.SetMember(org.ID, u.ID, "admin")
		}
	}

	if u.Status == "pending" {
		views.Login(false, s.hasPasskeys(), s.registrationOpen(),
			views.Flash{Msg: "Registered — an administrator has to approve your account before you can sign in."}).Render(r.Context(), w)
		return
	}
	s.grantSession(w, r, u.ID)
}

// handleInstanceSettings updates the DB-backed policy toggles (super admin).
func (s *Server) handleInstanceSettings(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	for _, key := range []string{"allow_registrations", "require_approval"} {
		v := "0"
		if r.FormValue(key) != "" {
			v = "1"
		}
		if err := s.db.SetSetting(key, v); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	s.settingsFlash(w, r, "Instance settings saved.")
}

// planFromForm reads the shared plan form fields.
func planFromForm(r *http.Request) store.Plan {
	geti := func(name string) int64 {
		n, _ := strconv.ParseInt(strings.TrimSpace(r.FormValue(name)), 10, 64)
		if n < 0 {
			n = 0
		}
		return n
	}
	storageBytes, _ := formBytes(r, "plan_storage")
	retention, _ := formSeconds(r, "plan_retention")
	return store.Plan{
		Name:         strings.TrimSpace(r.FormValue("name")),
		MaxCaches:    geti("max_caches"),
		MaxMembers:   geti("max_members"),
		MaxStorage:   storageBytes,
		MaxRetention: retention,
		OrgsAllowed:  r.FormValue("orgs_allowed") != "",
		Public:       r.FormValue("public") != "",
	}
}

func (s *Server) handleCreatePlan(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	p := planFromForm(r)
	if p.Name == "" {
		s.settingsFlash(w, r, "Plan name is required.")
		return
	}
	if _, err := s.db.CreatePlan(&p); err != nil {
		s.settingsFlash(w, r, "Could not create plan: "+err.Error())
		return
	}
	s.settingsFlash(w, r, fmt.Sprintf("Plan %q created.", p.Name))
}

func (s *Server) handleEditPlan(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	cur, err := s.db.GetPlan(id)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	p := planFromForm(r)
	p.ID = cur.ID
	if p.Name == "" {
		p.Name = cur.Name
	}
	if err := s.db.UpdatePlan(&p); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.settingsFlash(w, r, fmt.Sprintf("Plan %q updated.", p.Name))
}

func (s *Server) handleDeletePlan(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.db.DeletePlan(id); err != nil {
		s.settingsFlash(w, r, "Could not delete plan: "+err.Error())
		return
	}
	s.settingsFlash(w, r, "Plan deleted.")
}

func (s *Server) handleApproveUser(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	u, ok := s.userByPath(w, r)
	if !ok {
		return
	}
	if err := s.db.SetUserStatus(u.ID, "active"); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.settingsFlash(w, r, fmt.Sprintf("%s approved.", u.Name))
}

// userCanCreateOrg: instance admins always; otherwise the personal account's
// plan must include organizations (multi-tenant mode only).
func (s *Server) userCanCreateOrg(u *store.User) bool {
	if u == nil {
		return false
	}
	if u.Role == "admin" {
		return true
	}
	if !s.cfg.MultiTenant {
		return false
	}
	personal, err := s.db.GetAccount(u.Name)
	if err != nil {
		return false
	}
	plan, err := s.db.AccountPlan(personal)
	if err != nil || plan == nil {
		// No plan = unlimited, which includes orgs.
		return err == nil
	}
	return plan.OrgsAllowed
}

// handleUserCreateOrg lets a plan-entitled user mint an organization they
// administer. The org inherits the creator's plan.
func (s *Server) handleUserCreateOrg(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	if !s.userCanCreateOrg(u) {
		http.Error(w, "your plan does not include organizations", http.StatusForbidden)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if !store.ValidSlug(name) {
		s.settingsFlash(w, r, "Invalid organization name.")
		return
	}
	if _, err := s.db.GetAccount(name); err == nil {
		s.settingsFlash(w, r, "That name is taken.")
		return
	}
	org, err := s.db.EnsureAccount(name, "org")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if u.Role != "admin" {
		if personal, err := s.db.GetAccount(u.Name); err == nil && personal.PlanID != 0 {
			_ = s.db.SetAccountPlan(org.ID, personal.PlanID)
		}
	}
	if err := s.db.SetMember(org.ID, u.ID, "admin"); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.settingsFlash(w, r, fmt.Sprintf("Organization %q created.", name))
}

// ---- plan limit enforcement (create-time checks) ----

// checkCacheQuota returns an error when the account's plan caps caches and
// the cap is reached.
func (s *Server) checkCacheQuota(acc *store.Account) error {
	plan, err := s.db.AccountPlan(acc)
	if err != nil || plan == nil || plan.MaxCaches == 0 {
		return err
	}
	caches, err := s.db.ListAccountCaches(acc.ID)
	if err != nil {
		return err
	}
	if int64(len(caches)) >= plan.MaxCaches {
		return fmt.Errorf("plan %q allows at most %d caches", plan.Name, plan.MaxCaches)
	}
	return nil
}

// checkStorageQuota rejects pushes once an account's plan storage cap is
// reached. Logical bytes (summed NarSize) are the quota currency; pulls keep
// working — over-quota accounts go read-only, data is never auto-deleted.
func (s *Server) checkStorageQuota(c *store.Cache) error {
	acc, err := s.db.GetAccountByID(c.AccountID)
	if err != nil {
		return nil // account lookup failing must not block pushes
	}
	plan, err := s.db.AccountPlan(acc)
	if err != nil || plan == nil || plan.MaxStorage == 0 {
		return nil
	}
	used, err := s.db.AccountLogicalBytes(acc.ID)
	if err != nil {
		return nil
	}
	if used >= plan.MaxStorage {
		return fmt.Errorf("storage quota exceeded (plan %q, %d of %d bytes) — account is read-only for pushes", plan.Name, used, plan.MaxStorage)
	}
	return nil
}

// checkMemberQuota is the same gate for org membership.
func (s *Server) checkMemberQuota(acc *store.Account) error {
	plan, err := s.db.AccountPlan(acc)
	if err != nil || plan == nil || plan.MaxMembers == 0 {
		return err
	}
	members, err := s.db.ListMembers(acc.ID)
	if err != nil {
		return err
	}
	if int64(len(members)) >= plan.MaxMembers {
		return fmt.Errorf("plan %q allows at most %d members", plan.Name, plan.MaxMembers)
	}
	return nil
}
