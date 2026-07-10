package views

import "testing"

func TestFillGrading(t *testing.T) {
	cases := []struct {
		used, cap int64
		pct       int
		class     string
	}{
		{0, 0, 0, ""},     // uncapped
		{50, 100, 50, ""}, // headroom
		{75, 100, 75, "warn"},
		{95, 100, 95, "over"},
		{200, 100, 100, "over"}, // clamped
	}
	for _, c := range cases {
		if got := fillPct(c.used, c.cap); got != c.pct {
			t.Errorf("fillPct(%d,%d) = %d, want %d", c.used, c.cap, got, c.pct)
		}
		if got := fillClass(c.used, c.cap); got != c.class {
			t.Errorf("fillClass(%d,%d) = %q, want %q", c.used, c.cap, got, c.class)
		}
	}
}

func TestDurParts(t *testing.T) {
	if v, u := durParts(0); v != "" || u != "d" {
		t.Errorf("durParts(0) = %q %q", v, u)
	}
	if v, u := durParts(30 * 86400); v != "1" || u != "mo" {
		t.Errorf("durParts(1mo) = %q %q", v, u)
	}
	if v, u := durParts(7 * 86400); v != "7" || u != "d" {
		t.Errorf("durParts(7d) = %q %q", v, u)
	}
	if v, u := durParts(2 * 31536000); v != "2" || u != "y" {
		t.Errorf("durParts(2y) = %q %q", v, u)
	}
	if v, u := durParts(36 * 3600); v != "36" || u != "h" {
		t.Errorf("durParts(36h) = %q %q", v, u)
	}
}

func TestSizeParts(t *testing.T) {
	if v, u := sizeParts(0); v != "" || u != "GiB" {
		t.Errorf("sizeParts(0) = %q %q", v, u)
	}
	if v, u := sizeParts(50 << 30); v != "50" || u != "GiB" {
		t.Errorf("sizeParts(50GiB) = %q %q", v, u)
	}
	if v, u := sizeParts(110 << 20); v != "110" || u != "MiB" {
		t.Errorf("sizeParts(110MiB) = %q %q", v, u)
	}
	if v, u := sizeParts(2 << 40); v != "2" || u != "TiB" {
		t.Errorf("sizeParts(2TiB) = %q %q", v, u)
	}
}

func TestScopeAll(t *testing.T) {
	if !scopeAll(nil) || !scopeAll([]string{"*"}) || scopeAll([]string{"demo"}) {
		t.Error("scopeAll misclassifies")
	}
}

func TestPageOf(t *testing.T) {
	items := make([]int, 45)
	page, p, pages := PageOf(items, 1, 20)
	if len(page) != 20 || p != 1 || pages != 3 {
		t.Errorf("page1: len=%d p=%d pages=%d", len(page), p, pages)
	}
	page, p, pages = PageOf(items, 3, 20)
	if len(page) != 5 || p != 3 || pages != 3 {
		t.Errorf("page3: len=%d p=%d pages=%d", len(page), p, pages)
	}
	page, p, pages = PageOf(items, 99, 20)
	if len(page) != 5 || p != 3 {
		t.Errorf("clamp: len=%d p=%d pages=%d", len(page), p, pages)
	}
	page, p, pages = PageOf([]int{}, 1, 20)
	if len(page) != 0 || p != 1 || pages != 1 {
		t.Errorf("empty: len=%d p=%d pages=%d", len(page), p, pages)
	}
}

func TestPathParts(t *testing.T) {
	cases := []struct{ in, hash, name string }{
		{"/nix/store/8kvxvr3pmsypxiypq4g8zy13glnfr7nx-glibc-2.42-67", "8kvxvr3p", "glibc-2.42-67"},
		{"/nix/store/short-x", "short", "x"},
		{"/nix/store/nodash", "", "nodash"},
		{"not-a-store-path", "", "not-a-store-path"},
	}
	for _, c := range cases {
		if h, n := pathParts(c.in); h != c.hash || n != c.name {
			t.Errorf("pathParts(%q) = %q %q, want %q %q", c.in, h, n, c.hash, c.name)
		}
	}
}
