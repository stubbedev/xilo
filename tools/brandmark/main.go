// Command brandmark draws xilo's mark: an isometric cube with a dashed 3×3×3
// lattice, of which only a small pile of cells in the back corner is filled.
// One geometry, every output: the favicon, the inline marks the admin embeds,
// and the animated README banners, so no file can drift from another.
//
// Every cell edge projects to the same length in an isometric view, and two
// edges that overlap on screen overlap exactly. Each edge therefore carries its
// own dash pattern, centred and symmetric, which renders identically whichever
// cell draws it and in whichever direction: that is what lets the animation add
// and remove one cell at a time without the dashes of neighbours clashing.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type vec3 [3]float64

type point struct{ x, y float64 }

type edge [2]vec3

type cell [3]int

const divisions = 3

var pile = []cell{{0, 0, 0}, {1, 0, 0}, {0, 1, 0}, {0, 0, 1}}

var outline = []edge{
	{{1, 0, 1}, {1, 1, 1}}, {{0, 1, 1}, {1, 1, 1}}, {{1, 1, 0}, {1, 1, 1}},
	{{0, 0, 1}, {1, 0, 1}}, {{0, 0, 1}, {0, 1, 1}},
	{{1, 0, 0}, {1, 0, 1}}, {{0, 1, 0}, {0, 1, 1}},
	{{1, 0, 0}, {1, 1, 0}}, {{0, 1, 0}, {1, 1, 0}},
}

var cos30 = math.Sqrt(3) / 2

func project(v vec3) point {
	return point{(v[0] - v[1]) * cos30, (v[0]+v[1])/2 - v[2]}
}

type stroke struct {
	outer, inner  float64
	dashesPerEdge int
}

type mark struct {
	size, pad float64
	stroke    stroke
}

var (
	small    = mark{size: 32, pad: 3, stroke: stroke{outer: 1.5, inner: 1, dashesPerEdge: 1}}
	animated = mark{size: 64, pad: 6, stroke: stroke{outer: 1.2, inner: .8, dashesPerEdge: 3}}
	banner   = mark{size: 128, pad: 12, stroke: stroke{outer: 1.5, inner: 1, dashesPerEdge: 5}}
)

func (m mark) scale() float64 { return (m.size - 2*m.pad) / 2 }

func (m mark) place(v vec3) point {
	p, k := project(v), m.scale()
	return point{m.size/2 + p.x*k, m.size/2 + p.y*k}
}

func (m mark) edgeLength() float64 { return m.scale() / divisions }

func num(f float64) string {
	return strconv.FormatFloat(math.Round(f*1000)/1000, 'f', -1, 64)
}

func (m mark) segment(e edge) string {
	a, b := m.place(e[0]), m.place(e[1])
	return "M" + num(a.x) + " " + num(a.y) + "L" + num(b.x) + " " + num(b.y)
}

func (m mark) outlinePath() string {
	var b strings.Builder
	for _, e := range outline {
		b.WriteString(m.segment(e))
	}
	return fmt.Sprintf(`<path d="%s" stroke-width="%s"/>`, b.String(), num(m.stroke.outer))
}

func (m mark) dashAttrs() string {
	period := m.edgeLength() / float64(m.stroke.dashesPerEdge)
	dash := math.Max(period/2-m.stroke.inner, .01)
	gap := period - dash
	return fmt.Sprintf(`stroke-width="%s" stroke-dasharray="%s %s" stroke-dashoffset="%s"`,
		num(m.stroke.inner), num(dash), num(gap), num(dash+gap/2))
}

func cellEdges(c cell) []edge {
	s := 1.0 / divisions
	lo := vec3{float64(c[0]) * s, float64(c[1]) * s, float64(c[2]) * s}
	var es []edge
	for axis := range 3 {
		for _, u := range []float64{0, s} {
			for _, v := range []float64{0, s} {
				a := lo
				a[(axis+1)%3] += u
				a[(axis+2)%3] += v
				b := a
				b[axis] += s
				es = append(es, edge{a, b})
			}
		}
	}
	return es
}

func onOutline(e edge) bool {
	a, b := project(e[0]), project(e[1])
	for _, o := range outline {
		p, q := project(o[0]), project(o[1])
		if within(a, p, q) && within(b, p, q) {
			return true
		}
	}
	return false
}

func within(x, p, q point) bool {
	const eps = 1e-9
	cross := (q.x-p.x)*(x.y-p.y) - (q.y-p.y)*(x.x-p.x)
	if math.Abs(cross) > eps {
		return false
	}
	dot := (x.x-p.x)*(q.x-p.x) + (x.y-p.y)*(q.y-p.y)
	return dot >= -eps && dot <= (q.x-p.x)*(q.x-p.x)+(q.y-p.y)*(q.y-p.y)+eps
}

func segmentKey(e edge) string {
	a, b := project(e[0]), project(e[1])
	ka := num(a.x) + "," + num(a.y)
	kb := num(b.x) + "," + num(b.y)
	if kb < ka {
		ka, kb = kb, ka
	}
	return ka + " " + kb
}

func (m mark) cellPath(c cell, seen map[string]bool) string {
	var b strings.Builder
	for _, e := range cellEdges(c) {
		if onOutline(e) {
			continue
		}
		if seen != nil {
			k := segmentKey(e)
			if seen[k] {
				continue
			}
			seen[k] = true
		}
		b.WriteString(m.segment(e))
	}
	return b.String()
}

func (m mark) staticBody() string {
	seen := map[string]bool{}
	var d strings.Builder
	for _, c := range pile {
		d.WriteString(m.cellPath(c, seen))
	}
	return fmt.Sprintf(`<path d="%s" %s/>`, d.String(), m.dashAttrs()) + m.outlinePath()
}

const (
	slot      = 0.7
	fade      = 0.25
	dropUnits = 3.0
)

func slots() int { return 2*len(pile) + 4 }

func lifetime(i int) (in, out float64) {
	in = float64(i + 1)
	return in, float64(2*len(pile)+2) - in
}

func (m mark) animatedBody(prefix string) string {
	var b strings.Builder
	for i, c := range pile {
		fmt.Fprintf(&b, `<path class="%s-c %s-c%d" d="%s" %s/>`, prefix, prefix, i+1, m.cellPath(c, nil), m.dashAttrs())
	}
	b.WriteString(m.outlinePath())
	return b.String()
}

func animationCSS(prefix string, drop float64) string {
	total := float64(slots())
	pct := func(s float64) string { return num(s/total*100) + "%" }
	var b strings.Builder
	fmt.Fprintf(&b, ".%s-c{animation:%ss ease-out infinite both}", prefix, num(total*slot))
	hidden := fmt.Sprintf("opacity:0;transform:translateY(-%spx)", num(drop))
	shown := "opacity:1;transform:none"
	for i := range pile {
		in, out := lifetime(i)
		fmt.Fprintf(&b, ".%s-c%d{animation-name:%s-c%d}", prefix, i+1, prefix, i+1)
		fmt.Fprintf(&b, "@keyframes %s-c%d{0%%,%s{%s}%s,%s{%s}%s,100%%{%s}}", prefix, i+1,
			pct(in), hidden, pct(in+fade), pct(out), shown, pct(out+fade), hidden)
	}
	fmt.Fprintf(&b, "@media (prefers-reduced-motion:reduce){.%s-c{animation:none}}", prefix)
	return b.String()
}

func group(color, body string) string {
	return fmt.Sprintf(`<g fill="none" stroke="%s" stroke-linecap="round" stroke-linejoin="round">%s</g>`, color, body)
}

type palette struct {
	name, ink, mark, muted string
}

var (
	latte = palette{"light", "#4c4f69", "#8839ef", "#5c5f77"}
	mocha = palette{"dark", "#cdd6f4", "#cba6f7", "#a6adc8"}
)

func favicon() string {
	m := small
	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="%[1]s" height="%[1]s" viewBox="0 0 %[1]s %[1]s">`+
		`<style>svg{color:%[2]s}@media (prefers-color-scheme:dark){svg{color:%[3]s}}</style>%[4]s</svg>`+"\n",
		num(m.size), latte.mark, mocha.mark, group("currentColor", m.staticBody()))
}

func inline(m mark, body, css string) string {
	style := ""
	if css != "" {
		style = "<style>" + css + "</style>"
	}
	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %[1]s %[1]s" width="100%%" height="100%%" aria-hidden="true">%[2]s%[3]s</svg>`+"\n",
		num(m.size), style, group("currentColor", body))
}

const sansFont = "ui-sans-serif,-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,sans-serif"

func bannerSVG(p palette) string {
	const width, height = 640.0, 160.0
	m := banner
	top := (height - m.size) / 2
	textX := top + m.size + 24
	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="%[1]s" height="%[2]s" viewBox="0 0 %[1]s %[2]s">`+
		`<title>xilo</title><style>%[3]s</style>`+
		`<g transform="translate(%[4]s %[4]s)">%[5]s</g>`+
		`<g font-family="%[6]s"><text x="%[7]s" y="86" font-size="64" font-weight="600" letter-spacing="-2" fill="%[8]s">xilo</text>`+
		`<text x="%[9]s" y="118" font-size="18" fill="%[10]s">%[11]s</text></g></svg>`+"\n",
		num(width), num(height), animationCSS("xm", dropUnits*m.size/32), num(top),
		group(p.mark, m.animatedBody("xm")), sansFont, num(textX), p.ink, num(textX+3), p.muted, tagline)
}

type output struct {
	path, contents string
}

func outputs() []output {
	return []output{
		{"internal/server/static/favicon.svg", favicon()},
		{"internal/server/views/mark.svg", inline(small, small.staticBody(), "")},
		{"internal/server/views/mark-animated.svg", inline(animated, animated.animatedBody("xm"), animationCSS("xm", dropUnits*animated.size/32))},
		{"docs/brand/banner-light.svg", bannerSVG(latte)},
		{"docs/brand/banner-dark.svg", bannerSVG(mocha)},
	}
}

func writeAtomic(path, contents string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create directory for %s: %w", path, err)
	}
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

const tagline = "A self-hosted Nix binary cache in a single Go binary."

func check() error {
	var stale []string
	for _, o := range outputs() {
		got, err := os.ReadFile(o.path)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("read %s: %w", o.path, err)
		}
		if string(got) != o.contents {
			stale = append(stale, o.path)
		}
	}
	if len(stale) > 0 {
		return fmt.Errorf("stale, run 'just brand' and commit: %s", strings.Join(stale, ", "))
	}
	return nil
}

func write() error {
	for _, o := range outputs() {
		if err := writeAtomic(o.path, o.contents); err != nil {
			return err
		}
		fmt.Println("wrote", o.path)
	}
	return nil
}

func main() {
	verify := flag.Bool("check", false, "fail if a committed mark differs from what would be drawn, instead of writing")
	flag.Parse()
	run := write
	if *verify {
		run = check
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "brandmark:", err)
		os.Exit(1)
	}
}
