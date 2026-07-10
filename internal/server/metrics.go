package server

import (
	"fmt"
	"net/http"
	"sync/atomic"
	"time"
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
	reqTotal     atomic.Int64 // every HTTP request
	reqDurNs     atomic.Int64 // summed request wall time
}

// counters returns every metric as name → value, shared by the Prometheus
// and JSON shapes so the two can't drift.
func (m *metrics) counters() []struct {
	Name, Help string
	V          int64
} {
	return []struct {
		Name, Help string
		V          int64
	}{
		{"xilo_narinfo_hits_total", "narinfo lookups that were found", m.narinfoHit.Load()},
		{"xilo_narinfo_misses_total", "narinfo lookups that 404'd", m.narinfoMiss.Load()},
		{"xilo_nar_served_total", "NAR downloads served", m.narServed.Load()},
		{"xilo_nar_bytes_total", "uncompressed NAR bytes served", m.narBytes.Load()},
		{"xilo_chunks_received_total", "chunk uploads accepted", m.chunksRecv.Load()},
		{"xilo_chunks_deduped_total", "chunk uploads already present (deduped)", m.chunksDedup.Load()},
		{"xilo_paths_pushed_total", "store paths registered", m.pathsPushed.Load()},
		{"xilo_auth_failures_total", "rejected auth attempts", m.authFailures.Load()},
		{"xilo_http_requests_total", "cache-protocol HTTP requests handled (admin/static/probes excluded)", m.reqTotal.Load()},
		{"xilo_http_request_duration_ns_total", "summed cache-protocol request wall time in nanoseconds", m.reqDurNs.Load()},
	}
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	m := &s.metrics
	if wantsJSON(r) {
		out := map[string]int64{"uptime_seconds": int64(time.Since(s.started).Seconds())}
		for _, c := range m.counters() {
			out[c.Name] = c.V
		}
		jsonOut(w, out)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	for _, c := range m.counters() {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", c.Name, c.Help, c.Name, c.Name, c.V)
	}
}
