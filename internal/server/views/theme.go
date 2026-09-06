package views

// Palette is one selectable color scheme. The id lands on <html data-palette>
// and selects a variable block in css/input.css; light/dark stays the
// browser-side toggle, so every palette ships both halves.
type Palette struct {
	ID, Name string
}

// Palettes is the Appearance picker, default first.
var Palettes = []Palette{
	{"", "Xilo"},
	{"ayu", "Ayu"},
	{"catppuccin", "Catppuccin"},
	{"dracula", "Dracula"},
	{"everforest", "Everforest"},
	{"github-dark", "GitHub Dark"},
	{"github-light", "GitHub Light"},
	{"gruvbox", "Gruvbox"},
	{"kanagawa", "Kanagawa"},
	{"nord", "Nord"},
	{"one-dark", "One Dark"},
	{"rosepine", "Rosepine"},
	{"solarized-dark", "Solarized Dark"},
	{"solarized-light", "Solarized Light"},
	{"tokyo-night", "Tokyo Night"},
}

// ValidPalette reports whether id names a shipped palette ("" included).
func ValidPalette(id string) bool {
	for _, p := range Palettes {
		if p.ID == id {
			return true
		}
	}
	return false
}
