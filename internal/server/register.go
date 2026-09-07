package server

import (
	"context"
	"errors"
	"log"
	"net/http"
	netmail "net/mail"
	"strconv"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"github.com/stubbedev/xilo/internal/server/views"
	"github.com/stubbedev/xilo/internal/store"
)

// Self-service surface: self-registration with plan selection, instance
// policy toggles, plan CRUD, org creation, and pending-user approval. All of
// it exists only when self_service is on; without it the admin creates every
// user and organization. Accounts and caches work the same either way — this
// flag governs signup, not tenancy.

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
	return s.cfg.SelfService && s.db.SettingBool("allow_registrations", false)
}

// validEmail is a light structural check (net/mail) — good enough to reject
// typos and empties; real proof is a verification link (future work).
func validEmail(email string) bool {
	if email == "" {
		return false
	}
	addr, err := netmail.ParseAddress(email)
	return err == nil && addr.Address == email && strings.Contains(email, "@")
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
		uiError(w, r, err)
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
	if !s.logins.allow(s.clientIP(r)) {
		uiFail(w, r, http.StatusTooManyRequests, views.T(r.Context(), "err.throttled"), nil)
		return
	}
	plans, err := s.db.PublicPlans()
	if err != nil {
		uiError(w, r, err)
		return
	}
	fail := func(msg string) {
		views.Register(plans, views.Flash{Msg: msg}).Render(r.Context(), w)
	}

	username := strings.TrimSpace(r.FormValue("username"))
	email := strings.TrimSpace(r.FormValue("email"))
	password := r.FormValue("password")
	if !store.ValidSlug(username) {
		fail(views.T(r.Context(), "reg.err.username"))
		return
	}
	// Multi-tenant registration always requires a valid, verifiable email —
	// it is the account-recovery and notification channel.
	if !validEmail(email) {
		fail("A valid email address is required.")
		return
	}
	if len(password) < 8 {
		fail(views.T(r.Context(), "flash.pwshort"))
		return
	}
	if len(password) > 72 {
		fail(views.T(r.Context(), "flash.pwlong"))
		return
	}

	var plan *store.Plan
	pid, _ := strconv.ParseInt(r.FormValue("plan"), 10, 64)
	if pid == 0 {
		pid = s.defaultPlan() // the instance's rule for "no plan named"
	}
	if pid != 0 {
		p, err := s.db.GetPlan(pid)
		if err != nil || !p.Public {
			fail(views.T(r.Context(), "reg.err.plan"))
			return
		}
		plan = p
	} else if len(plans) > 0 {
		fail(views.T(r.Context(), "reg.err.plan"))
		return
	}

	orgName := strings.TrimSpace(r.FormValue("org"))
	if orgName != "" {
		if plan == nil || !plan.OrgsAllowed {
			fail(views.T(r.Context(), "reg.err.noorgs"))
			return
		}
		if !store.ValidSlug(orgName) {
			fail(views.T(r.Context(), "reg.err.orgname"))
			return
		}
		if _, err := s.db.GetAccount(orgName); err == nil {
			fail(views.T(r.Context(), "flash.nametaken"))
			return
		}
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		uiError(w, r, err)
		return
	}
	var u *store.User
	if s.requireApproval() {
		u, err = s.db.CreatePendingUser(username, email, string(hash))
	} else {
		u, err = s.db.CreateUser(username, email, string(hash), "user")
	}
	if errors.Is(err, store.ErrNameTaken) {
		fail(views.T(r.Context(), "flash.nametaken"))
		return
	}
	if err != nil {
		// Generic message for anything else (notably an email collision): the
		// raw driver error would confirm the email is registered (account
		// enumeration) and leak schema/engine strings to an anonymous client.
		log.Printf("register %q: %v", username, err)
		fail(views.T(r.Context(), "reg.err.failed"))
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
			_ = s.db.MakeOwner(org.ID, u.ID)
		}
	}

	if u.Status == "pending" {
		s.mailUser(u, views.T(r.Context(), "mail.pendingsub"),
			views.Tf(r.Context(), "mail.pendingbody", u.Name, s.cfg.BaseURL))
		s.mailAdmins(func(ctx context.Context) (string, string) {
			return views.T(ctx, "mail.adminsub"),
				views.Tf(ctx, "mail.adminbody", u.Name, s.cfg.BaseURL)
		})
		views.Login(false, s.hasPasskeys(), s.registrationOpen(),
			views.Flash{Msg: views.T(r.Context(), "flash.regpending"), OK: true}).Render(r.Context(), w)
		return
	}
	s.mailUser(u, views.Tf(r.Context(), "mail.welcomesub", s.cfg.BaseURL),
		views.Tf(r.Context(), "mail.welcomebody", u.Name, s.cfg.BaseURL))
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
			uiError(w, r, err)
			return
		}
	}
	s.instanceFlash(w, r, views.T(r.Context(), "flash.settingssaved"))
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
		s.instanceFlash(w, r, views.T(r.Context(), "flash.plannamereq"))
		return
	}
	if _, err := s.db.CreatePlan(&p); err != nil {
		s.flashStore(w, r, "/admin/settings", err)
		return
	}
	s.instanceFlash(w, r, views.Tf(r.Context(), "flash.plancreated", p.Name))
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
		uiError(w, r, err)
		return
	}
	p := planFromForm(r)
	p.ID = cur.ID
	if p.Name == "" {
		p.Name = cur.Name
	}
	if err := s.db.UpdatePlan(&p); err != nil {
		uiError(w, r, err)
		return
	}
	s.instanceFlash(w, r, views.Tf(r.Context(), "flash.planupdated", p.Name))
}

func (s *Server) handleDeletePlan(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.db.DeletePlan(id); err != nil {
		s.flashStore(w, r, "/admin/settings", err)
		return
	}
	s.instanceFlash(w, r, views.T(r.Context(), "flash.plandeleted"))
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
		uiError(w, r, err)
		return
	}
	mctx := mailCtx(u)
	s.mailUser(u, views.T(mctx, "mail.approvedsub"),
		views.Tf(mctx, "mail.approvedbody", u.Name, s.cfg.BaseURL, s.cfg.BaseURL))
	s.instanceFlash(w, r, views.Tf(r.Context(), "flash.userapproved", u.Name))
}

// userCanCreateOrg: instance admins always; otherwise the personal account's
// plan must include organizations (multi-tenant mode only).
func (s *Server) userCanCreateOrg(u *store.User) bool {
	if u == nil {
		return false
	}
	if u.Superadmin() {
		return true
	}
	if !s.cfg.SelfService {
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
		uiFail(w, r, http.StatusForbidden, views.T(r.Context(), "err.noorgs"), nil)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if !store.ValidSlug(name) {
		s.orgsFlash(w, r, views.T(r.Context(), "flash.badorgname"))
		return
	}
	if _, err := s.db.GetAccount(name); err == nil {
		s.orgsFlash(w, r, views.T(r.Context(), "flash.nametaken"))
		return
	}
	org, err := s.db.EnsureAccount(name, "org")
	if errors.Is(err, store.ErrSlugReserved) {
		s.orgsFlash(w, r, views.T(r.Context(), "flash.nametaken"))
		return
	}
	if err != nil {
		uiError(w, r, err)
		return
	}
	if !u.Superadmin() {
		if personal, err := s.db.GetAccount(u.Name); err == nil && personal.PlanID != 0 {
			_ = s.db.SetAccountPlan(org.ID, personal.PlanID)
		}
	}
	if err := s.db.MakeOwner(org.ID, u.ID); err != nil {
		uiError(w, r, err)
		return
	}
	s.orgsFlash(w, r, views.Tf(r.Context(), "flash.orgcreated", name))
}

// ---- plan limit enforcement (create-time checks) ----

// checkCacheQuota returns an error when the account's plan caps caches and
// the cap is reached.
func (s *Server) checkCacheQuota(ctx context.Context, acc *store.Account) error {
	plan, err := s.db.AccountPlan(acc)
	if err != nil || plan == nil || plan.MaxCaches == 0 {
		return err
	}
	caches, err := s.db.ListAccountCaches(acc.ID)
	if err != nil {
		return err
	}
	if int64(len(caches)) >= plan.MaxCaches {
		return errors.New(views.Tf(ctx, "quota.caches", plan.Name, plan.MaxCaches))
	}
	return nil
}

// checkStorageQuota rejects pushes once an account's plan storage cap is
// reached. Logical bytes (summed NarSize) are the quota currency; pulls keep
// working — over-quota accounts go read-only, data is never auto-deleted.
func (s *Server) checkStorageQuota(ctx context.Context, c *store.Cache) error {
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
		return errors.New(views.Tf(ctx, "quota.storage", humanBytes(used), humanBytes(plan.MaxStorage), plan.Name))
	}
	return nil
}

// checkMemberQuota is the same gate for org membership.
func (s *Server) checkMemberQuota(ctx context.Context, acc *store.Account) error {
	plan, err := s.db.AccountPlan(acc)
	if err != nil || plan == nil || plan.MaxMembers == 0 {
		return err
	}
	members, err := s.db.ListMembers(acc.ID)
	if err != nil {
		return err
	}
	if int64(len(members)) >= plan.MaxMembers {
		return errors.New(views.Tf(ctx, "quota.members", plan.Name, plan.MaxMembers))
	}
	return nil
}
