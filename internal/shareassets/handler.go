package shareassets

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"html"
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
const localBrandIDMarker = "__CONVERSATION_EXPORT_LOCAL_BRAND_ID__"
const publicBrandMeta = `<meta name="conversation-export-public-brand" content="" />`

var relativeAssetPath = regexp.MustCompile(`^(?:[A-Za-z0-9._-]+/)*[A-Za-z0-9._-]+$`)
var brandScheme = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
var reservedSchemes = map[string]bool{"http": true, "https": true, "javascript": true, "data": true, "vbscript": true, "file": true, "blob": true}

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
		bytes.Count(template, []byte(assetOriginMarker)) == 0 ||
		bytes.Count(template, []byte(localBrandIDMarker)) != 1 {
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

func (b *Bundle) Render(snapshot []byte, assetOrigin, brandID, productName, productDownloadPageURL string) ([]byte, error) {
	if !json.Valid(snapshot) {
		return nil, errors.New("conversation snapshot is invalid")
	}
	var envelope struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(snapshot, &envelope); err != nil {
		return nil, err
	}
	if envelope.Version != 1 {
		return nil, errors.New("unsupported conversation snapshot version")
	}
	origin, err := normalizedOrigin(assetOrigin)
	if err != nil {
		return nil, err
	}
	var escaped bytes.Buffer
	json.HTMLEscape(&escaped, snapshot)
	htmlBytes := bytes.Replace(b.template, []byte(snapshotMarker), escaped.Bytes(), 1)
	htmlBytes = bytes.ReplaceAll(htmlBytes, []byte(assetOriginMarker), []byte(origin))
	if !brandScheme.MatchString(brandID) || reservedSchemes[brandID] || strings.TrimSpace(productName) == "" {
		return nil, errors.New("public share brand is invalid")
	}
	if bytes.Count(htmlBytes, []byte(publicBrandMeta)) != 1 {
		return nil, errors.New("public share brand placeholder is unavailable")
	}
	downloadPageURL, err := normalizedProductDownloadPageURL(productDownloadPageURL)
	if err != nil {
		return nil, err
	}
	brandJSON, err := json.Marshal(struct {
		ID              string `json:"id"`
		ProductName     string `json:"productName"`
		OpenScheme      string `json:"openScheme"`
		DownloadPageURL string `json:"downloadPageUrl,omitempty"`
	}{brandID, productName, brandID, downloadPageURL})
	if err != nil {
		return nil, err
	}
	brandMeta := `<meta name="conversation-export-public-brand" content="` + html.EscapeString(string(brandJSON)) + `" />`
	htmlBytes = bytes.Replace(htmlBytes, []byte(publicBrandMeta), []byte(brandMeta), 1)
	htmlBytes = bytes.Replace(htmlBytes, []byte(localBrandIDMarker), nil, 1)
	return htmlBytes, nil
}

func normalizedProductDownloadPageURL(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil {
		return "", errors.New("product download page URL is invalid")
	}
	hostname := strings.Trim(strings.ToLower(parsed.Hostname()), "[]")
	loopback := hostname == "localhost" || hostname == "127.0.0.1" || hostname == "::1"
	if parsed.Scheme != "https" && !(loopback && parsed.Scheme == "http") {
		return "", errors.New("product download page URL is invalid")
	}
	return parsed.String(), nil
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
	assetSet, assetPath, found := strings.Cut(relativePath, "/")
	if relativePath == r.URL.Path || !found ||
		!regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(assetSet) ||
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
	case ".svg":
		return "image/svg+xml"
	default:
		return "application/octet-stream"
	}
}
