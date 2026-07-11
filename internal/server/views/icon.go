package views

import (
	"github.com/a-h/templ"
	"github.com/templui/templui/components/icon"
)

// iconSize maps a pixel size to its Tailwind class. Literal strings so the
// Tailwind scanner (which sources this file) sees them.
var iconSize = map[int]string{12: "size-3", 16: "size-4", 20: "size-5", 32: "size-8", 48: "size-12"}

// IconClass renders a templui/Lucide icon at a given size with extra classes
// (used for the theme toggle's sun/moon and other sized glyphs). No children.
func IconClass(name string, size int, class string) templ.Component {
	return icon.Icon(name)(icon.Props{Class: iconSize[size] + " " + class})
}

// rawIcon is the inline SVG for a 16px icon, used by the Icon templ component.
func rawIcon(name string) templ.Component {
	return icon.Icon(name)(icon.Props{Class: "size-4"})
}
