package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"sync/atomic"
	"time"
)

// countersKey is the settings-table row holding the persisted counter totals.
const countersKey = "counters"

// restoreCounters loads the persisted totals at startup — without this every
// restart zeroes the status-page KPIs (hit rate shows "—", NARs served 0)
// and /metrics counters. It also baselines the sampler's last-seen values so
// the first 5s sample doesn't read the whole restored history as one burst.
func (s *Server) restoreCounters() {
	if v := s.db.Setting(countersKey); v != "" {
		var snap map[string]int64
		if err := json.Unmarshal([]byte(v), &snap); err == nil {
			s.metrics.restore(snap)
		}
	}
	s.stat.lastPull = s.metrics.reqTotal.Load()
	s.stat.lastReq = s.stat.lastPull + s.metrics.pushReq.Load()
	s.stat.lastDurNs = s.metrics.reqDurNs.Load()
	s.stat.lastNar = s.metrics.narBytes.Load()
}

// persistCounters saves the totals, skipping the write when nothing changed
// (idle instances shouldn't churn the WAL every tick). Called only from the
// status sampler goroutine, so lastSaved needs no lock.
func (s *Server) persistCounters() {
	b, err := json.Marshal(s.metrics.snapshot())
	if err != nil || string(b) == s.lastSavedCounters {
		return
	}
	if err := s.db.SetSetting(countersKey, string(b)); err == nil {
		s.lastSavedCounters = string(b)
	}
}

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
	// Pull protocol (narinfo/nar/cache-info) and push API (/c/…/api/…) are
	// counted separately: chunk uploads legitimately take seconds and would
	// otherwise drown the sub-ms serving latency the dashboard charts.
	reqTotal  atomic.Int64 // pull-protocol requests
	reqDurNs  atomic.Int64 // summed pull-request wall time
	pushReq   atomic.Int64 // push-API requests
	pushDurNs atomic.Int64 // summed push-request wall time
}

// counterDef ties a Prometheus name to its backing atomic. counters(),
// snapshot() and restore() all derive from defs() so they can't drift.
type counterDef struct {
	Name, Help string
	v          *atomic.Int64
}

func (m *metrics) defs() []counterDef {
	return []counterDef{
		{"xilo_narinfo_hits_total", "narinfo lookups that were found", &m.narinfoHit},
		{"xilo_narinfo_misses_total", "narinfo lookups that 404'd", &m.narinfoMiss},
		{"xilo_nar_served_total", "NAR downloads served", &m.narServed},
		{"xilo_nar_bytes_total", "uncompressed NAR bytes served", &m.narBytes},
		{"xilo_chunks_received_total", "chunk uploads accepted", &m.chunksRecv},
		{"xilo_chunks_deduped_total", "chunk uploads already present (deduped)", &m.chunksDedup},
		{"xilo_paths_pushed_total", "store paths registered", &m.pathsPushed},
		{"xilo_auth_failures_total", "rejected auth attempts", &m.authFailures},
		{"xilo_http_requests_total", "pull-protocol HTTP requests handled (push API, admin, static and probes excluded)", &m.reqTotal},
		{"xilo_http_request_duration_ns_total", "summed pull-protocol request wall time in nanoseconds", &m.reqDurNs},
		{"xilo_push_requests_total", "push-API HTTP requests handled", &m.pushReq},
		{"xilo_push_request_duration_ns_total", "summed push-API request wall time in nanoseconds", &m.pushDurNs},
	}
}

// counters returns every metric as name → value, shared by the Prometheus
// and JSON shapes so the two can't drift.
func (m *metrics) counters() []struct {
	Name, Help string
	V          int64
} {
	defs := m.defs()
	out := make([]struct {
		Name, Help string
		V          int64
	}, len(defs))
	for i, d := range defs {
		out[i] = struct {
			Name, Help string
			V          int64
		}{d.Name, d.Help, d.v.Load()}
	}
	return out
}

// snapshot/restore round-trip the totals through the settings table so the
// dashboard KPIs and /metrics survive restarts.
func (m *metrics) snapshot() map[string]int64 {
	out := make(map[string]int64)
	for _, d := range m.defs() {
		out[d.Name] = d.v.Load()
	}
	return out
}

func (m *metrics) restore(snap map[string]int64) {
	for _, d := range m.defs() {
		if v, ok := snap[d.Name]; ok && v > 0 {
			d.v.Store(v)
		}
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
