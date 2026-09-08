package main

import (
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path/filepath"
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
	if got := panelByTitle(t, c, "Requests served per run").scale; got != "requests, higher is better" {
		t.Errorf("served panel scale = %q", got)
	}
	if got := panelByTitle(t, c, "Read latency, p95").scale; got != "ms, lower is better" {
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
	if !qps.bars[0].accent || qps.bars[1].accent {
		t.Error("accent should mark xilo's bar and no other")
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

func TestRenderProducesWellFormedThemedSVG(t *testing.T) {
	k6Dir, _ := writeFixtures(t)
	c, err := k6Charts(k6Dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, th := range themes {
		svg := render(c, th)
		dec := xml.NewDecoder(strings.NewReader(svg))
		for {
			if _, err := dec.Token(); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				t.Fatalf("%s theme: SVG is not well-formed XML: %v", th.name, err)
			}
		}
		if !strings.Contains(svg, th.bg) || !strings.Contains(svg, th.accent) {
			t.Errorf("%s theme: rendered SVG uses neither its background nor its accent", th.name)
		}
		if !strings.Contains(svg, "4.8 ms") {
			t.Errorf("%s theme: measured value missing from the drawing", th.name)
		}
	}
}

func TestRunWritesBothChartsInBothThemes(t *testing.T) {
	k6Dir, benchFile := writeFixtures(t)
	out := filepath.Join(t.TempDir(), "perf")
	if err := run(k6Dir, benchFile, out); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"k6-gate-light.svg", "k6-gate-dark.svg", "compare-light.svg", "compare-dark.svg",
	} {
		if _, err := os.Stat(filepath.Join(out, name)); err != nil {
			t.Errorf("missing %s: %v", name, err)
		}
	}
}

func TestRunWithoutAnyInput(t *testing.T) {
	dir := t.TempDir()
	if err := run(filepath.Join(dir, "nope"), filepath.Join(dir, "nope.json"), filepath.Join(dir, "out")); err == nil {
		t.Fatal("expected an error when neither input exists")
	}
}

func TestChartMaxRoundsUp(t *testing.T) {
	for _, tc := range []struct {
		in, want float64
	}{
		{0, 1}, {0.6, 1}, {4.8, 5}, {6, 10}, {45, 50}, {64.8, 100},
		{1091, 2000}, {579388, 1000000},
	} {
		if got := chartMax(tc.in); got != tc.want {
			t.Errorf("chartMax(%v) = %v, want %v", tc.in, got, tc.want)
		}
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
