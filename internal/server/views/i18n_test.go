package views

import (
	"context"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// tCall matches every literal lookup, whatever the context expression is
// called and whichever function does it: T(ctx, "key") in the views package,
// views.T(r.Context(), "key") from the handlers, views.T(mctx, "key") from the
// mail paths, Tf(ctx, "key", args…) wherever the message has a value in it.
// Group 1 is the function suffix ("" or "f"), group 2 the key. Dynamic keys
// (T(ctx, "role."+role)) do not match and are not checked here; the smoke test
// covers them by rendering the components that build them.
var tCall = regexp.MustCompile(`\bT(f?)\([A-Za-z_][\w.()]*, "([a-zA-Z0-9._]+)"\s*[,)]`)

// hasVerb reports whether a catalog value takes fmt arguments. "%%" is an
// escaped percent, not a verb.
func hasVerb(s string) bool {
	return strings.Contains(strings.ReplaceAll(s, "%%", ""), "%")
}

// TestCatalogCoversEveryLookup walks the source tree and checks two things a
// silent fallback would otherwise hide until someone saw the page:
//
//   - a key missing from the catalog: T returns the id, so a typo renders as
//     "err.notadmin" instead of erroring;
//   - a mismatch between T and Tf: T on a value with %s prints the verb
//     literally, Tf on a value without one appends "%!(EXTRA …)".
func TestCatalogCoversEveryLookup(t *testing.T) {
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == ".git" || name == "tmp" || name == "bin" || name == "node_modules" {
				return fs.SkipDir
			}
			return nil
		}
		ext := filepath.Ext(path)
		if ext != ".go" && ext != ".templ" {
			return nil
		}
		if strings.HasSuffix(path, "_templ.go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		for _, m := range tCall.FindAllStringSubmatch(string(src), -1) {
			formatted, key := m[1] == "f", m[2]
			val, ok := en[key]
			if !ok {
				t.Errorf("%s: T(%q) has no entry in the en catalog", rel, key)
				continue
			}
			switch {
			case formatted && !hasVerb(val):
				t.Errorf("%s: Tf(%q) but %q takes no arguments — use T", rel, key, val)
			case !formatted && hasVerb(val):
				t.Errorf("%s: T(%q) but %q takes arguments — use Tf", rel, key, val)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// verbSpec matches one fmt verb, including an explicit argument index:
// %s, %q, %d, %[2]s. Group 1 is the index when the translator reordered.
var verbSpec = regexp.MustCompile(`%(?:\[(\d+)\])?[-+ #0]*[0-9]*(?:\.[0-9]+)?([a-zA-Z%])`)

// argVerbs maps argument position → verb letter, following fmt's rule that an
// explicit [n] index also moves the implicit counter to n+1. Word order is a
// translator's business, so "%[2]s on %[1]s" must count as the same contract
// as "%s on %s" reversed — what has to match is which argument each verb eats.
func argVerbs(s string) map[int]string {
	out := map[int]string{}
	next := 1
	for _, m := range verbSpec.FindAllStringSubmatch(s, -1) {
		if m[2] == "%" { // an escaped percent consumes nothing
			continue
		}
		if m[1] != "" {
			n, _ := strconv.Atoi(m[1])
			next = n
		}
		out[next] = m[2]
		next++
	}
	return out
}

// TestCatalogsAgreeOnArguments keeps every translation substitutable for the
// English it replaces: it must consume the same arguments with the same verbs,
// reordered however the language needs. A catalog that drops a %s silently
// loses the value; one that adds a verb prints "%!s(MISSING)".
func TestCatalogsAgreeOnArguments(t *testing.T) {
	for id, c := range catalogs {
		if id == "en" {
			continue
		}
		for key, val := range c {
			base, ok := en[key]
			if !ok {
				t.Errorf("%s: key %q is not in the en catalog", id, key)
				continue
			}
			want, got := argVerbs(base), argVerbs(val)
			if !maps.Equal(want, got) {
				t.Errorf("%s: %q takes %v, en takes %v", id, key, got, want)
			}
		}
	}
}

func TestMatchLocale(t *testing.T) {
	for header, want := range map[string]string{
		"":                        "en",
		"de":                      "de",
		"de-AT,de;q=0.9,en;q=0.8": "de",
		"en-GB,en;q=0.9":          "en",
		"zh-Hans-CN,zh;q=0.9":     "zh",
		"kl-GL, da;q=0.9":         "en", // neither shipped
		"  FR-ca  ":               "fr",
	} {
		if got := MatchLocale(header); got != want {
			t.Errorf("MatchLocale(%q) = %q, want %q", header, got, want)
		}
	}
}

func TestTFallsBackToEnglish(t *testing.T) {
	ctx := WithLocale(context.Background(), "de")
	if LocaleFrom(ctx) != "de" {
		t.Fatal("locale not stored on the context")
	}
	// A key no catalog defines returns the id, so a typo is visible.
	if got := T(ctx, "no.such.key"); got != "no.such.key" {
		t.Errorf("unknown key = %q", got)
	}
	// A key only English defines still renders rather than vanishing.
	if got := T(ctx, "nav.caches"); got != en["nav.caches"] {
		t.Errorf("de fallback = %q, want the English %q", got, en["nav.caches"])
	}
}
