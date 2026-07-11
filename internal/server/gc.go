package server

import (
	"context"
	"time"
)

// runGC evicts paths past their retention window (per-cache override, else the
// global default) then sweeps unreferenced chunks older than the grace window.
func (s *Server) runGC(ctx context.Context) (deleted int, freed int64, err error) {
	now := time.Now()

	globalRetention := parseDur(s.cfg.GC.Retention)
	caches, err := s.db.ListCaches()
	if err != nil {
		return 0, 0, err
	}
	// Plan retention ceilings, resolved once per account per sweep.
	ceiling := map[int64]time.Duration{}
	for _, c := range caches {
		if _, ok := ceiling[c.AccountID]; ok {
			continue
		}
		var lim time.Duration
		if acc, err := s.db.GetAccountByID(c.AccountID); err == nil {
			if plan, err := s.db.AccountPlan(acc); err == nil && plan != nil && plan.MaxRetention > 0 {
				lim = time.Duration(plan.MaxRetention) * time.Second
			}
		}
		ceiling[c.AccountID] = lim
	}
	for _, c := range caches {
		ret := globalRetention
		if c.Retention > 0 {
			ret = time.Duration(c.Retention) * time.Second
		}
		// A plan ceiling caps whatever the cache asked for (including
		// "no retention at all" — the ceiling then IS the retention).
		if lim := ceiling[c.AccountID]; lim > 0 && (ret == 0 || ret > lim) {
			ret = lim
		}
		if ret > 0 {
			cutoff := now.Add(-ret).Unix()
			if _, err := s.db.EvictCachePathsOlderThan(c.ID, cutoff); err != nil {
				return 0, 0, err
			}
		}
		// Per-cache storage cap: evict least-recently-pulled paths over the cap.
		if _, err := s.db.EnforceCacheCap(c.ID, c.MaxBytes); err != nil {
			return 0, 0, err
		}
	}

	// Global storage cap: evict LRU across all caches until under.
	if _, err := s.db.EnforceGlobalCap(s.cfg.Limits.TotalBytes()); err != nil {
		return 0, 0, err
	}

	grace := parseDurSafe(s.cfg.GC.Grace, time.Hour)
	graceCutoff := now.Add(-grace).Unix()
	for name, st := range s.sts {
		d, f, err := s.db.GC(ctx, st, name, graceCutoff)
		deleted += d
		freed += f
		if err != nil {
			return deleted, freed, err
		}
	}
	return deleted, freed, nil
}
