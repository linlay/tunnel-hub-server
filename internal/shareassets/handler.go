package shareassets

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"strings"
)

const PublicPathPrefix = "/assets/conversation-export/"

var publicAssetPath = regexp.MustCompile(`^[a-f0-9]{64}/(?:[A-Za-z0-9._-]+/)*[A-Za-z0-9._-]+$`)

//go:embed files
var embeddedFiles embed.FS

type handler struct {
	files      fs.FS
	fileServer http.Handler
}

func NewHandler() http.Handler {
	files, err := fs.Sub(embeddedFiles, "files")
	if err != nil {
		panic("conversation export assets are unavailable: " + err.Error())
	}
	return &handler{
		files:      files,
		fileServer: http.StripPrefix(PublicPathPrefix, http.FileServer(http.FS(files))),
	}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	relativePath := strings.TrimPrefix(r.URL.Path, PublicPathPrefix)
	if relativePath == r.URL.Path || relativePath != path.Clean(relativePath) || !publicAssetPath.MatchString(relativePath) {
		http.NotFound(w, r)
		return
	}
	info, err := fs.Stat(h.files, relativePath)
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}

	header := w.Header()
	header.Set("Cache-Control", "public, max-age=31536000, immutable")
	header.Set("Access-Control-Allow-Origin", "*")
	header.Set("Cross-Origin-Resource-Policy", "cross-origin")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Content-Type", assetContentType(relativePath))
	h.fileServer.ServeHTTP(w, r)
}

func assetContentType(filename string) string {
	switch strings.ToLower(path.Ext(filename)) {
	case ".js":
		return "application/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".woff2":
		return "font/woff2"
	case ".woff":
		return "font/woff"
	case ".ttf":
		return "font/ttf"
	case ".txt":
		return "text/plain; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}
