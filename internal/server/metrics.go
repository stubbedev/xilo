package server

import (
	"fmt"
	"net/http"
	"sync/atomic"
)

// metrics holds process counters exposed at /metrics in Prometheus text format.
// Hand-rolled to keep the zero-runtime-dependency guarantee.
type metrics struct {
	narinfoHit   atomic.Int64
	narinfoMiss  atomic.Int64
	narServed    atomic.Int64
	narBytes     atomic.Int64
	chunksRecv   atomic.Int64
	chunksDedup  atomic.Int64 // uploads that were already present
	pathsPushed  atomic.Int64
	authFailures atomic.Int64
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	m := &s.metrics
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	write := func(name, help string, v int64) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", name, help, name, name, v)
	}
	write("xilo_narinfo_hits_total", "narinfo lookups that were found", m.narinfoHit.Load())
	write("xilo_narinfo_misses_total", "narinfo lookups that 404'd", m.narinfoMiss.Load())
	write("xilo_nar_served_total", "NAR downloads served", m.narServed.Load())
	write("xilo_nar_bytes_total", "uncompressed NAR bytes served", m.narBytes.Load())
	write("xilo_chunks_received_total", "chunk uploads accepted", m.chunksRecv.Load())
	write("xilo_chunks_deduped_total", "chunk uploads already present (deduped)", m.chunksDedup.Load())
	write("xilo_paths_pushed_total", "store paths registered", m.pathsPushed.Load())
	write("xilo_auth_failures_total", "rejected auth attempts", m.authFailures.Load())
}
