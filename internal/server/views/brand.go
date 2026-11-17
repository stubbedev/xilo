package views

import (
	_ "embed"

	"github.com/a-h/templ"
)

var (
	//go:embed mark.svg
	markSVG string
	//go:embed mark-animated.svg
	markAnimatedSVG string

	mark         = templ.Raw(markSVG)
	markAnimated = templ.Raw(markAnimatedSVG)
)
