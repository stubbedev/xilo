package views

import (
	"testing"

	"github.com/stubbedev/xilo/internal/store"
)

func TestDefaultAccount(t *testing.T) {
	if got := defaultAccount(nil); got != "" {
		t.Fatalf("empty = %q", got)
	}
	if got := defaultAccount([]store.Account{{Slug: "acme"}, {Slug: "beta"}}); got != "acme" {
		t.Fatalf("first = %q", got)
	}
	// "default" wins wherever it sits in the list.
	if got := defaultAccount([]store.Account{{Slug: "acme"}, {Slug: "default"}}); got != "default" {
		t.Fatalf("default preferred = %q", got)
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
