// Command perfchart renders the committed performance numbers as Mermaid bar
// charts inside the markdown that shows them: the k6 gate
// (tests/k6/baselines/*.json) and the cross-implementation comparison
// (tests/bench/results.json). Each chart replaces the region between
// <!-- perfchart:NAME --> and <!-- /perfchart:NAME --> in README.md and
// docs/benchmarks.md, so GitHub draws it natively in whichever theme the
// reader has, instead of embedding a picture of one.
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
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var documents = []string{"README.md", "docs/benchmarks.md"}

func main() {
	k6Dir := flag.String("k6", "tests/k6/baselines", "directory of committed k6 baselines")
	benchFile := flag.String("bench", "tests/bench/results.json", "output of tests/bench/bench.sh")
	verify := flag.Bool("check", false, "fail if a document's charts differ from what would be drawn, instead of writing")
	flag.Parse()

	if err := run(*k6Dir, *benchFile, documents, *verify); err != nil {
		fmt.Fprintln(os.Stderr, "perfchart:", err)
		os.Exit(1)
	}
}

func run(k6Dir, benchFile string, docs []string, verify bool) error {
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
	placed := map[string]bool{}
	for _, path := range docs {
		before, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		after, names, err := fill(string(before), charts)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		for _, n := range names {
			placed[n] = true
		}
		if after != string(before) {
			rewrites = append(rewrites, rewrite{path, after})
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
	for _, r := range rewrites {
		if err := writeAtomic(r.path, r.contents); err != nil {
			return err
		}
		fmt.Println("wrote", r.path)
	}
	return nil
}

var region = regexp.MustCompile(`(?s)<!-- perfchart:([a-z0-9-]+) -->\n.*?<!-- /perfchart:([a-z0-9-]+) -->`)

func fill(doc string, charts map[string]chart) (string, []string, error) {
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
		return "<!-- perfchart:" + open + " -->\n" + c.markdown() + "<!-- /perfchart:" + open + " -->"
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
	label  string
	value  string
	amount float64
}

type panel struct {
	title string
	scale string // what the panel's full width means
	bars  []bar
}

type chart struct {
	subtitle string
	footnote string
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
			read.bars = append(read.bars, bar{label: row.label, value: fmtMs(v), amount: v})
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
			push.bars = append(push.bars, bar{label: row.label, value: fmtMs(v), amount: v})
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
			served.bars = append(served.bars, bar{label: row.label, value: fmtCount(v), amount: v})
		}
	}

	c := chart{
		subtitle: k6Subtitle(perf),
		footnote: "Each chart has its own scale; every bar carries the measured value.",
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

	var c chart
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
				amount: v,
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

// Catppuccin, picked to read on both of GitHub's themes, since one Mermaid
// block cannot switch palettes: Latte's mauve for the bars, and Latte's
// overlay greys for text and rules, which hold above 4:1 on white and on
// GitHub's dark ground alike.
const (
	barColor  = "#8839ef"
	textColor = "#7c7f93"
	ruleColor = "#9ca0b0"
)

func (c chart) markdown() string {
	var b strings.Builder
	if c.subtitle != "" {
		b.WriteString(c.subtitle + "\n\n")
	}
	for _, p := range c.panels {
		b.WriteString(p.mermaid())
		b.WriteString("\n")
	}
	if c.footnote != "" {
		b.WriteString(c.footnote + "\n")
	}
	return b.String()
}

func (p panel) mermaid() string {
	labels := make([]string, len(p.bars))
	amounts := make([]string, len(p.bars))
	top := 0.0
	for i, bar := range p.bars {
		labels[i] = quote(bar.label + "  " + bar.value)
		amounts[i] = strconv.FormatFloat(bar.amount, 'f', -1, 64)
		top = math.Max(top, bar.amount)
	}
	init := fmt.Sprintf(`%%%%{init: {"theme": "base", "themeVariables": {"xyChart": {"backgroundColor": "transparent", "titleColor": "%[1]s", "xAxisLabelColor": "%[1]s", "xAxisTitleColor": "%[1]s", "xAxisLineColor": "%[2]s", "yAxisTitleColor": "%[1]s", "yAxisLineColor": "%[2]s", "plotColorPalette": "%[3]s"}}, "xyChart": {"height": %[4]d, "yAxis": {"showLabel": false, "showTick": false}, "xAxis": {"showTick": false}}}}%%%%`,
		textColor, ruleColor, barColor, 80+40*len(p.bars))
	return fmt.Sprintf("```mermaid\n%s\nxychart-beta horizontal\n    title %s\n    x-axis [%s]\n    y-axis %s 0 --> %s\n    bar [%s]\n```\n",
		init, quote(p.title), strings.Join(labels, ", "), quote(p.scale),
		strconv.FormatFloat(chartMax(top), 'f', -1, 64), strings.Join(amounts, ", "))
}

func quote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, "'") + `"`
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
