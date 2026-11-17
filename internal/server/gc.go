package server

import (
	"context"
	"time"

	"github.com/stubbedev/xilo/internal/gc"
)

func (s *Server) runGC(ctx context.Context, olderThan time.Duration) (gc.Result, error) {
	return gc.Run(ctx, s.db, s.sts, gc.Sweep{
		Policy:        s.gcPolicy,
		ConfiguredCap: s.cfg.Limits.TotalBytes(),
		OlderThan:     olderThan,
	}, time.Now())
}
