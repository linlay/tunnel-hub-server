package admin

import (
	"embed"
	"net/http"
)

//go:embed share_templates_page/*
var shareTemplatesPage embed.FS

// The shell contains no management data. All reads and login use the existing
// admin API and its HttpOnly session cookie; no separate auth mechanism exists.
func (s *Server) serveShareTemplatesPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var file, contentType string
	switch r.URL.Path {
	case "/api/admin/share-templates/view":
		file, contentType = "index.html", "text/html; charset=utf-8"
	case "/api/admin/share-templates/view/app.js":
		file, contentType = "app.js", "text/javascript; charset=utf-8"
	case "/api/admin/share-templates/view/style.css":
		file, contentType = "style.css", "text/css; charset=utf-8"
	default:
		http.NotFound(w, r)
		return
	}
	body, err := shareTemplatesPage.ReadFile("share_templates_page/" + file)
	if err != nil {
		s.writeInternal(w, "read template management page", err)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	if r.Method == http.MethodGet {
		_, _ = w.Write(body)
	}
}
