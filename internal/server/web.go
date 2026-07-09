package server

import (
	"embed"
	"fmt"
	"net/http"
	"path"
)

//go:embed static
var staticFS embed.FS

// registerStatic serves the embedded assets as EXACT routes. A /static/ subtree
// would be ambiguous with the root /{cache}/… wildcard (e.g.
// /static/nix-cache-info matches both), so each file is its own literal path,
// which the router treats as strictly more specific.
func (s *Server) registerStatic(mux *http.ServeMux) {
	types := map[string]string{".css": "text/css; charset=utf-8", ".js": "application/javascript; charset=utf-8"}
	for _, name := range []string{"pico.min.css", "xilo.css", "alpine.min.js"} {
		data, err := staticFS.ReadFile("static/" + name)
		if err != nil {
			panic(err)
		}
		ct := types[path.Ext(name)]
		mux.HandleFunc("GET /static/"+name, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", ct)
			w.Header().Set("Cache-Control", "public, max-age=3600")
			w.Write(data)
		})
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
