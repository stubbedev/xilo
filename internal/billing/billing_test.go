package billing

import (
	"context"
	"errors"
	"testing"
)

// The default provider must refuse rather than pretend: a checkout that
// silently does nothing is the failure mode this seam exists to avoid.
func TestDisabledRefusesEverything(t *testing.T) {
	var p Provider = Disabled{}
	if p.Enabled() {
		t.Error("Disabled reports itself enabled")
	}
	if _, err := p.Checkout(context.Background(), "acme", "price_x"); !errors.Is(err, ErrDisabled) {
		t.Errorf("Checkout err = %v want ErrDisabled", err)
	}
	if _, err := p.Subscription(context.Background(), "sub_x"); !errors.Is(err, ErrDisabled) {
		t.Errorf("Subscription err = %v want ErrDisabled", err)
	}
	if err := p.Cancel(context.Background(), "sub_x"); !errors.Is(err, ErrDisabled) {
		t.Errorf("Cancel err = %v want ErrDisabled", err)
	}
}
