package main

import (
	"encoding/xml"
	"errors"
	"io"
	"math"
	"strings"
	"testing"
)

func TestOutputsAreWellFormedSVG(t *testing.T) {
	for _, o := range outputs() {
		dec := xml.NewDecoder(strings.NewReader(o.contents))
		root := ""
		for {
			tok, err := dec.Token()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("%s: %v", o.path, err)
			}
			if se, ok := tok.(xml.StartElement); ok && root == "" {
				root = se.Name.Local
			}
		}
		if root != "svg" {
			t.Errorf("%s: root element %q, want svg", o.path, root)
		}
	}
}

func TestPileRestsOnTheFloorOrOnAnotherCell(t *testing.T) {
	filled := map[cell]bool{}
	for _, c := range pile {
		if c[2] > 0 && !filled[cell{c[0], c[1], c[2] - 1}] {
			t.Errorf("cell %v is placed before anything holds it up", c)
		}
		filled[c] = true
	}
}

func TestEveryPileCellDrawsSomething(t *testing.T) {
	for _, c := range pile {
		if small.cellPath(c, nil) == "" {
			t.Errorf("cell %v has no edge off the outline, so adding it would change nothing", c)
		}
	}
}

func TestAnimationFillsThenEmptiesInReverse(t *testing.T) {
	for i := 1; i < len(pile); i++ {
		prevIn, prevOut := lifetime(i - 1)
		in, out := lifetime(i)
		if in <= prevIn || out >= prevOut {
			t.Errorf("cell %d lives [%v,%v], not inside cell %d's [%v,%v]", i, in, out, i-1, prevIn, prevOut)
		}
		if in >= out {
			t.Errorf("cell %d appears at %v but leaves at %v", i, in, out)
		}
	}
	_, firstOut := lifetime(0)
	if firstOut+fade > float64(slots()) {
		t.Errorf("the last removal ends after the cycle restarts")
	}
}

func TestDashesAreSymmetricOnEveryEdge(t *testing.T) {
	for _, m := range []mark{small, animated, banner} {
		period := m.edgeLength() / float64(m.stroke.dashesPerEdge)
		dash := math.Max(period/2-m.stroke.inner, .01)
		gap := period - dash
		want := `stroke-dasharray="` + num(dash) + " " + num(gap) + `" stroke-dashoffset="` + num(dash+gap/2) + `"`
		if !strings.Contains(m.dashAttrs(), want) {
			t.Errorf("size %v: %s, want %s", m.size, m.dashAttrs(), want)
		}
		if got := float64(m.stroke.dashesPerEdge) * (dash + gap); math.Abs(got-m.edgeLength()) > 1e-9 {
			t.Errorf("size %v: pattern covers %v of a %v edge", m.size, got, m.edgeLength())
		}
	}
}

func TestReducedMotionShowsTheFullPile(t *testing.T) {
	css := animationCSS("xm", 3)
	if !strings.Contains(css, "@media (prefers-reduced-motion:reduce){.xm-c{animation:none}}") {
		t.Errorf("no reduced-motion rule: %s", css)
	}
}

func TestOutputIsDeterministic(t *testing.T) {
	a, b := outputs(), outputs()
	for i := range a {
		if a[i] != b[i] {
			t.Errorf("%s differs between two runs", a[i].path)
		}
	}
}
