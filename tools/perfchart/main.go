// Command perfchart renders the committed performance numbers into the SVG
// charts the README embeds: the k6 gate (tests/k6/baselines/*.json) and the
// cross-implementation comparison (tests/bench/results.json).
//
// It lives outside cmd/xilo for the same reason schemagen does: nothing in a
// user's binary needs to draw a chart. Two files per chart, light and dark, so
// the README can pick one with <picture> and prefers-color-scheme; a single SVG
// carrying both palettes would depend on media queries surviving whatever
// renders it.
//
// Every number drawn comes from a committed file. The tool computes bar
// lengths and formats labels, and invents nothing: a metric missing from the
// input is a missing bar, never a zero.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"html"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func main() {
	k6Dir := flag.String("k6", "tests/k6/baselines", "directory of committed k6 baselines")
	benchFile := flag.String("bench", "tests/bench/results.json", "output of tests/bench/bench.sh")
	outDir := flag.String("out", "docs/perf", "directory to write the SVGs into")
	flag.Parse()

	if err := run(*k6Dir, *benchFile, *outDir); err != nil {
		fmt.Fprintln(os.Stderr, "perfchart:", err)
		os.Exit(1)
	}
}

func run(k6Dir, benchFile, outDir string) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	wrote := 0

	if charts, err := k6Charts(k6Dir); err != nil {
		fmt.Fprintln(os.Stderr, "perfchart: skipping k6 chart:", err)
	} else {
		if err := writeChart(outDir, "k6-gate", charts); err != nil {
			return err
		}
		wrote++
	}

	if charts, err := benchCharts(benchFile); err != nil {
		fmt.Fprintln(os.Stderr, "perfchart: skipping comparison chart:", err)
	} else {
		if err := writeChart(outDir, "compare", charts); err != nil {
			return err
		}
		wrote++
	}

	if wrote == 0 {
		return fmt.Errorf("no inputs found (looked for %s and %s)", k6Dir, benchFile)
	}
	return nil
}

// ── inputs ──────────────────────────────────────────────────────────────────

// baseline is one tests/k6/baselines/<suite>.json file. Metric entries carry
// more than numbers (a tolerance, a direction, how many runs were sampled), so
// the values are decoded loosely: a field this tool does not chart must never
// be the reason a chart fails to draw.
type baseline struct {
	Suite       string                    `json:"suite"`
	SampledRuns int                       `json:"sampled_runs"`
	Metrics     map[string]map[string]any `json:"metrics"`
}

func loadBaseline(path string) (*baseline, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var b baseline
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &b, nil
}

// stat reads one numeric statistic out of a baseline, reporting whether it was
// there and was a number.
func (b *baseline) stat(metric, field string) (float64, bool) {
	if b == nil {
		return 0, false
	}
	m, ok := b.Metrics[metric]
	if !ok {
		return 0, false
	}
	v, ok := m[field].(float64)
	if !ok || math.IsNaN(v) {
		return 0, false
	}
	return v, true
}

// direction is the baseline's own word for which way is better, so the chart's
// caption cannot contradict the gate it is drawn from.
func (b *baseline) direction(metric string) string {
	if b == nil {
		return ""
	}
	if m, ok := b.Metrics[metric]; ok {
		if d, ok := m["direction"].(string); ok {
			return d
		}
	}
	return ""
}

// benchResults is tests/bench/results.json, written by tests/bench/bench.sh.
type benchResults struct {
	Date     string                    `json:"date"`
	Runner   string                    `json:"runner"`
	XiloRev  string                    `json:"xilo_rev"`
	Workload map[string]any            `json:"workload"`
	Targets  map[string]map[string]any `json:"targets"`
}

func loadBench(path string) (*benchResults, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r benchResults
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(r.Targets) == 0 {
		return nil, fmt.Errorf("%s: no targets in results", path)
	}
	return &r, nil
}

// num reads a numeric field of one target, reporting whether it was there and
// usable. A target that was skipped carries an error string instead.
func (r *benchResults) num(target, key string) (float64, bool) {
	t, ok := r.Targets[target]
	if !ok {
		return 0, false
	}
	v, ok := t[key].(float64)
	if !ok || math.IsNaN(v) {
		return 0, false
	}
	return v, true
}

func (r *benchResults) str(target, key string) string {
	if t, ok := r.Targets[target]; ok {
		if s, ok := t[key].(string); ok {
			return s
		}
	}
	return ""
}

// ── chart construction ──────────────────────────────────────────────────────

type bar struct {
	label  string
	value  string  // the number, drawn at the end of the bar
	frac   float64 // 0..1 of the panel's scale
	accent bool    // xilo's own bar
}

type panel struct {
	title string
	scale string // what the panel's full width means
	bars  []bar
}

type chart struct {
	title    string
	subtitle string
	footnote string
	cols     int
	panels   []panel
}

// k6Charts builds the gate chart from the committed baselines: the numbers CI
// measures on every push and refuses to let regress.
func k6Charts(dir string) (chart, error) {
	perf, err := loadBaseline(filepath.Join(dir, "perf.json"))
	if err != nil {
		return chart{}, err
	}
	pressure, _ := loadBaseline(filepath.Join(dir, "pressure.json"))

	betterIs := func(b *baseline, metric, fallback string) string {
		switch b.direction(metric) {
		case "lower":
			return "lower is better"
		case "higher":
			return "higher is better"
		}
		return fallback
	}
	readScale := "ms, " + betterIs(perf, "http_req_duration{scenario:narinfo_hit}", "lower is better")
	read := panel{title: "Read latency, p95", scale: readScale}
	for _, row := range []struct {
		metric, label string
		src           *baseline
	}{
		{"http_req_duration{scenario:narinfo_hit}", "narinfo hit", perf},
		{"http_req_duration{scenario:narinfo_miss}", "narinfo miss", perf},
		{"http_req_duration{scenario:pull_identity}", "NAR pull", perf},
		{"http_req_duration{scenario:pull_zstd}", "NAR pull, zstd", perf},
		{"http_req_duration{scenario:pull_big}", "NAR pull, 64 MiB", perf},
		{"http_req_duration{scenario:mixed_narinfo}", "narinfo under push load", perf},
		{"http_req_duration{scenario:storm}", "narinfo, 512 clients", pressure},
		{"http_req_duration{scenario:flood}", "narinfo, 5000 rps arrival", pressure},
	} {
		if v, ok := row.src.stat(row.metric, "p(95)"); ok {
			read.bars = append(read.bars, bar{label: row.label, value: fmtMs(v), frac: v, accent: true})
		}
	}

	push := panel{
		title: "Push round trip, p95",
		scale: "ms, " + betterIs(perf, "iteration_duration{scenario:push_fresh}", "lower is better"),
	}
	for _, row := range []struct{ metric, label string }{
		{"iteration_duration{scenario:push_dedup}", "repeat push (deduped)"},
		{"iteration_duration{scenario:push_fresh}", "fresh push"},
	} {
		if v, ok := perf.stat(row.metric, "p(95)"); ok {
			push.bars = append(push.bars, bar{label: row.label, value: fmtMs(v), frac: v, accent: true})
		}
	}

	served := panel{
		title: "Requests served per run",
		scale: "requests, " + betterIs(perf, "http_reqs", "higher is better"),
	}
	for _, row := range []struct {
		label string
		src   *baseline
	}{
		{"throughput suite", perf},
		{"pressure suite", pressure},
	} {
		if v, ok := row.src.stat("http_reqs", "count"); ok {
			served.bars = append(served.bars, bar{label: row.label, value: fmtCount(v), frac: v, accent: true})
		}
	}

	c := chart{
		title:    "xilo under the k6 gate",
		subtitle: k6Subtitle(perf),
		footnote: "Each panel has its own scale; every bar carries the measured value.",
		cols:     1,
	}
	for _, p := range []panel{read, push, served} {
		if len(p.bars) > 0 {
			c.panels = append(c.panels, p)
		}
	}
	if len(c.panels) == 0 {
		return chart{}, fmt.Errorf("%s: baselines held none of the charted metrics", dir)
	}
	return c, nil
}

// k6Subtitle says where the numbers come from, including how many CI runs the
// baseline was distilled from when the file records it: a gate built from one
// run and a gate built from a dozen are not the same claim.
func k6Subtitle(perf *baseline) string {
	base := "Committed baselines from tests/k6/baselines, measured on 2-core GitHub runners and re-checked on every push"
	if perf != nil && perf.SampledRuns > 1 {
		return fmt.Sprintf("%s. Worst of %d sampled runs, plus each metric's tolerance.", base, perf.SampledRuns)
	}
	return base + "."
}

// targetOrder fixes the bar order so a redraw does not reshuffle the chart,
// and names each implementation as its project does.
var targetOrder = []struct{ id, name string }{
	{"xilo", "xilo"},
	{"attic", "attic"},
	{"harmonia", "harmonia"},
	{"nixserve", "nix-serve-ng"},
	{"s3", "MinIO + nix copy"},
	{"garage", "Garage + nix copy"},
}

// benchCharts builds the cross-implementation comparison: one panel per
// measured quantity, because the quantities share neither unit nor direction.
func benchCharts(path string) (chart, error) {
	r, err := loadBench(path)
	if err != nil {
		return chart{}, err
	}

	// A spec reads its own number, because two of the panels are ratios rather
	// than a field: what a server costs is only legible against what it served.
	type spec struct {
		title, scale string
		value        func(target string) (float64, bool)
		format       func(float64) string
		// comparableOnly drops targets whose bytes are not the same quantity
		// (a bucket serving compressed NARs is not serving what xilo serves).
		comparableOnly bool
	}
	key := func(name string) func(string) (float64, bool) {
		return func(target string) (float64, bool) { return r.num(target, name) }
	}
	// Raw CPU percentage rewards the server that served least, so the panel is
	// CPU spent per MB/s delivered.
	cpuPerMBs := func(target string) (float64, bool) {
		cpu, ok1 := r.num(target, "mean_cpu_pct")
		mbs, ok2 := r.num(target, "pull_mbs")
		if !ok1 || !ok2 || mbs <= 0 {
			return 0, false
		}
		return cpu / mbs, true
	}
	// Likewise for memory: xilo holding less RAM while serving more bytes is
	// the comparison, and the ratio states it without a caption.
	specs := []spec{
		{"narinfo throughput", "requests/s, higher is better", key("narinfo_qps"), fmtQPS, false},
		{"NAR pull throughput", "MB/s, higher is better", key("pull_mbs"), fmtMBs, true},
		{"Memory under pull load", "peak RAM in use, lower is better", key("max_rss_mib"), fmtMiB, false},
		{"CPU per MB/s served", "% of a core per MB/s, lower is better", cpuPerMBs, fmtRatio, true},
		{"Cold push of the closure", "seconds, lower is better", key("cold_push_s"), fmtSec, false},
		{"Repeat push of the closure", "seconds, lower is better", key("repeat_push_s"), fmtSec, false},
		{"Stored on disk", "bytes for this closure; a host store keeps it unpacked", key("stored_bytes"), fmtBytes, false},
		{"Image or closure to deploy", "bytes, lower is better", key("image_bytes"), fmtBytes, false},
	}

	c := chart{
		title: "xilo against other self-hostable Nix caches",
		cols:  2,
	}
	workload := ""
	if p, ok := r.Workload["paths"].(float64); ok {
		workload = fmtCount(p) + " paths"
		if b, ok := r.Workload["nar_bytes"].(float64); ok && b > 0 {
			workload += fmt.Sprintf(", %s of NAR", fmtBytes(b))
		}
	}
	c.subtitle = strings.TrimSpace(fmt.Sprintf("Same machine, same closure (%s), same load script. %s, %s.",
		workload, r.Runner, r.Date))
	c.footnote = "Each panel has its own scale. A missing bar is a target that does not do that thing, not a zero."

	for _, s := range specs {
		p := panel{title: s.title, scale: s.scale}
		for _, t := range targetOrder {
			if s.comparableOnly && r.str(t.id, "nar_comparable") == "false" {
				continue
			}
			v, ok := s.value(t.id)
			if !ok {
				continue
			}
			p.bars = append(p.bars, bar{
				label:  t.name,
				value:  s.format(v),
				frac:   v,
				accent: t.id == "xilo",
			})
		}
		if len(p.bars) > 1 {
			c.panels = append(c.panels, p)
		}
	}
	if len(c.panels) == 0 {
		return chart{}, fmt.Errorf("%s: nothing comparable across targets", path)
	}
	return c, nil
}

// ── rendering ───────────────────────────────────────────────────────────────

type theme struct {
	name, bg, fg, muted, grid, accent, other string
}

// Literal hex, and a pair that was checked on both grounds: an SVG loaded as
// an image resolves neither var() nor a host page's tokens.
var themes = []theme{
	{"light", "#ffffff", "#0f172a", "#64748b", "#e5e9f0", "#b45309", "#94a3b8"},
	{"dark", "#0d1117", "#e6edf3", "#8b949e", "#30363d", "#f0b429", "#6e7681"},
}

const (
	width     = 900
	padX      = 24
	padY      = 22
	labelW    = 170
	valueW    = 108
	rowH      = 26
	barH      = 13
	panelHead = 34
	panelGap  = 18
	colGap    = 24
)

const (
	sansFont = "ui-sans-serif,-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,sans-serif"
	monoFont = "ui-monospace,SFMono-Regular,Menlo,Consolas,'Liberation Mono',monospace"
)

func writeChart(dir, name string, c chart) error {
	for _, th := range themes {
		path := filepath.Join(dir, fmt.Sprintf("%s-%s.svg", name, th.name))
		if err := os.WriteFile(path, []byte(render(c, th)), 0o644); err != nil {
			return err
		}
		fmt.Println("wrote", path)
	}
	return nil
}

// panelHeight is the drawn height of one panel.
func panelHeight(p panel) float64 {
	return panelHead + float64(len(p.bars))*rowH + 6
}

func render(c chart, th theme) string {
	cols := max(c.cols, 1)
	panelW := (width - 2*padX - float64(cols-1)*colGap) / float64(cols)

	// Row-major layout, each grid row as tall as its tallest panel, so a panel
	// with two bars beside one with eight does not stretch to match it.
	type placed struct {
		p    panel
		x, y float64
		w    float64
	}
	var out []placed
	y := padY + 46.0
	if c.subtitle == "" {
		y = padY + 30
	}
	for i := 0; i < len(c.panels); i += cols {
		rowPanels := c.panels[i:min(i+cols, len(c.panels))]
		rowH := 0.0
		for _, p := range rowPanels {
			rowH = math.Max(rowH, panelHeight(p))
		}
		for j, p := range rowPanels {
			out = append(out, placed{
				p: p,
				x: padX + float64(j)*(panelW+colGap),
				y: y,
				w: panelW,
			})
		}
		y += rowH + panelGap
	}
	height := y + 10
	if c.footnote != "" {
		height += 18
	}

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %.0f" width="%d" height="%.0f" role="img" aria-label="%s">`,
		width, height, width, height, html.EscapeString(c.title))
	fmt.Fprintf(&b, `<title>%s</title>`, html.EscapeString(c.title))
	fmt.Fprintf(&b, `<rect width="100%%" height="100%%" fill="%s"/>`, th.bg)
	fmt.Fprintf(&b, `<text x="%d" y="%d" fill="%s" font-family="%s" font-size="16" font-weight="600">%s</text>`,
		padX, padY+8, th.fg, sansFont, html.EscapeString(c.title))
	if c.subtitle != "" {
		fmt.Fprintf(&b, `<text x="%d" y="%d" fill="%s" font-family="%s" font-size="11.5">%s</text>`,
			padX, padY+28, th.muted, sansFont, html.EscapeString(c.subtitle))
	}

	for _, pl := range out {
		renderPanel(&b, pl.p, pl.x, pl.y, pl.w, th)
	}

	if c.footnote != "" {
		fmt.Fprintf(&b, `<text x="%d" y="%.0f" fill="%s" font-family="%s" font-size="10.5">%s</text>`,
			padX, height-8, th.muted, sansFont, html.EscapeString(c.footnote))
	}
	b.WriteString(`</svg>`)
	b.WriteString("\n")
	return b.String()
}

func renderPanel(b *strings.Builder, p panel, x, y, w float64, th theme) {
	fmt.Fprintf(b, `<text x="%.0f" y="%.0f" fill="%s" font-family="%s" font-size="12.5" font-weight="600">%s</text>`,
		x, y+12, th.fg, sansFont, html.EscapeString(p.title))
	if p.scale != "" {
		fmt.Fprintf(b, `<text x="%.0f" y="%.0f" fill="%s" font-family="%s" font-size="10.5">%s</text>`,
			x, y+27, th.muted, sansFont, html.EscapeString(p.scale))
	}

	trackX := x + labelW
	trackW := w - labelW - valueW
	if trackW < 40 {
		trackW = 40
	}
	scale := chartMax(maxFrac(p.bars))

	// One recessive rule at the axis top, and nothing else behind the bars:
	// a grid on eight bars of one series is furniture.
	fmt.Fprintf(b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="%s" stroke-width="1"/>`,
		trackX, y+panelHead-6, trackX+trackW, y+panelHead-6, th.grid)

	for i, bar := range p.bars {
		rowY := y + panelHead + float64(i)*rowH
		fill := th.other
		if bar.accent {
			fill = th.accent
		}
		length := 2.0
		if scale > 0 {
			length = math.Max(2, bar.frac/scale*trackW)
		}
		fmt.Fprintf(b, `<text x="%.0f" y="%.1f" fill="%s" font-family="%s" font-size="11.5">%s</text>`,
			x, rowY+barH-1, th.fg, sansFont, html.EscapeString(bar.label))
		fmt.Fprintf(b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%d" rx="2" fill="%s"/>`,
			trackX, rowY+1, length, barH, fill)
		fmt.Fprintf(b, `<text x="%.1f" y="%.1f" fill="%s" font-family="%s" font-size="11">%s</text>`,
			trackX+length+8, rowY+barH-1, th.fg, monoFont, html.EscapeString(bar.value))
	}
}

func maxFrac(bars []bar) float64 {
	m := 0.0
	for _, b := range bars {
		m = math.Max(m, b.frac)
	}
	return m
}

// chartMax rounds a maximum up to the next 1, 2 or 5 times a power of ten, so
// bar lengths are read against a round number rather than against whichever
// value happened to be largest.
func chartMax(v float64) float64 {
	if v <= 0 {
		return 1
	}
	exp := math.Floor(math.Log10(v))
	base := math.Pow(10, exp)
	for _, step := range []float64{1, 2, 5, 10} {
		if v <= step*base {
			return step * base
		}
	}
	return 10 * base
}

// ── number formatting ───────────────────────────────────────────────────────

func fmtMs(v float64) string {
	if v < 10 {
		return fmt.Sprintf("%.1f ms", v)
	}
	if v < 1000 {
		return fmt.Sprintf("%.0f ms", v)
	}
	return fmt.Sprintf("%.2f s", v/1000)
}

func fmtSec(v float64) string { return fmt.Sprintf("%.1f s", v) }

func fmtQPS(v float64) string { return fmtCount(v) + "/s" }

func fmtMBs(v float64) string { return fmt.Sprintf("%.0f MB/s", v) }

func fmtMiB(v float64) string { return fmt.Sprintf("%.0f MiB", v) }

func fmtRatio(v float64) string {
	if v < 10 {
		return fmt.Sprintf("%.2f", v)
	}
	return fmt.Sprintf("%.1f", v)
}

func fmtBytes(v float64) string {
	switch {
	case v >= 1e9:
		return fmt.Sprintf("%.2f GB", v/1e9)
	case v >= 1e6:
		return fmt.Sprintf("%.0f MB", v/1e6)
	case v >= 1e3:
		return fmt.Sprintf("%.0f kB", v/1e3)
	default:
		return fmt.Sprintf("%.0f B", v)
	}
}

// fmtCount groups thousands, because these are read as figures in a column.
func fmtCount(v float64) string {
	s := strconv.FormatInt(int64(math.Round(v)), 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var parts []string
	for len(s) > 3 {
		parts = append([]string{s[len(s)-3:]}, parts...)
		s = s[:len(s)-3]
	}
	parts = append([]string{s}, parts...)
	out := strings.Join(parts, ",")
	if neg {
		return "-" + out
	}
	return out
}
