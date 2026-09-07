package server

import (
	"net/http"
	"strconv"
	"time"

	"github.com/stubbedev/xilo/internal/server/views"
)

// Instance rules: the sysadmin's knobs, stored in `settings` and read at the
// point they apply rather than at boot, so changing one takes effect without a
// restart. Each of these has exactly one enforcement site — a rule the UI can
// set but nothing consults is worse than no rule at all.
const (
	settingInstanceCap = "instance_max_bytes" // total bytes across every cache
	settingDefaultPlan = "default_plan"       // plan a signup lands on
	settingCacheRetain = "default_retention"  // seconds a new cache keeps a path
	settingMaxTokenTTL = "max_token_ttl"      // seconds, the longest a token may live
)

// settingInt reads a rule as a non-negative count of bytes or seconds. Zero
// (and anything unparseable) means "no rule", which is the default for all of
// them: a fresh instance imposes nothing.
func (s *Server) settingInt(key string) int64 {
	n, err := strconv.ParseInt(s.db.Setting(key), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// instanceCap is the ceiling on everything stored, from the rule when set and
// the config file otherwise — the config value is the floor an operator boots
// with, the rule is what they can change while it runs.
func (s *Server) instanceCap() int64 {
	if n := s.settingInt(settingInstanceCap); n > 0 {
		return n
	}
	return s.cfg.Limits.TotalBytes()
}

// defaultRetention is the retention a new cache starts with when its creator
// names none, so an instance can promise "nothing older than 90 days" without
// asking every tenant to set it.
func (s *Server) defaultRetention() int64 { return s.settingInt(settingCacheRetain) }

// capTokenExpiry holds a token's expiry inside the instance's maximum: a
// never-expiring token on a service someone pays for is a key left in a door.
// Zero expiry means "never", which is exactly what the cap exists to refuse.
func (s *Server) capTokenExpiry(expires int64) int64 {
	max := s.settingInt(settingMaxTokenTTL)
	if max <= 0 {
		return expires
	}
	limit := time.Now().Unix() + max
	if expires == 0 || expires > limit {
		return limit
	}
	return expires
}

// defaultPlan is the plan a signup lands on when the form names none.
func (s *Server) defaultPlan() int64 { return s.settingInt(settingDefaultPlan) }

// handleInstanceRules stores the instance rules. Empty means "no rule", which
// is how one gets removed.
func (s *Server) handleInstanceRules(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	for key, secs := range map[string]int64{
		settingInstanceCap: formValueOrZero(formBytes(r, "instance_cap")),
		settingCacheRetain: formValueOrZero(formSeconds(r, "default_retention")),
		settingMaxTokenTTL: formValueOrZero(formSeconds(r, "max_token_ttl")),
		settingDefaultPlan: formInt(r, "default_plan"),
	} {
		if err := s.db.SetSetting(key, strconv.FormatInt(secs, 10)); err != nil {
			uiError(w, r, err)
			return
		}
	}
	s.instanceFlash(w, r, views.T(r.Context(), "flash.settingssaved"))
}

// formValueOrZero turns the empty-keeps-current contract of formBytes and
// formSeconds into empty-clears-the-rule, which is what a rules form needs: a
// blank field is how a sysadmin removes a ceiling.
func formValueOrZero(v int64, ok bool) int64 {
	if !ok {
		return 0
	}
	return v
}

func formInt(r *http.Request, name string) int64 {
	n, _ := strconv.ParseInt(r.FormValue(name), 10, 64)
	if n < 0 {
		return 0
	}
	return n
}
