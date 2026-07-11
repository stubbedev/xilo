package views_test

import (
	"testing"

	"github.com/stubbedev/xilo/internal/server/views"
)

func TestCount(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{
		{0, "0"}, {999, "999"},
		{1000, "1k"}, {1500, "1.5k"}, {21393, "21.4k"}, {999949, "999.9k"},
		{999_999, "1m"}, {1_000_000, "1m"}, {2_100_000, "2.1m"}, {21_393_000, "21.4m"},
		{999_950_000, "1b"},
		{1_300_000_000, "1.3b"},
	} {
		if got := views.Count(tc.in); got != tc.want {
			t.Errorf("Count(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
