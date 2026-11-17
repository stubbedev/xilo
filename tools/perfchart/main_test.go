package main

import (
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const perfFixture = `{
  "suite": "perf",
  "sampled_runs": 12,
  "metrics": {
    "http_req_duration{scenario:narinfo_hit}": {"p(95)": 4.8, "tolerance": 0.6, "direction": "lower", "samples": 12},
    "http_req_duration{scenario:pull_big}": {"p(95)": 64.8, "tolerance": 0.6, "direction": "lower", "samples": 12},
    "iteration_duration{scenario:push_fresh}": {"p(95)": 1091.2, "tolerance": 0.6, "direction": "lower", "samples": 12},
    "http_reqs": {"count": 579388, "tolerance": 0.35, "direction": "higher", "samples": 12}
  }
}`

const pressureFixture = `{
  "suite": "pressure",
  "sampled_runs": 12,
  "metrics": {
    "http_req_duration{scenario:storm}": {"p(95)": 45.0, "tolerance": 0.6, "direction": "lower", "samples": 12},
    "http_reqs": {"count": 502926, "tolerance": 0.35, "direction": "higher", "samples": 12}
  }
}`

const benchFixture = `{
  "date": "2026-09-08",
  "runner": "test runner, 2 cores",
  "workload": {"paths": 91, "megabytes": 1024},
  "targets": {
    "xilo":     {"status": "ok", "narinfo_qps": 5981, "pull_mbs": 695, "max_rss_mib": 108, "mean_cpu_pct": 347.5, "stored_bytes": 380000000, "nar_comparable": "true"},
    "attic":    {"status": "ok", "narinfo_qps": 1887, "pull_mbs": 216, "max_rss_mib": 197, "mean_cpu_pct": 216.0, "stored_bytes": 520000000, "nar_comparable": "true"},
    "harmonia": {"status": "ok", "narinfo_qps": 2500, "pull_mbs": 300, "max_rss_mib": 70, "nar_comparable": "true"},
    "nixserve": {"status": "ok", "narinfo_qps": 3100, "pull_mbs": 800, "max_rss_mib": 60, "nar_comparable": "true"},
    "s3":       {"status": "ok", "narinfo_qps": 2400, "pull_mbs": 950, "max_rss_mib": 240, "stored_bytes": 300000000, "nar_comparable": "false"},
    "garage":   {"status": "ok", "narinfo_qps": 2100, "pull_mbs": 880, "max_rss_mib": 90, "stored_bytes": 310000000, "nar_comparable": "false"},
    "broken":   {"status": "skipped", "error": "would not start"}
  }
}`

func writeFixtures(t *testing.T) (k6Dir, benchFile string) {
	t.Helper()
	dir := t.TempDir()
	k6Dir = filepath.Join(dir, "baselines")
	if err := os.MkdirAll(k6Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"perf.json": perfFixture, "pressure.json": pressureFixture} {
		if err := os.WriteFile(filepath.Join(k6Dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	benchFile = filepath.Join(dir, "results.json")
	if err := os.WriteFile(benchFile, []byte(benchFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	return k6Dir, benchFile
}

// panelByTitle finds a rendered panel, so a test can assert on the bars of the
// one it cares about.
func panelByTitle(t *testing.T, c chart, title string) panel {
	t.Helper()
	for _, p := range c.panels {
		if p.title == title {
			return p
		}
	}
	t.Fatalf("no panel %q in %v", title, titles(c))
	return panel{}
}

func titles(c chart) []string {
	var out []string
	for _, p := range c.panels {
		out = append(out, p.title)
	}
	return out
}

func TestK6ChartUsesCommittedBaselines(t *testing.T) {
	k6Dir, _ := writeFixtures(t)
	c, err := k6Charts(k6Dir)
	if err != nil {
		t.Fatal(err)
	}
	read := panelByTitle(t, c, "Read latency, p95")
	// Three of the eight charted read metrics are in the fixture; the absent
	// ones must not appear as zero-length bars.
	if len(read.bars) != 3 {
		t.Fatalf("read panel has %d bars, want 3: %+v", len(read.bars), read.bars)
	}
	if got := read.bars[0].value; got != "4.8 ms" {
		t.Errorf("first bar value = %q, want 4.8 ms", got)
	}
	served := panelByTitle(t, c, "Requests served per run")
	if got := served.bars[0].value; got != "579,388" {
		t.Errorf("request count = %q, want 579,388", got)
	}
}

func TestK6ChartTakesCaptionsFromTheBaseline(t *testing.T) {
	k6Dir, _ := writeFixtures(t)
	c, err := k6Charts(k6Dir)
	if err != nil {
		t.Fatal(err)
	}
	// The gate file says which way is better and how many runs it distilled;
	// the chart must not invent a different story from the same numbers.
	if got := panelByTitle(t, c, "Requests served per run").scale(); got != "requests, higher is better" {
		t.Errorf("served panel scale = %q", got)
	}
	if got := panelByTitle(t, c, "Read latency, p95").scale(); got != "ms, lower is better" {
		t.Errorf("read panel scale = %q", got)
	}
	if !strings.Contains(c.subtitle, "Worst of 12 sampled runs") {
		t.Errorf("subtitle should carry the sample count, got %q", c.subtitle)
	}
}

// A metric entry that grows a field this tool does not chart must not stop the
// chart from drawing: the gate's schema is not this tool's to freeze.
func TestK6ChartToleratesUnknownMetricFields(t *testing.T) {
	dir := t.TempDir()
	body := `{"suite":"perf","metrics":{"http_reqs":{"count":10,"tolerance":0.3,"direction":"higher","note":"whatever","nested":{"a":1}}}}`
	if err := os.WriteFile(filepath.Join(dir, "perf.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := k6Charts(dir)
	if err != nil {
		t.Fatalf("unknown fields broke the chart: %v", err)
	}
	if got := panelByTitle(t, c, "Requests served per run").bars[0].value; got != "10" {
		t.Errorf("value = %q, want 10", got)
	}
}

func TestK6ChartMissingBaselines(t *testing.T) {
	if _, err := k6Charts(t.TempDir()); err == nil {
		t.Fatal("expected an error when no baselines exist")
	}
}

func TestBenchChartOrdersTargetsAndKeepsBytesComparable(t *testing.T) {
	_, benchFile := writeFixtures(t)
	c, err := benchCharts(benchFile)
	if err != nil {
		t.Fatal(err)
	}

	qps := panelByTitle(t, c, "narinfo throughput")
	var labels []string
	for _, b := range qps.bars {
		labels = append(labels, b.label)
	}
	want := []string{"xilo", "attic", "harmonia", "nix-serve-ng", "MinIO + nix copy", "Garage + nix copy"}
	if strings.Join(labels, ",") != strings.Join(want, ",") {
		t.Errorf("bar order = %v, want %v", labels, want)
	}

	// A bucket serving compressed NARs is not serving the same bytes, so it is
	// left out of the throughput panel rather than winning it.
	pull := panelByTitle(t, c, "NAR pull throughput")
	for _, b := range pull.bars {
		if b.label == "MinIO + nix copy" || b.label == "Garage + nix copy" {
			t.Errorf("incomparable target %q must not appear in the NAR throughput panel", b.label)
		}
	}

	// A target that has no such number (nix-serve-ng serves the host store and
	// stores nothing of its own) contributes no bar.
	stored := panelByTitle(t, c, "Stored on disk")
	for _, b := range stored.bars {
		if b.label == "nix-serve-ng" {
			t.Error("target without stored_bytes must not be charted")
		}
	}

	// A target the harness could not measure is never drawn: an implementation
	// that did not run has no bar to compare. It is also not in targetOrder, so
	// an unknown id cannot smuggle itself into a chart either.
	for _, p := range c.panels {
		for _, b := range p.bars {
			if b.label == "broken" {
				t.Errorf("skipped target charted in panel %q", p.title)
			}
		}
	}

	if !strings.Contains(c.subtitle, "test runner, 2 cores") || !strings.Contains(c.subtitle, "2026-09-08") {
		t.Errorf("subtitle should name the machine and the date, got %q", c.subtitle)
	}
}

func TestBenchChartDividesCPUByBytesServed(t *testing.T) {
	_, benchFile := writeFixtures(t)
	c, err := benchCharts(benchFile)
	if err != nil {
		t.Fatal(err)
	}
	// Raw CPU percentage flatters whichever server served least, so the panel
	// is a ratio: 347.5% over 695 MB/s is half a percent of a core per MB/s.
	cpu := panelByTitle(t, c, "CPU per MB/s served")
	if got, want := cpu.bars[0].value, "0.50"; got != want {
		t.Errorf("xilo CPU per MB/s = %q, want %q", got, want)
	}
	if got, want := cpu.bars[1].value, "1.00"; got != want {
		t.Errorf("attic CPU per MB/s = %q, want %q", got, want)
	}
	// nix-serve-ng has no CPU sample in the fixture, so it contributes no bar
	// rather than a zero that would read as free.
	if len(cpu.bars) != 2 {
		t.Errorf("CPU panel has %d bars, want 2", len(cpu.bars))
	}
}

var barEnd = regexp.MustCompile(`<path d="M([0-9.]+) [0-9.]+H([0-9.]+)A([0-9.]+) `)

func barLengths(t *testing.T, svg string) []float64 {
	t.Helper()
	var out []float64
	for _, m := range barEnd.FindAllStringSubmatch(svg, -1) {
		x, _ := strconv.ParseFloat(m[1], 64)
		h, _ := strconv.ParseFloat(m[2], 64)
		r, _ := strconv.ParseFloat(m[3], 64)
		out = append(out, h+r-x)
	}
	return out
}

func TestSVGDrawsZeroBasedBarsScaledToTheLongest(t *testing.T) {
	p := panel{title: "Throughput", unit: "MB/s", better: higherIsBetter, bars: []bar{
		{label: "xilo", value: "600 MB/s", amount: 600, highlight: true},
		{label: "attic", value: "150 MB/s", amount: 150},
		{label: "nix-serve-ng", value: "300 MB/s", amount: 300},
	}}
	svg := chart{panels: []panel{p}}.svg(themes[0])
	got := barLengths(t, svg)
	if len(got) != 3 {
		t.Fatalf("drew %d bars, want 3:\n%s", len(got), svg)
	}
	for i, want := range []float64{1, 0.25, 0.5} {
		if math.Abs(got[i]/got[0]-want) > 0.01 {
			t.Errorf("bar %d is %.3f of the longest, want %.2f (lengths %v)", i, got[i]/got[0], want, got)
		}
	}
}

func TestSVGHighlightsXiloAndBoldsItAmongOthers(t *testing.T) {
	_, benchFile := writeFixtures(t)
	c, err := benchCharts(benchFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, th := range themes {
		svg := c.svg(th)
		if got := strings.Count(svg, `fill="`+th.highlight+`"/>`); got != len(c.panels) {
			t.Errorf("%s: %d highlighted bars for %d panels each holding xilo once", th.name, got, len(c.panels))
		}
		if got := strings.Count(svg, `font-weight="600" text-anchor="end" dominant-baseline="central">xilo<`); got != len(c.panels) {
			t.Errorf("%s: xilo is bold in %d of %d panels; identity must not rest on colour alone", th.name, got, len(c.panels))
		}
	}
}

func TestSVGDoesNotBoldWhenEveryBarIsHighlighted(t *testing.T) {
	k6Dir, _ := writeFixtures(t)
	c, err := k6Charts(k6Dir)
	if err != nil {
		t.Fatal(err)
	}
	if svg := c.svg(themes[0]); strings.Contains(svg, `font-weight="600" text-anchor="end"`) {
		t.Errorf("a chart of one target bolded its labels:\n%.400s", svg)
	}
}

func TestSVGThemesInkTheirOwnTextOnATransparentGround(t *testing.T) {
	_, benchFile := writeFixtures(t)
	c, err := benchCharts(benchFile)
	if err != nil {
		t.Fatal(err)
	}
	light, dark := c.svg(themes[0]), c.svg(themes[1])
	if !strings.Contains(light, `fill="`+themes[0].text+`"`) || strings.Contains(light, `fill="`+themes[1].text+`"`) {
		t.Error("light chart is not inked with the light theme's text colour")
	}
	if !strings.Contains(dark, `fill="`+themes[1].text+`"`) || strings.Contains(dark, `fill="`+themes[0].text+`"`) {
		t.Error("dark chart is not inked with the dark theme's text colour")
	}
	for _, svg := range []string{light, dark} {
		if strings.Contains(svg, "<rect") {
			t.Error("a chart paints its own background; it must sit on the page's")
		}
	}
}

func TestSVGEscapesTextAndNeverPrintsScientificNotation(t *testing.T) {
	c := chart{alt: `a "b" <c>`, panels: []panel{{title: "A & B", unit: "bytes", bars: []bar{
		{label: "<x>", value: "225 MB", amount: 225000000},
		{label: "y", value: "1 B", amount: 1},
	}}}}
	svg := c.svg(themes[0])
	for _, want := range []string{"A &amp; B", "&lt;x&gt;", "a &#34;b&#34; &lt;c&gt;"} {
		if !strings.Contains(svg, want) {
			t.Errorf("missing %q in:\n%s", want, svg)
		}
	}
	if regexp.MustCompile(`[0-9]e[+-]?[0-9]`).MatchString(svg) {
		t.Errorf("scientific notation in:\n%s", svg)
	}
}

func TestMarkdownPicksTheChartByThemeAndKeepsATable(t *testing.T) {
	_, benchFile := writeFixtures(t)
	c, err := benchCharts(benchFile)
	if err != nil {
		t.Fatal(err)
	}
	md := c.markdown("compare", "perf")
	for _, want := range []string{
		`<source media="(prefers-color-scheme: dark)" srcset="perf/compare-dark.svg">`,
		`src="perf/compare-light.svg"`,
		`alt="Bar charts comparing xilo with other self-hostable Nix caches`,
		"<summary>The same numbers as a table</summary>",
		"| Measure | xilo |",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("missing %q in:\n%s", want, md)
		}
	}
}

func TestTargetTableBoldsTheBestAndMarksMissingTargets(t *testing.T) {
	c := chart{table: tableByTarget, panels: []panel{
		{title: "Throughput", unit: "MB/s", better: higherIsBetter, bars: []bar{
			{label: "xilo", value: "650 MB/s", amount: 650}, {label: "attic", value: "70 MB/s", amount: 70},
		}},
		{title: "Push", unit: "s", better: lowerIsBetter, bars: []bar{
			{label: "xilo", value: "0.1 s", amount: 0.1}, {label: "garage", value: "0.1 s", amount: 0.1},
		}},
		{title: "Stored", unit: "bytes", bars: []bar{
			{label: "xilo", value: "225 MB", amount: 225}, {label: "attic", value: "212 MB", amount: 212},
		}},
	}}
	table := c.targetTable()
	for _, want := range []string{
		"| Measure | xilo | attic | garage |\n",
		"| Throughput (MB/s, higher is better) | **650 MB/s** | 70 MB/s | — |\n",
		"| Push (s, lower is better) | **0.1 s** | — | **0.1 s** |\n",
		"| Stored (bytes) | 225 MB | 212 MB | — |\n",
	} {
		if !strings.Contains(table, want) {
			t.Errorf("missing %q in:\n%s", want, table)
		}
	}
}

func TestTargetTableBoldsEveryValueThatReadsAsTheBest(t *testing.T) {
	c := chart{table: tableByTarget, panels: []panel{{title: "CPU", unit: "%", better: lowerIsBetter, bars: []bar{
		{label: "xilo", value: "0.28", amount: 0.2789},
		{label: "harmonia", value: "0.28", amount: 0.2836},
		{label: "attic", value: "2.38", amount: 2.38},
	}}}}
	if table := c.targetTable(); !strings.Contains(table, "| CPU (%, lower is better) | **0.28** | **0.28** | 2.38 |\n") {
		t.Errorf("two values printed alike must both be bold:\n%s", table)
	}
}

func TestRowTableListsEveryBar(t *testing.T) {
	k6Dir, _ := writeFixtures(t)
	c, err := k6Charts(k6Dir)
	if err != nil {
		t.Fatal(err)
	}
	table := c.rowTable()
	if !strings.Contains(table, "| Read latency, p95 (ms, lower is better) | narinfo hit | 4.8 ms |\n") {
		t.Errorf("row missing in:\n%s", table)
	}
	bars := 0
	for _, p := range c.panels {
		bars += len(p.bars)
	}
	if got := strings.Count(table, "\n") - 2; got != bars {
		t.Errorf("%d table rows for %d bars", got, bars)
	}
}

func writeDocs(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "README.md")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunFillsEveryRegionAndKeepsTheRest(t *testing.T) {
	k6Dir, benchFile := writeFixtures(t)
	doc := writeDocs(t, "intro\n<!-- perfchart:compare -->\nold\n<!-- /perfchart:compare -->\nmiddle\n<!-- perfchart:k6-gate -->\n<!-- /perfchart:k6-gate -->\noutro\n")
	out := filepath.Join(filepath.Dir(doc), "docs", "perf")
	if err := run(k6Dir, benchFile, out, []string{doc}, false); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(doc)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	for _, want := range []string{"intro\n", "\nmiddle\n", "\noutro\n", `srcset="docs/perf/compare-dark.svg"`, `src="docs/perf/k6-gate-light.svg"`, "| narinfo hit | 4.8 ms |"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	if strings.Contains(s, "\nold\n") {
		t.Error("the previous chart was not replaced")
	}
	svg, err := os.ReadFile(filepath.Join(out, "compare-light.svg"))
	if err != nil || !strings.Contains(string(svg), "5,981/s") {
		t.Errorf("compare-light.svg missing or without xilo's figure (err %v)", err)
	}
	if err := run(k6Dir, benchFile, out, []string{doc}, true); err != nil {
		t.Errorf("check right after a write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(out, "k6-gate-dark.svg"), []byte("<svg/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run(k6Dir, benchFile, out, []string{doc}, true); err == nil || !strings.Contains(err.Error(), "k6-gate-dark.svg") {
		t.Errorf("check missed a stale chart: %v", err)
	}
}

func TestCheckReportsAStaleDocumentWithoutWriting(t *testing.T) {
	k6Dir, benchFile := writeFixtures(t)
	body := "<!-- perfchart:compare -->\nold\n<!-- /perfchart:compare -->\n<!-- perfchart:k6-gate -->\n<!-- /perfchart:k6-gate -->\n"
	doc := writeDocs(t, body)
	if err := run(k6Dir, benchFile, filepath.Join(filepath.Dir(doc), "perf"), []string{doc}, true); err == nil {
		t.Fatal("check passed on a stale document")
	}
	if got, _ := os.ReadFile(doc); string(got) != body {
		t.Error("check rewrote the document")
	}
}

func TestRunRefusesAChartWithNowhereToGo(t *testing.T) {
	k6Dir, benchFile := writeFixtures(t)
	doc := writeDocs(t, "<!-- perfchart:compare -->\n<!-- /perfchart:compare -->\n")
	if err := run(k6Dir, benchFile, filepath.Join(filepath.Dir(doc), "perf"), []string{doc}, false); err == nil || !strings.Contains(err.Error(), "k6-gate") {
		t.Fatalf("err = %v, want one naming the k6-gate chart", err)
	}
}

func TestRunRefusesAnUnclosedRegion(t *testing.T) {
	k6Dir, benchFile := writeFixtures(t)
	doc := writeDocs(t, "<!-- perfchart:compare -->\n<!-- perfchart:k6-gate -->\n<!-- /perfchart:k6-gate -->\n")
	if err := run(k6Dir, benchFile, filepath.Join(filepath.Dir(doc), "perf"), []string{doc}, false); err == nil {
		t.Fatal("an unclosed region was accepted")
	}
}

func TestRunWithoutAnyInput(t *testing.T) {
	dir := t.TempDir()
	if err := run(filepath.Join(dir, "nope"), filepath.Join(dir, "nope.json"), filepath.Join(dir, "perf"), nil, false); err == nil {
		t.Fatal("expected an error when neither input exists")
	}
}

func TestFormatters(t *testing.T) {
	for _, tc := range []struct {
		got, want string
	}{
		{fmtMs(4.83), "4.8 ms"},
		{fmtMs(64.8), "65 ms"},
		{fmtMs(1091.2), "1.09 s"},
		{fmtCount(579388), "579,388"},
		{fmtCount(999), "999"},
		{fmtBytes(380000000), "380 MB"},
		{fmtBytes(1.2e9), "1.20 GB"},
		{fmtQPS(5981), "5,981/s"},
	} {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
}

func TestRunWritesNothingWhenAChartHasNowhereToGo(t *testing.T) {
	k6Dir, benchFile := writeFixtures(t)
	body := "<!-- perfchart:compare -->\nold\n<!-- /perfchart:compare -->\n"
	doc := writeDocs(t, body)
	if err := run(k6Dir, benchFile, filepath.Join(filepath.Dir(doc), "perf"), []string{doc}, false); err == nil {
		t.Fatal("expected an error")
	}
	if got, _ := os.ReadFile(doc); string(got) != body {
		t.Error("a failing run still rewrote the document")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(doc), "perf")); !os.IsNotExist(err) {
		t.Errorf("a failing run still wrote charts (stat err %v)", err)
	}
}
