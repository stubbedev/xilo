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
	for _, c := range caches {
		ret := globalRetention
		if c.Retention > 0 {
			ret = time.Duration(c.Retention) * time.Second
		}
		if ret <= 0 {
			continue
		}
		cutoff := now.Add(-ret).Unix()
		if _, err := s.db.EvictCachePathsOlderThan(c.ID, cutoff); err != nil {
			return 0, 0, err
		}
	}

	grace := parseDur(s.cfg.GC.Grace)
	graceCutoff := now.Add(-grace).Unix()
	return s.db.GC(ctx, s.st, graceCutoff)
}
