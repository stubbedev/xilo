package views

import (
	"github.com/a-h/templ"
	lucide "github.com/kaugesaar/lucide-go"
)

// IconClass renders a Lucide icon at a given size with extra classes (used for
// the theme toggle's sun/moon and other sized glyphs). No children.
func IconClass(name string, size int, class string) templ.Component {
	return templ.Raw(lucide.Icon(name, map[string]any{"size": size, "class": "ic " + class}))
}

// rawIcon is the inline SVG for a 16px icon, used by the Icon templ component.
func rawIcon(name string) templ.Component {
	return templ.Raw(lucide.Icon(name, map[string]any{"size": 16, "class": "ic"}))
}
