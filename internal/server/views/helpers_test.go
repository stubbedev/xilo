package views

import (
	"testing"

	"github.com/stubbedev/xilo/internal/store"
)

func TestDefaultAccount(t *testing.T) {
	accounts := []store.Account{{Slug: "acme"}, {Slug: "beta"}}
	if got := defaultAccount(DashboardData{}); got != "" {
		t.Fatalf("empty = %q", got)
	}
	if got := defaultAccount(DashboardData{Accounts: accounts}); got != "acme" {
		t.Fatalf("first = %q", got)
	}
	// The viewing context wins over everything.
	d := DashboardData{Accounts: accounts, Nav: Nav{Active: "beta", UserName: "acme"}}
	if got := defaultAccount(d); got != "beta" {
		t.Fatalf("active context = %q", got)
	}
	// Otherwise the user's own personal account, wherever it sits.
	d = DashboardData{Accounts: accounts, Nav: Nav{UserName: "beta"}}
	if got := defaultAccount(d); got != "beta" {
		t.Fatalf("personal account = %q", got)
	}
	// An account literally named "default" is no longer special.
	d = DashboardData{Accounts: []store.Account{{Slug: "acme"}, {Slug: "default"}}}
	if got := defaultAccount(d); got != "acme" {
		t.Fatalf("\"default\" must not win = %q", got)
	}
}

func TestFirstStr(t *testing.T) {
	if got := firstStr(nil); got != "" {
		t.Fatalf("nil = %q", got)
	}
	if got := firstStr([]string{"a", "b"}); got != "a" {
		t.Fatalf("first = %q", got)
	}
}

func TestUnlimitedClass(t *testing.T) {
	// No server cap → bare "unlimited".
	if got := unlimitedClass(0, 0); got != "unlimited" {
		t.Fatalf("no cap = %q", got)
	}
	// Under a cap that is near full → "unlimited " + the fill grade.
	if got := unlimitedClass(95, 100); got == "unlimited" || got[:10] != "unlimited " {
		t.Fatalf("pressured cap = %q", got)
	}
}
