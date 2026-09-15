package shareassets

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
)

const PublicPathPrefix = "/assets/conversation-export/"
const TemplatePublicPath = PublicPathPrefix + "conversation.template.html"

const snapshotMarker = "__CONVERSATION_EXPORT_SNAPSHOT_JSON_V1__"
const assetOriginMarker = "__CONVERSATION_EXPORT_ASSET_ORIGIN__"

var relativeAssetPath = regexp.MustCompile(`^(?:[A-Za-z0-9._-]+/)*[A-Za-z0-9._-]+$`)

//go:embed files conversation.template.html conversation-assets.json
var embeddedFiles embed.FS

type assetManifest struct {
	Profile  string `json:"profile"`
	AssetSet string `json:"assetSet"`
	Files    []struct {
		Path string `json:"path"`
	} `json:"files"`
}

type Bundle struct {
	assetSet   string
	template   []byte
	files      fs.FS
	fileServer http.Handler
}

func NewBundle() *Bundle {
	manifestBytes, err := embeddedFiles.ReadFile("conversation-assets.json")
	if err != nil {
		panic("conversation export manifest is unavailable: " + err.Error())
	}
	var manifest assetManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil ||
		manifest.Profile != "conversation-export-assets" ||
		!regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(manifest.AssetSet) ||
		len(manifest.Files) == 0 {
		panic("conversation export manifest is invalid")
	}
	template, err := embeddedFiles.ReadFile("conversation.template.html")
	if err != nil || bytes.Count(template, []byte(snapshotMarker)) != 1 ||
		bytes.Count(template, []byte(assetOriginMarker)) == 0 {
		panic("conversation export template is invalid")
	}
	files, err := fs.Sub(embeddedFiles, "files")
	if err != nil {
		panic("conversation export assets are unavailable: " + err.Error())
	}
	for _, file := range manifest.Files {
		if !relativeAssetPath.MatchString(file.Path) {
			panic("conversation export manifest contains an invalid asset path")
		}
		info, statErr := fs.Stat(files, path.Join(manifest.AssetSet, file.Path))
		if statErr != nil || !info.Mode().IsRegular() {
			panic("conversation export manifest references a missing asset")
		}
	}
	return &Bundle{
		assetSet:   manifest.AssetSet,
		template:   template,
		files:      files,
		fileServer: http.StripPrefix(PublicPathPrefix, http.FileServer(http.FS(files))),
	}
}

func NewHandler() http.Handler {
	return NewBundle()
}

func (b *Bundle) Render(snapshot []byte, assetOrigin string) ([]byte, error) {
	if !json.Valid(snapshot) {
		return nil, errors.New("conversation snapshot is invalid")
	}
	origin, err := normalizedOrigin(assetOrigin)
	if err != nil {
		return nil, err
	}
	var escaped bytes.Buffer
	json.HTMLEscape(&escaped, snapshot)
	html := bytes.Replace(b.template, []byte(snapshotMarker), escaped.Bytes(), 1)
	html = bytes.ReplaceAll(html, []byte(assetOriginMarker), []byte(origin))
	return html, nil
}

func (b *Bundle) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path == TemplatePublicPath {
		serveTemplate(w, r, b.template)
		return
	}
	relativePath := strings.TrimPrefix(r.URL.Path, PublicPathPrefix)
	wantPrefix := b.assetSet + "/"
	assetPath := strings.TrimPrefix(relativePath, wantPrefix)
	if relativePath == r.URL.Path || !strings.HasPrefix(relativePath, wantPrefix) ||
		assetPath != path.Clean(assetPath) || !relativeAssetPath.MatchString(assetPath) {
		http.NotFound(w, r)
		return
	}
	info, err := fs.Stat(b.files, relativePath)
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
	b.fileServer.ServeHTTP(w, r)
}

func serveTemplate(w http.ResponseWriter, r *http.Request, template []byte) {
	header := w.Header()
	header.Set("Cache-Control", "no-store")
	header.Set("Access-Control-Allow-Origin", "*")
	header.Set("Cross-Origin-Resource-Policy", "cross-origin")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Content-Type", "text/html; charset=utf-8")
	header.Set("Content-Length", strconv.Itoa(len(template)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(template)
	}
}

func normalizedOrigin(value string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil ||
		(parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Path != "" ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("conversation asset origin is invalid")
	}
	return parsed.Scheme + "://" + parsed.Host, nil
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
