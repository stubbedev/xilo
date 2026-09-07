package server

import (
	"errors"
	"net/url"
	"testing"

	"github.com/stubbedev/xilo/internal/billing"
	"github.com/stubbedev/xilo/internal/store"
)

// TestBillingSeam pins the two things the seam promises: an instance takes no
// payments until a provider is wired in, and the plan catalogue can carry a
// price either way — a sysadmin publishing prices does not require a provider
// to already be connected.
func TestBillingSeam(t *testing.T) {
	srv, db, ts := mtServer(t)
	bootstrapAdmin(t, db)
	c := adminClient(t, ts)

	// No provider: the seam refuses instead of pretending.
	if srv.billing.Enabled() {
		t.Error("a fresh instance reports billing enabled")
	}
	if _, err := srv.billing.Checkout(t.Context(), "acme", "price_x"); !errors.Is(err, billing.ErrDisabled) {
		t.Errorf("checkout on a disabled instance: %v", err)
	}

	// A priced plan round-trips: whole units in, cents stored.
	_, b := postFlash(t, c, ts.URL+"/admin/plans", url.Values{
		"name": {"team"}, "price": {"12.50"}, "interval": {"year"},
		"external_id": {"price_team_yearly"}, "public": {"on"},
	})
	if !contains(b, "team") {
		t.Fatalf("creating a priced plan: %.120q", b)
	}
	plans, err := db.ListPlans()
	if err != nil || len(plans) == 0 {
		t.Fatalf("ListPlans: %v %v", plans, err)
	}
	p := plans[0]
	if p.PriceCents != 1250 {
		t.Errorf("price = %d cents want 1250", p.PriceCents)
	}
	if p.Interval != "year" {
		t.Errorf("interval = %q want year", p.Interval)
	}
	if p.ExternalID != "price_team_yearly" {
		t.Errorf("external id = %q", p.ExternalID)
	}

	// A free plan carries no billing period, so nothing renders a period for
	// something that never renews.
	postFlash(t, c, ts.URL+"/admin/plans", url.Values{"name": {"hobby"}, "interval": {"month"}})
	all, err := db.ListPlans()
	if err != nil {
		t.Fatalf("ListPlans: %v", err)
	}
	var free *store.Plan
	for i := range all {
		if all[i].Name == "hobby" {
			free = &all[i]
		}
	}
	if free == nil {
		t.Fatal("free plan not created")
	}
	if free.PriceCents != 0 || free.Interval != "" {
		t.Errorf("free plan carries billing: %d %q", free.PriceCents, free.Interval)
	}
}
