package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/stubbedev/xilo/internal/server/views"
	"github.com/stubbedev/xilo/internal/store"
)

// The status dashboard samples counters on a fixed cadence; the ring holds
// three hours of history (the largest selectable window). Charts never draw
// more than ~drawnPoints points — longer windows are bucketed server-side, so
// render cost is constant regardless of window or uptime.
const (
	statusEvery = 5 * time.Second
	ringCap     = 3 * 60 * 12 // 3h of 5s samples
	drawnPoints = 120
)

// statusPoint is one sampled interval.
type statusPoint struct {
	ReqPerSec float64
	LatMs     float64 // mean request latency over the interval
	NarBps    float64 // NAR payload bytes/s served
	Stored    int64   // on-disk bytes at sample time
}

type statusRing struct {
	mu                          sync.Mutex
	pts                         []statusPoint
	lastReq, lastDurNs, lastNar int64
	global                      store.Global // cached; refreshed every globalEveryTicks
	tick                        int
	// current-minute accumulator, flushed to the store on minute rollover
	curMin                 int64
	minReq, minLat, minBps float64
}

// globalEveryTicks spaces out GlobalStats refreshes: the stored-bytes sum is a
// chunks-table scan, and 60s freshness is plenty for a dashboard KPI. All other
// chart data comes from in-process atomics and costs nothing.
const globalEveryTicks = 12

func (s *Server) startStatusSampler(ctx context.Context) {
	go func() {
		s.sampleStatus() // prime the ring and the cached GlobalStats right away
		t := time.NewTicker(statusEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.sampleStatus()
				s.flushEgress()
			}
		}
	}()
}

func (s *Server) sampleStatus() {
	m := &s.metrics
	req, dur, nar := m.reqTotal.Load(), m.reqDurNs.Load(), m.narBytes.Load()

	r := &s.stat
	r.mu.Lock()
	needGlobal := r.tick%globalEveryTicks == 0
	r.tick++
	r.mu.Unlock()
	var g store.Global
	if needGlobal {
		g, _ = s.db.GlobalStats() // queried outside the lock so reads never stall on it
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if needGlobal {
		r.global = g
	} else {
		g = r.global
	}
	dreq := req - r.lastReq
	secs := statusEvery.Seconds()
	p := statusPoint{ReqPerSec: float64(dreq) / secs, NarBps: float64(nar-r.lastNar) / secs, Stored: g.StoredBytes}
	if dreq > 0 {
		p.LatMs = float64(dur-r.lastDurNs) / float64(dreq) / 1e6
	}
	r.lastReq, r.lastDurNs, r.lastNar = req, dur, nar
	r.pts = append(r.pts, p)
	if len(r.pts) > ringCap {
		r.pts = r.pts[len(r.pts)-ringCap:]
	}

	// Roll the current minute; flush the finished one outside the lock (the
	// DB writer must never be waited on while readers want this mutex).
	minute := time.Now().Unix() / 60 * 60
	var flush *store.MetricMinute
	if r.curMin != 0 && minute != r.curMin {
		flush = &store.MetricMinute{TS: r.curMin, Req: r.minReq, Lat: r.minLat, Bps: r.minBps, Stored: p.Stored}
		r.minReq, r.minLat, r.minBps = 0, 0, 0
	}
	r.curMin = minute
	r.minReq = max(r.minReq, p.ReqPerSec)
	r.minLat = max(r.minLat, p.LatMs)
	r.minBps = max(r.minBps, p.NarBps)
	if flush != nil {
		go func() {
			if err := s.db.AddMetricMinute(*flush); err != nil {
				log.Printf("status: persist metrics minute: %v", err)
			}
		}()
	}
}

// seriesSet is one computed dashboard window: four aligned series plus their
// slot timestamps and the window bounds (unix seconds).
type seriesSet struct {
	req, lat, bps, stored []float64
	times                 []int64
	minT, maxT            int64
	fromStr, toStr        string // set on custom ranges
}

// statusSeries computes the chart series for a range. Live windows ≤3h come
// from the in-memory 5s ring; longer presets and custom date ranges read the
// persisted minute rollups. At most drawnPoints points per chart, always.
func (s *Server) statusSeries(q statusRangeQ) seriesSet {
	s.stat.mu.Lock()
	g := s.stat.global
	liveReq, liveLat, liveBps := s.stat.minReq, s.stat.minLat, s.stat.minBps
	s.stat.mu.Unlock()

	// Rollup-backed ranges that reach "now" lag up to a minute behind; patch
	// the newest slot with the in-progress minute so live views read live.
	patchLive := func(set *seriesSet) {
		vals := []float64{liveReq, liveLat, liveBps, float64(g.StoredBytes)}
		for i, sl := range [][]float64{set.req, set.lat, set.bps, set.stored} {
			if len(sl) > 0 {
				sl[len(sl)-1] = max(sl[len(sl)-1], vals[i])
			}
		}
	}

	var set seriesSet
	now := time.Now().Unix()
	switch {
	case q.Custom:
		set.fromStr = q.From.Format("2006-01-02")
		set.toStr = q.To.AddDate(0, 0, -1).Format("2006-01-02")
		set.minT, set.maxT = q.From.Unix(), q.To.Unix()
		if set.maxT > now {
			set.maxT = now // don't chart the not-yet part of today
		}
		set.req, set.lat, set.bps, set.stored, set.times = s.rangeSeries(set.minT, q.To.Unix())
		if !q.To.Before(time.Now()) {
			patchLive(&set)
		}
	case q.WinMin > 180: // beyond the ring: minute rollups
		set.minT, set.maxT = now-int64(q.WinMin)*60, now
		set.req, set.lat, set.bps, set.stored, set.times = s.rangeSeries(set.minT, set.maxT)
		patchLive(&set)
	default: // live 5s ring
		set.minT, set.maxT = now-int64(q.WinMin)*60, now
		winSamples := q.WinMin * 60 / int(statusEvery.Seconds())
		s.stat.mu.Lock()
		pts := s.stat.pts
		if len(pts) > winSamples {
			pts = pts[len(pts)-winSamples:]
		}
		pts = append([]statusPoint(nil), pts...)
		s.stat.mu.Unlock()
		for _, p := range pts {
			set.req = append(set.req, p.ReqPerSec)
			set.lat = append(set.lat, p.LatMs)
			set.bps = append(set.bps, p.NarBps)
			set.stored = append(set.stored, float64(p.Stored))
		}
		per := (winSamples + drawnPoints - 1) / drawnPoints // samples per drawn point
		set.req, set.lat, set.bps, set.stored = bucketMax(set.req, per), bucketMax(set.lat, per), bucketMax(set.bps, per), bucketMax(set.stored, per)
		// Pad the not-yet-sampled head of the window with zeros, same as the
		// rollup views: the zero line spans the whole window instead of the
		// chart starting mid-air at process start.
		fullSlots := (winSamples + per - 1) / per
		if pad := fullSlots - len(set.req); pad > 0 {
			zero := make([]float64, pad)
			set.req = append(append([]float64{}, zero...), set.req...)
			set.lat = append(append([]float64{}, zero...), set.lat...)
			set.bps = append(append([]float64{}, zero...), set.bps...)
			set.stored = append(append([]float64{}, zero...), set.stored...)
		}
		stepSec := int64(per) * int64(statusEvery.Seconds())
		set.times = make([]int64, len(set.req))
		for i := range set.times {
			set.times[i] = set.maxT - int64(len(set.times)-1-i)*stepSec
		}
	}
	return set
}

var (
	fmtReq = func(v float64) string { return fmt.Sprintf("%.1f/s", v) }
	fmtLat = func(v float64) string { return fmt.Sprintf("%.1f ms", v) }
	fmtBps = func(v float64) string { return humanBytes(int64(v)) + "/s" }
	fmtB   = func(v float64) string { return humanBytes(int64(v)) }
)

// statusData builds the page view model (initial server render; the poller
// keeps it fresh through the JSON endpoint afterwards).
func (s *Server) statusData(q statusRangeQ) views.StatusData {
	_, healthErr := s.db.ListCaches()
	m := &s.metrics
	set := s.statusSeries(q)

	s.stat.mu.Lock()
	g := s.stat.global
	s.stat.mu.Unlock()

	d := views.StatusData{
		Healthy:   healthErr == nil,
		Uptime:    humanDur(time.Since(s.started)),
		HitPct:    hitPct(m.narinfoHit.Load(), m.narinfoMiss.Load()),
		Global:    g,
		AuthFails: m.authFailures.Load(),
		NarServed: m.narServed.Load(),
		Requests:  m.reqTotal.Load(),
		Bytes:     humanBytes,
		Updated:   time.Now().Format("15:04:05"),
		Window:    q.WinMin,
		From:      set.fromStr,
		To:        set.toStr,
	}
	d.Charts = []views.ChartData{
		statusChartData("req", "Requests /s", set.req, set.times, fmtReq),
		statusChartData("lat", "Latency (ms)", set.lat, set.times, fmtLat),
		statusChartData("thru", "NAR throughput", set.bps, set.times, fmtBps),
		statusChartData("stored", "Stored bytes", set.stored, set.times, fmtB),
	}
	return d
}

// statusChartJSON is one chart's slice of the polled JSON payload.
type statusChartJSON struct {
	Cur    string       `json:"cur"`
	Peak   string       `json:"peak"`
	Points [][2]float64 `json:"points"` // [unix ms, value]
}

// statusJSON is the poll payload: formatted KPI strings plus raw chart
// points. The client only places these values; every number is computed here.
type statusJSON struct {
	Healthy   bool                       `json:"healthy"`
	Uptime    string                     `json:"uptime"`
	HitPct    string                     `json:"hitPct"`
	Stored    string                     `json:"stored"`
	Paths     string                     `json:"paths"`
	Nars      string                     `json:"nars"`
	Requests  string                     `json:"requests"`
	AuthFails string                     `json:"authFails"`
	Updated   string                     `json:"updated"`
	MinT      int64                      `json:"minT"` // unix ms, chart x range
	MaxT      int64                      `json:"maxT"`
	Charts    map[string]statusChartJSON `json:"charts"`
}

func (s *Server) buildStatusJSON(q statusRangeQ) statusJSON {
	_, healthErr := s.db.ListCaches()
	m := &s.metrics
	set := s.statusSeries(q)

	s.stat.mu.Lock()
	g := s.stat.global
	s.stat.mu.Unlock()

	chart := func(vals []float64, f func(float64) string) statusChartJSON {
		c := statusChartJSON{Points: make([][2]float64, len(vals))}
		for i, v := range vals {
			c.Points[i] = [2]float64{float64(set.times[i] * 1000), v}
		}
		if len(vals) > 0 {
			c.Cur = f(vals[len(vals)-1])
			if m := maxOf(vals); m > 0 {
				c.Peak = f(m)
			}
		}
		return c
	}
	return statusJSON{
		Healthy:   healthErr == nil,
		Uptime:    humanDur(time.Since(s.started)),
		HitPct:    hitPct(m.narinfoHit.Load(), m.narinfoMiss.Load()),
		Stored:    humanBytes(g.StoredBytes),
		Paths:     views.Count(g.Paths),
		Nars:      views.Count(m.narServed.Load()),
		Requests:  views.Count(m.reqTotal.Load()),
		AuthFails: views.Count(m.authFailures.Load()),
		Updated:   time.Now().Format("15:04:05"),
		MinT:      set.minT * 1000,
		MaxT:      set.maxT * 1000,
		Charts: map[string]statusChartJSON{
			"req":    chart(set.req, fmtReq),
			"lat":    chart(set.lat, fmtLat),
			"thru":   chart(set.bps, fmtBps),
			"stored": chart(set.stored, fmtB),
		},
	}
}

// rangeSeries buckets persisted minute rollups for [from, to) into exactly
// drawnPoints slots per metric (max per slot; empty slots stay zero, so
// downtime gaps are visible instead of interpolated away). times holds each
// slot's start timestamp.
func (s *Server) rangeSeries(from, to int64) (reqRate, latMs, narBps, stored []float64, times []int64) {
	rows, err := s.db.MetricRange(from, to)
	if err != nil || to <= from {
		return nil, nil, nil, nil, nil
	}
	reqRate = make([]float64, drawnPoints)
	latMs = make([]float64, drawnPoints)
	narBps = make([]float64, drawnPoints)
	stored = make([]float64, drawnPoints)
	times = make([]int64, drawnPoints)
	span := to - from
	for i := range times {
		times[i] = from + int64(i)*span/int64(drawnPoints)
	}
	for _, r := range rows {
		i := int((r.TS - from) * int64(drawnPoints) / span)
		if i < 0 || i >= drawnPoints {
			continue
		}
		reqRate[i] = max(reqRate[i], r.Req)
		latMs[i] = max(latMs[i], r.Lat)
		narBps[i] = max(narBps[i], r.Bps)
		stored[i] = max(stored[i], float64(r.Stored))
	}
	return reqRate, latMs, narBps, stored, times
}

// bucketMax groups per consecutive samples into their max, aligned from the
// newest edge so the freshest drawn point is a full bucket. Max (not mean)
// keeps spikes visible and the peak label honest with the drawn curve.
func bucketMax(vals []float64, per int) []float64 {
	if per <= 1 {
		return vals
	}
	out := make([]float64, 0, (len(vals)+per-1)/per)
	for hi := len(vals); hi > 0; hi -= per {
		lo := max(hi-per, 0)
		m := 0.0
		for _, v := range vals[lo:hi] {
			if v > m {
				m = v
			}
		}
		out = append(out, m)
	}
	slices.Reverse(out)
	return out
}

// statusRangeQ is a validated chart range: either a live preset window
// (WinMin minutes back from now) or a custom date range [From, To).
type statusRangeQ struct {
	WinMin   int
	From, To time.Time
	Custom   bool
}

// statusPresets are the allowed live windows in minutes. Windows past the
// ring (>180) read minute rollups from the store instead.
var statusPresets = map[string]int{
	"10": 10, "30": 30, "60": 60, "180": 180,
	"720": 720, "1440": 1440, "10080": 10080, "43200": 43200,
}

// statusRange validates the window/from/to query params. A parseable from+to
// pair wins over the preset; anything invalid falls back to the default.
func statusRange(r *http.Request) statusRangeQ {
	if from, err := time.ParseInLocation("2006-01-02", r.URL.Query().Get("from"), time.Local); err == nil {
		if to, err := time.ParseInLocation("2006-01-02", r.URL.Query().Get("to"), time.Local); err == nil && !to.Before(from) {
			return statusRangeQ{From: from, To: to.AddDate(0, 0, 1), Custom: true} // To is inclusive
		}
	}
	if m, ok := statusPresets[r.URL.Query().Get("window")]; ok {
		return statusRangeQ{WinMin: m}
	}
	return statusRangeQ{WinMin: 60}
}

// statusChartData packages one series (header strings + drawn points) for the
// server-side templui chart render.
func statusChartData(id, label string, vals []float64, times []int64, f func(float64) string) views.ChartData {
	c := views.ChartData{ID: id, Label: label}
	c.Points = vals
	c.Labels = make([]string, len(vals))
	for i := range vals {
		if i < len(times) {
			c.Labels[i] = time.Unix(times[i], 0).Format("15:04")
		}
	}
	if len(vals) == 0 {
		return c
	}
	c.Cur = f(vals[len(vals)-1])
	if m := maxOf(vals); m > 0 {
		c.Peak = f(m)
	}
	return c
}

func maxOf(vals []float64) float64 {
	m := 0.0
	for _, v := range vals {
		if v > m {
			m = v
		}
	}
	return m
}

// statusRate whitelists the dashboard poll interval (seconds).
func statusRate(r *http.Request) int {
	switch r.URL.Query().Get("rate") {
	case "2":
		return 2
	case "10":
		return 10
	case "30":
		return 30
	case "60":
		return 60
	default:
		return 5
	}
}

// hitPct formats the narinfo hit ratio, "—" before any traffic.
func hitPct(hits, misses int64) string {
	if hits+misses == 0 {
		return "—"
	}
	return fmt.Sprintf("%d%%", hits*100/(hits+misses))
}

// humanDur renders an uptime coarsely: 45s, 12m, 3h 20m, 5d 4h.
func humanDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	d := s.statusData(statusRange(r))
	d.Rate = statusRate(r)
	d.Nav = s.nav(r, s.currentUser(r))
	views.StatusPage(d).Render(r.Context(), w)
}

// handleStatusData is the polling target: pure JSON, applied client-side to
// the existing chart instances (no DOM swaps, no blink).
func (s *Server) handleStatusData(w http.ResponseWriter, r *http.Request) {
	if !s.loggedIn(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	jsonOut(w, s.buildStatusJSON(statusRange(r)))
}

// wantsJSON reports whether a health/metrics request asked for the JSON shape.
func wantsJSON(r *http.Request) bool {
	return r.URL.Query().Get("format") == "json" ||
		strings.Contains(r.Header.Get("Accept"), "application/json")
}
