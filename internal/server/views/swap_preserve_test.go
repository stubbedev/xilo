package views

import (
	"context"
	"strings"
	"testing"

	"github.com/stubbedev/xilo/internal/store"
)

// A region swapped in place by htmx (live search, chips, sort, pager) must
// render the machinery that keeps the reader's scroll and focus across the
// swap: htmx refocuses the focused control by id only, and scrollAnchorLayer
// (layout.templ) is the script that holds the viewport still. See issue #31.
func TestSwapPreservation(t *testing.T) {
	var b strings.Builder
	d := AuditData{
		Nav:     Nav{},
		Entries: []store.AuditEntry{{Actor: "a", Path: "/x"}},
		Methods: []string{"GET", "POST"},
		Classes: []string{"2xx"},
		Pager:   Pager{Pages: 2, Page: 1, Prev: "/admin/audit", Next: "/admin/audit?page=2", Target: "#audit-results"},
		Sort:    SortCtx{Path: "/admin/audit", Target: "#audit-results"},
	}
	if err := AuditPage(d).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	html := b.String()
	for _, want := range []string{
		`id="audit-results-chip-method-get"`,
		`id="audit-results-chip-method-all"`,
		`id="audit-results-chip-status-2xx"`,
		`id="audit-results-sort-time"`,
		`id="audit-results-prev"`,
		`id="audit-results-next"`,
		`pickAnchor`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("audit page missing %s", want)
		}
	}
}
