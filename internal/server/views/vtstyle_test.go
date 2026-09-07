package views

import "testing"

// A view-transition name has to be a CSS ident (no slashes, no dots) and has to
// stay distinct per object: two elements sharing one abort the whole
// transition, which is how the animation silently stops working.
func TestVTStyleIsAnIdent(t *testing.T) {
	got := vtStyle("Acme/Web.1")
	if want := "view-transition-name:vt-acme_web_1"; got != want {
		t.Fatalf("vtStyle = %q, want %q", got, want)
	}
	if vtStyle("acme/web") == vtStyle("acme/api") {
		t.Fatal("two caches share a transition name")
	}
}
