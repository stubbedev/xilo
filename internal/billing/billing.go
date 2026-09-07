// Package billing is the seam between xilo and a payment provider.
//
// It exists so that the self-hosted build never links a payment SDK and the
// hosted build swaps a provider in behind the same three calls. Everything the
// rest of the server does with money goes through Provider; nothing else
// imports a provider package, so "does this instance charge" is one wiring
// decision rather than a condition sprinkled through the handlers.
//
// The no-op Disabled provider is the default and answers ErrDisabled to every
// request. That is deliberate: a checkout that silently does nothing is worse
// than one that says billing is not configured.
package billing

import (
	"context"
	"errors"
)

// ErrDisabled means this instance takes no payments.
var ErrDisabled = errors.New("billing is not configured on this instance")

// Subscription is what a provider tells us about a workspace's standing. It
// carries only what xilo acts on: which plan is paid for, and whether the
// account should be serving, read-only, or closed. Invoices, cards and dunning
// schedules stay on the provider's side of the seam.
type Subscription struct {
	ID string
	// PlanExternalID is the provider's price handle, which maps back to a row
	// in the plan catalogue (Plan.ExternalID).
	PlanExternalID string
	// Status is the provider's verdict, already reduced to the account
	// lifecycle xilo enforces: "active", "past_due" or "suspended".
	Status string
}

// Provider is a payment backend. A hosted instance supplies one; a
// self-hosted instance gets Disabled.
type Provider interface {
	// Enabled reports whether this instance charges at all. The UI asks
	// before it offers anything to click.
	Enabled() bool
	// Checkout returns a URL where the owner of `accountSlug` can start or
	// change a subscription to the plan identified by `planExternalID`.
	Checkout(ctx context.Context, accountSlug, planExternalID string) (string, error)
	// Subscription reads a subscription's current standing, so a webhook is a
	// convenience rather than the only source of truth — an instance that
	// missed an event can always re-read.
	Subscription(ctx context.Context, subscriptionID string) (Subscription, error)
	// Cancel ends a subscription at the end of the paid period.
	Cancel(ctx context.Context, subscriptionID string) error
}

// Disabled is the provider for an instance that takes no payments: every plan
// is whatever the sysadmin says it is, and nothing is for sale.
type Disabled struct{}

func (Disabled) Enabled() bool { return false }

func (Disabled) Checkout(context.Context, string, string) (string, error) {
	return "", ErrDisabled
}

func (Disabled) Subscription(context.Context, string) (Subscription, error) {
	return Subscription{}, ErrDisabled
}

func (Disabled) Cancel(context.Context, string) error { return ErrDisabled }

// Ensure the no-op keeps satisfying the interface as it grows.
var _ Provider = Disabled{}
