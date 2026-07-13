package server

import (
	"fmt"
	"net/http"
	"runtime"
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

// runtimeGauges surfaces the process health signals a leak would show up in
// (goroutine count, heap, GC). Hand-rolled from the runtime package to keep
// the zero-runtime-dependency guarantee.
func runtimeGauges() []struct {
	Name, Help string
	V          int64
} {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return []struct {
		Name, Help string
		V          int64
	}{
		{"go_goroutines", "number of live goroutines", int64(runtime.NumGoroutine())},
		{"go_heap_inuse_bytes", "heap bytes in use", int64(ms.HeapInuse)},
		{"go_sys_bytes", "total bytes obtained from the OS", int64(ms.Sys)},
		{"go_gc_cycles_total", "completed GC cycles", int64(ms.NumGC)},
	}
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	// Operational counters (auth failures, traffic, heap) are not public.
	// Allow an instance-admin token (Prometheus/automation, via authorization
	// config) or a signed-in owner (the dashboard, a human).
	if !s.db.AuthorizeAdmin(extractToken(r), time.Now().Unix()) {
		if u := s.currentUser(r); u == nil || u.Role != "owner" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="xilo"`)
			http.Error(w, "admin token or owner session required", http.StatusUnauthorized)
			return
		}
	}
	m := &s.metrics
	if wantsJSON(r) {
		out := map[string]int64{"uptime_seconds": int64(time.Since(s.started).Seconds())}
		for _, c := range m.counters() {
			out[c.Name] = c.V
		}
		for _, g := range runtimeGauges() {
			out[g.Name] = g.V
		}
		jsonOut(w, out)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	for _, c := range m.counters() {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", c.Name, c.Help, c.Name, c.Name, c.V)
	}
	for _, g := range runtimeGauges() {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %d\n", g.Name, g.Help, g.Name, g.Name, g.V)
	}
}
