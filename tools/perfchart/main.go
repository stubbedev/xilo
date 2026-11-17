// Command perfchart draws the committed performance numbers as bar charts: the
// k6 gate (tests/k6/baselines/*.json) and the cross-implementation comparison
// (tests/bench/results.json). Each chart is an SVG per GitHub theme in
// docs/perf, and the region between <!-- perfchart:NAME --> and
// <!-- /perfchart:NAME --> in README.md and docs/benchmarks.md is rewritten to
// show it, with the same numbers as a table.
//
// It lives outside cmd/xilo for the same reason schemagen does: nothing in a
// user's binary needs to draw a chart.
//
// Every number drawn comes from a committed file. The tool formats labels and
// invents nothing: a metric missing from the input is a missing bar, never a
// zero.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"html"
	"maps"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var documents = []string{"README.md", "docs/benchmarks.md"}

const chartDir = "docs/perf"

func main() {
	k6Dir := flag.String("k6", "tests/k6/baselines", "directory of committed k6 baselines")
	benchFile := flag.String("bench", "tests/bench/results.json", "output of tests/bench/bench.sh")
	verify := flag.Bool("check", false, "fail if a chart or document differs from what would be drawn, instead of writing")
	flag.Parse()

	if err := run(*k6Dir, *benchFile, chartDir, documents, *verify); err != nil {
		fmt.Fprintln(os.Stderr, "perfchart:", err)
		os.Exit(1)
	}
}

func run(k6Dir, benchFile, outDir string, docs []string, verify bool) error {
	charts := map[string]chart{}
	if c, err := k6Charts(k6Dir); err != nil {
		fmt.Fprintln(os.Stderr, "perfchart: leaving the k6 chart as it is:", err)
	} else {
		charts["k6-gate"] = c
	}
	if c, err := benchCharts(benchFile); err != nil {
		fmt.Fprintln(os.Stderr, "perfchart: leaving the comparison chart as it is:", err)
	} else {
		charts["compare"] = c
	}
	if len(charts) == 0 {
		return fmt.Errorf("no inputs found (looked for %s and %s)", k6Dir, benchFile)
	}

	type rewrite struct{ path, contents string }
	var rewrites []rewrite
	stage := func(path, contents string) error {
		before, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("read %s: %w", path, err)
		}
		if string(before) != contents {
			rewrites = append(rewrites, rewrite{path, contents})
		}
		return nil
	}
	for _, name := range slices.Sorted(maps.Keys(charts)) {
		for _, th := range themes {
			if err := stage(filepath.Join(outDir, svgName(name, th)), charts[name].svg(th)); err != nil {
				return err
			}
		}
	}
	placed := map[string]bool{}
	for _, path := range docs {
		before, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		rel, err := filepath.Rel(filepath.Dir(path), outDir)
		if err != nil {
			return fmt.Errorf("%s: chart directory %s: %w", path, outDir, err)
		}
		after, names, err := fill(string(before), filepath.ToSlash(rel), charts)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		for _, n := range names {
			placed[n] = true
		}
		if err := stage(path, after); err != nil {
			return err
		}
	}
	for name := range charts {
		if !placed[name] {
			return fmt.Errorf("no document has a <!-- perfchart:%s --> region to draw into", name)
		}
	}
	if verify {
		if len(rewrites) == 0 {
			return nil
		}
		stale := make([]string, len(rewrites))
		for i, r := range rewrites {
			stale[i] = r.path
		}
		return fmt.Errorf("charts are stale, run 'just perf-charts' and commit: %s", strings.Join(stale, ", "))
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", outDir, err)
	}
	for _, r := range rewrites {
		if err := writeAtomic(r.path, r.contents); err != nil {
			return err
		}
		fmt.Println("wrote", r.path)
	}
	return nil
}

var region = regexp.MustCompile(`(?s)<!-- perfchart:([a-z0-9-]+) -->\n.*?<!-- /perfchart:([a-z0-9-]+) -->`)

func fill(doc, chartPath string, charts map[string]chart) (string, []string, error) {
	var names []string
	var failure error
	out := region.ReplaceAllStringFunc(doc, func(m string) string {
		sub := region.FindStringSubmatch(m)
		open, closing := sub[1], sub[2]
		if open != closing {
			failure = fmt.Errorf("region perfchart:%s is closed by /perfchart:%s", open, closing)
			return m
		}
		names = append(names, open)
		c, ok := charts[open]
		if !ok {
			return m
		}
		return "<!-- perfchart:" + open + " -->\n" + c.markdown(open, chartPath) + "<!-- /perfchart:" + open + " -->"
	})
	if failure != nil {
		return "", nil, failure
	}
	for name := range charts {
		if strings.Contains(doc, "<!-- perfchart:"+name+" -->") && !slices.Contains(names, name) {
			return "", nil, fmt.Errorf("region perfchart:%s has no closing <!-- /perfchart:%s -->", name, name)
		}
	}
	return out, names, nil
}

func writeAtomic(path, contents string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("stage %s: %w", path, err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(contents); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", tmp.Name(), err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmp.Name(), err)
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return fmt.Errorf("chmod %s: %w", tmp.Name(), err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("move %s into place: %w", path, err)
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
	label     string
	value     string
	amount    float64
	highlight bool
}

type direction int

const (
	noDirection direction = iota
	higherIsBetter
	lowerIsBetter
)

func (d direction) phrase() string {
	switch d {
	case higherIsBetter:
		return "higher is better"
	case lowerIsBetter:
		return "lower is better"
	}
	return ""
}

func directionOf(s string) (direction, bool) {
	switch s {
	case "higher":
		return higherIsBetter, true
	case "lower":
		return lowerIsBetter, true
	}
	return noDirection, false
}

type panel struct {
	title  string
	unit   string
	better direction
	note   string
	bars   []bar
}

func (p panel) scale() string {
	if phrase := p.better.phrase(); phrase != "" {
		return p.unit + ", " + phrase
	}
	return p.unit
}

type tableLayout int

const (
	tableByTarget tableLayout = iota
	tableByRow
)

type chart struct {
	alt      string
	subtitle string
	footnote string
	panels   []panel
	table    tableLayout
}

// k6Charts builds the gate chart from the committed baselines: the numbers CI
// measures on every push and refuses to let regress.
func k6Charts(dir string) (chart, error) {
	perf, err := loadBaseline(filepath.Join(dir, "perf.json"))
	if err != nil {
		return chart{}, err
	}
	pressure, _ := loadBaseline(filepath.Join(dir, "pressure.json"))

	betterIs := func(b *baseline, metric string, fallback direction) direction {
		if d, ok := directionOf(b.direction(metric)); ok {
			return d
		}
		return fallback
	}
	read := panel{title: "Read latency, p95", unit: "ms", better: betterIs(perf, "http_req_duration{scenario:narinfo_hit}", lowerIsBetter)}
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
			read.bars = append(read.bars, bar{label: row.label, value: fmtMs(v), amount: v, highlight: true})
		}
	}

	push := panel{
		title:  "Push round trip, p95",
		unit:   "ms",
		better: betterIs(perf, "iteration_duration{scenario:push_fresh}", lowerIsBetter),
	}
	for _, row := range []struct{ metric, label string }{
		{"iteration_duration{scenario:push_dedup}", "repeat push (deduped)"},
		{"iteration_duration{scenario:push_fresh}", "fresh push"},
	} {
		if v, ok := perf.stat(row.metric, "p(95)"); ok {
			push.bars = append(push.bars, bar{label: row.label, value: fmtMs(v), amount: v, highlight: true})
		}
	}

	served := panel{
		title:  "Requests served per run",
		unit:   "requests",
		better: betterIs(perf, "http_reqs", higherIsBetter),
	}
	for _, row := range []struct {
		label string
		src   *baseline
	}{
		{"throughput suite", perf},
		{"pressure suite", pressure},
	} {
		if v, ok := row.src.stat("http_reqs", "count"); ok {
			served.bars = append(served.bars, bar{label: row.label, value: fmtCount(v), amount: v, highlight: true})
		}
	}

	c := chart{
		alt:      "Bar charts of xilo's k6 performance gate: read latency, push round trip and requests served. The table below holds every number.",
		subtitle: k6Subtitle(perf),
		footnote: "Each chart has its own scale; every bar carries the measured value.",
		table:    tableByRow,
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
		title, unit string
		better      direction
		note        string
		value       func(target string) (float64, bool)
		format      func(float64) string
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
		{"narinfo throughput", "requests/s", higherIsBetter, "", key("narinfo_qps"), fmtQPS, false},
		{"NAR pull throughput", "MB/s", higherIsBetter, "", key("pull_mbs"), fmtMBs, true},
		{"Memory under pull load", "peak RAM in use", lowerIsBetter, "", key("max_rss_mib"), fmtMiB, false},
		{"CPU per MB/s served", "% of a core per MB/s", lowerIsBetter, "", cpuPerMBs, fmtRatio, true},
		{"Cold push of the closure", "seconds", lowerIsBetter, "", key("cold_push_s"), fmtSec, false},
		{"Repeat push of the closure", "seconds", lowerIsBetter, "", key("repeat_push_s"), fmtSec, false},
		{"Stored on disk", "bytes for this closure", noDirection, "a host store keeps the closure unpacked, a cache stores it compressed", key("stored_bytes"), fmtBytes, false},
		{"Image or closure to deploy", "bytes", lowerIsBetter, "", key("image_bytes"), fmtBytes, false},
	}

	c := chart{table: tableByTarget}
	workload := ""
	if p, ok := r.Workload["paths"].(float64); ok {
		workload = fmtCount(p) + " paths"
		if b, ok := r.Workload["nar_bytes"].(float64); ok && b > 0 {
			workload += fmt.Sprintf(", %s of NAR", fmtBytes(b))
		}
	}
	c.subtitle = strings.TrimSpace(fmt.Sprintf("Same machine, same closure (%s), same load script. %s, %s.",
		workload, r.Runner, r.Date))
	c.footnote = "Each chart has its own scale. A missing bar is a target that does not do that thing, not a zero."

	for _, s := range specs {
		p := panel{title: s.title, unit: s.unit, better: s.better, note: s.note}
		for _, t := range targetOrder {
			if s.comparableOnly && r.str(t.id, "nar_comparable") == "false" {
				continue
			}
			v, ok := s.value(t.id)
			if !ok {
				continue
			}
			p.bars = append(p.bars, bar{
				label:     t.name,
				value:     s.format(v),
				amount:    v,
				highlight: t.id == "xilo",
			})
		}
		if len(p.bars) > 1 {
			c.panels = append(c.panels, p)
		}
	}
	if len(c.panels) == 0 {
		return chart{}, fmt.Errorf("%s: nothing comparable across targets", path)
	}
	c.alt = fmt.Sprintf("Bar charts comparing xilo with other self-hostable Nix caches on %d measures, xilo highlighted. The table below holds every number.", len(c.panels))
	return c, nil
}

// ── rendering ───────────────────────────────────────────────────────────────

type theme struct {
	name, highlight, other, text, muted, rule string
}

var themes = []theme{
	{"light", "#7287fd", "#7c7f93", "#4c4f69", "#6c6f85", "#ccd0da"},
	{"dark", "#7287fd", "#6c7086", "#cdd6f4", "#a6adc8", "#313244"},
}

const (
	svgWidth   = 880.0
	gridCols   = 2
	colGap     = 48.0
	rowGap     = 28.0
	panelHead  = 46.0
	rowHeight  = 28.0
	barHeight  = 14.0
	barRadius  = 3.0
	labelSize  = 13.0
	valueSize  = 12.5
	labelGap   = 12.0
	valueGap   = 8.0
	sansFont   = "-apple-system,BlinkMacSystemFont,'Segoe UI','Noto Sans',Helvetica,Arial,sans-serif"
	tabularNum = "font-variant-numeric:tabular-nums"
)

func svgName(chart string, th theme) string { return chart + "-" + th.name + ".svg" }

func (c chart) markdown(name, chartPath string) string {
	var b strings.Builder
	if c.subtitle != "" {
		b.WriteString(c.subtitle + "\n\n")
	}
	themed := map[string]string{}
	for _, th := range themes {
		themed[th.name] = chartPath + "/" + svgName(name, th)
	}
	fmt.Fprintf(&b, "<picture>\n  <source media=\"(prefers-color-scheme: dark)\" srcset=\"%s\">\n  <img alt=\"%s\" src=\"%s\">\n</picture>\n\n",
		themed["dark"], html.EscapeString(c.alt), themed["light"])
	if c.footnote != "" {
		b.WriteString(c.footnote + "\n\n")
	}
	for _, p := range c.panels {
		if p.note != "" {
			b.WriteString(p.title + ": " + p.note + ".\n\n")
		}
	}
	b.WriteString("<details>\n<summary>The same numbers as a table</summary>\n\n")
	switch c.table {
	case tableByTarget:
		b.WriteString(c.targetTable())
	case tableByRow:
		b.WriteString(c.rowTable())
	}
	b.WriteString("\n</details>\n")
	return b.String()
}

func textWidth(s string, size float64) float64 {
	w := 0.0
	for _, r := range s {
		switch {
		case strings.ContainsRune("il.,:;|' ", r):
			w += 0.3
		case strings.ContainsRune("mwMW", r):
			w += 0.85
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			w += 0.62
		default:
			w += 0.54
		}
	}
	return w * size
}

func (c chart) columns() (labelW, valueW float64) {
	for _, p := range c.panels {
		for _, b := range p.bars {
			labelW = math.Max(labelW, textWidth(b.label, labelSize))
			valueW = math.Max(valueW, textWidth(b.value, valueSize))
		}
	}
	return math.Ceil(labelW) + labelGap, math.Ceil(valueW) + valueGap
}

func panelHeight(p panel) float64 { return panelHead + float64(len(p.bars))*rowHeight }

func (c chart) svg(th theme) string {
	labelW, valueW := c.columns()
	panelW := (svgWidth - colGap*(gridCols-1)) / gridCols
	trackW := panelW - labelW - valueW

	var body strings.Builder
	y := 0.0
	for row := range slices.Chunk(c.panels, gridCols) {
		height := 0.0
		for i, p := range row {
			p.render(&body, float64(i)*(panelW+colGap), y, labelW, trackW, th)
			height = math.Max(height, panelHeight(p))
		}
		y += height + rowGap
	}
	height := y - rowGap

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %s %s" width="%s" height="%s" role="img" aria-labelledby="t"><title id="t">%s</title>`,
		num(svgWidth), num(height), num(svgWidth), num(height), html.EscapeString(c.alt))
	fmt.Fprintf(&b, `<g font-family="%s">`, sansFont)
	b.WriteString(body.String())
	b.WriteString("</g></svg>\n")
	return b.String()
}

func (p panel) render(b *strings.Builder, x, y, labelW, trackW float64, th theme) {
	fmt.Fprintf(b, `<text x="%s" y="%s" fill="%s" font-size="15" font-weight="600">%s</text>`,
		num(x), num(y+15), th.text, html.EscapeString(p.title))
	fmt.Fprintf(b, `<text x="%s" y="%s" fill="%s" font-size="12.5">%s</text>`,
		num(x), num(y+33), th.muted, html.EscapeString(p.scale()))

	mixed := slices.ContainsFunc(p.bars, func(b bar) bool { return b.highlight }) &&
		slices.ContainsFunc(p.bars, func(b bar) bool { return !b.highlight })
	full := 0.0
	for _, bar := range p.bars {
		full = math.Max(full, bar.amount)
	}
	axisX := x + labelW
	rowsTop := y + panelHead
	fmt.Fprintf(b, `<line x1="%s" y1="%s" x2="%s" y2="%s" stroke="%s" stroke-width="1"/>`,
		num(axisX), num(rowsTop-4), num(axisX), num(rowsTop+float64(len(p.bars))*rowHeight-10), th.rule)
	for i, bar := range p.bars {
		mid := rowsTop + float64(i)*rowHeight + barHeight/2
		fill, weight := th.other, "400"
		if bar.highlight {
			fill = th.highlight
			if mixed {
				weight = "600"
			}
		}
		fmt.Fprintf(b, `<text x="%s" y="%s" fill="%s" font-size="%s" font-weight="%s" text-anchor="end" dominant-baseline="central">%s</text>`,
			num(axisX-labelGap), num(mid), th.text, num(labelSize), weight, html.EscapeString(bar.label))
		length := barRadius
		if full > 0 {
			length = math.Max(barRadius, bar.amount/full*trackW)
		}
		fmt.Fprintf(b, `<path d="%s" fill="%s"/>`, barPath(axisX, mid-barHeight/2, length), fill)
		fmt.Fprintf(b, `<text x="%s" y="%s" fill="%s" font-size="%s" dominant-baseline="central" style="%s">%s</text>`,
			num(axisX+length+valueGap), num(mid), th.text, num(valueSize), tabularNum, html.EscapeString(bar.value))
	}
}

func barPath(x, y, length float64) string {
	r := math.Min(barRadius, length)
	return fmt.Sprintf("M%s %sH%sA%s %s 0 0 1 %s %sV%sA%s %s 0 0 1 %s %sH%sZ",
		num(x), num(y), num(x+length-r), num(r), num(r), num(x+length), num(y+r),
		num(y+barHeight-r), num(r), num(r), num(x+length-r), num(y+barHeight), num(x))
}

func num(v float64) string { return strconv.FormatFloat(math.Round(v*10)/10, 'f', -1, 64) }

func (p panel) best() (bar, bool) {
	if p.better == noDirection || len(p.bars) < 2 {
		return bar{}, false
	}
	best := p.bars[0]
	for _, b := range p.bars[1:] {
		if (p.better == higherIsBetter && b.amount > best.amount) || (p.better == lowerIsBetter && b.amount < best.amount) {
			best = b
		}
	}
	return best, true
}

func (p panel) cell(b bar) string {
	if best, ok := p.best(); ok && b.value == best.value {
		return "**" + b.value + "**"
	}
	return b.value
}

func (c chart) targetTable() string {
	var targets []string
	for _, p := range c.panels {
		for _, b := range p.bars {
			if !slices.Contains(targets, b.label) {
				targets = append(targets, b.label)
			}
		}
	}
	var t strings.Builder
	t.WriteString("| Measure | " + strings.Join(targets, " | ") + " |\n")
	t.WriteString("|---|" + strings.Repeat("--:|", len(targets)) + "\n")
	for _, p := range c.panels {
		cells := make([]string, len(targets))
		for i, target := range targets {
			cells[i] = "—"
			for _, b := range p.bars {
				if b.label == target {
					cells[i] = p.cell(b)
				}
			}
		}
		t.WriteString("| " + tableText(p.title+" ("+p.scale()+")") + " | " + strings.Join(cells, " | ") + " |\n")
	}
	t.WriteString("\nBold is the best value of a measure that has a better direction; — is a target that does not do that thing.\n")
	return t.String()
}

func (c chart) rowTable() string {
	var t strings.Builder
	t.WriteString("| Measure | Scenario | Value |\n|---|---|--:|\n")
	for _, p := range c.panels {
		for _, b := range p.bars {
			t.WriteString("| " + tableText(p.title+" ("+p.scale()+")") + " | " + tableText(b.label) + " | " + b.value + " |\n")
		}
	}
	return t.String()
}

func tableText(s string) string { return strings.ReplaceAll(s, "|", "\\|") }

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
