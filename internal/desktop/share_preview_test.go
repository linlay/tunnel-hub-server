package desktop

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"example.invalid/tunnel-hub-server/internal/sharecontract"
)

func previewPackageExample(t *testing.T) (sharecontract.Envelope, map[string][]byte) {
	t.Helper()
	manifest := genericShareExample()
	manifest.Kind = "static-report"
	manifest.Title = "Frozen isolation package"
	manifest.Desktop = nil
	manifest.Preview = &sharecontract.PreviewSpec{Type: "package", Entrypoint: "index.html"}
	bodies := make(map[string][]byte)
	for _, name := range []string{"index.html", "script.js", "helper.js", "style.css", "data.json", "pixel.png", "font.woff2"} {
		var body []byte
		var err error
		switch name {
		case "pixel.png":
			body, err = base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+j7wAAAABJRU5ErkJggg==")
		case "font.woff2":
			files, globErr := filepath.Glob("../shareassets/files/*/fonts/KaTeX_Main-Regular.woff2")
			if globErr != nil || len(files) == 0 {
				t.Fatal("test font not found", globErr)
			}
			body, err = os.ReadFile(files[0])
		default:
			body, err = os.ReadFile(filepath.Join("testdata", "share-preview", name))
		}
		if err != nil {
			t.Fatal(err)
		}
		idHash := sha256.Sum256([]byte(name))
		digest := sha256.Sum256(body)
		id := hex.EncodeToString(idHash[:12])
		contentType := mime.TypeByExtension(filepath.Ext(name))
		if strings.Contains(contentType, ";") {
			contentType = strings.SplitN(contentType, ";", 2)[0]
		}
		if name == "font.woff2" {
			contentType = "font/woff2"
		}
		manifest.Resources = append(manifest.Resources, sharecontract.Resource{ID: id, Name: name, MIMEType: contentType, Size: int64(len(body)), SHA256: hex.EncodeToString(digest[:]), Path: name})
		bodies[id] = body
	}
	return manifest, bodies
}

func TestSharePreviewIsExplicitIsolatedAndRevocable(t *testing.T) {
	cfg := desktopTestConfig(t)
	cfg.SharePublicBaseURL = "https://share.example.test"
	cfg.ShareLimits.MaxSnapshotBytes = 8192
	cfg.ShareLimits.MaxResourceBytes = 1 << 20
	server, _ := newDesktopTestServerWithConfig(t, cfg)
	manifest, bodies := previewPackageExample(t)
	record := decodeGenericRecord(t, postGenericShare(t, server, manifest, bodies))
	base := sharecontract.PublicPath + record.ID
	outer := performConversationShareRequest(server, http.MethodGet, base, nil, "")
	if outer.Code != http.StatusOK || !strings.Contains(outer.Body.String(), `sandbox="allow-scripts" credentialless`) || strings.Contains(outer.Body.String(), "allow-same-origin") || strings.Contains(outer.Body.String(), "<script") {
		t.Fatal("unsafe package outer page", outer.Code)
	}
	root := cfg.SharePublicBaseURL + base + "/preview/"
	if !strings.Contains(outer.Header().Get("Content-Security-Policy"), "frame-src "+root) {
		t.Fatal("outer frame navigation is not scoped to this share")
	}
	if strings.Contains(outer.Body.String(), "://share/") {
		t.Fatal("receiver link invented without a receiver")
	}
	for _, resource := range manifest.Resources {
		request := httptest.NewRequest(http.MethodGet, base+"/preview/"+resource.Path, nil)
		request.Header.Set("Sec-Fetch-Dest", "empty")
		if resource.MIMEType == "text/html" {
			request.Header.Set("Sec-Fetch-Dest", "iframe")
		}
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != http.StatusOK || response.Header().Get("Access-Control-Allow-Origin") != "*" || response.Header().Get("Access-Control-Allow-Credentials") != "" || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("package resource cannot load anonymously", resource.Path, response.Code)
		}
		policy := response.Header().Get("Content-Security-Policy")
		for _, required := range []string{"sandbox allow-scripts", "script-src " + root, "connect-src " + root, "frame-src 'none'", "form-action 'none'", "frame-ancestors " + cfg.SharePublicBaseURL} {
			if !strings.Contains(policy, required) {
				t.Fatal("missing package policy", required)
			}
		}
		if strings.Contains(policy, "allow-same-origin") || strings.Contains(policy, "allow-popups") || strings.Contains(policy, "allow-downloads") {
			t.Fatal("package gained a host capability")
		}
	}
	if response := performConversationShareRequest(server, http.MethodGet, base+"/preview/index.html", nil, ""); response.Code != http.StatusForbidden {
		t.Fatal("package HTML ran outside trusted iframe")
	}
	for _, destination := range []string{"document", "object", "worker"} {
		request := httptest.NewRequest(http.MethodGet, base+"/preview/pixel.png", nil)
		request.Header.Set("Sec-Fetch-Dest", destination)
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Fatal("non-frame document bypassed outer policy", destination)
		}
	}
	for _, path := range []string{"../index.html", "%69ndex.html", "missing.js", "index.html?action=desktop"} {
		response := performConversationShareRequest(server, http.MethodGet, base+"/preview/"+path, nil, "")
		if response.Code != http.StatusNotFound && response.Code != http.StatusBadRequest {
			t.Fatal("invalid package path accepted", path, response.Code)
		}
	}
	resolved := performConversationShareRequest(server, http.MethodGet, base+"?action=resolve", nil, "")
	var resolution sharecontract.Resolution
	if err := json.Unmarshal(resolved.Body.Bytes(), &resolution); err != nil || len(resolution.Actions) != 2 {
		t.Fatal("package behaviors not advertised", err)
	}
	if response := performConversationShareRequest(server, http.MethodDelete, sharecontract.ManagementPath+"/"+record.ID, nil, defaultDesktopJWT); response.Code != http.StatusNoContent {
		t.Fatal(response.Code)
	}
	for _, resource := range manifest.Resources {
		request := httptest.NewRequest(http.MethodGet, base+"/preview/"+resource.Path, nil)
		request.Header.Set("Sec-Fetch-Dest", "iframe")
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound {
			t.Fatal("revoked package resource readable", resource.Path, response.Code)
		}
	}
}

func TestSharePreviewPageSeparatesAppActionsFromPackageFiles(t *testing.T) {
	cfg := desktopTestConfig(t)
	cfg.SharePublicBaseURL = "https://share.example.test"
	cfg.ProductName = "Test & Product"
	cfg.ProductDownloadPageURL = "https://product.example.test/download?platform=desktop&source=share"
	cfg.ShareLimits.MaxSnapshotBytes = 8192
	cfg.ShareLimits.MaxResourceBytes = 1 << 20
	server, _ := newDesktopTestServerWithConfig(t, cfg)
	manifest, bodies := previewPackageExample(t)
	manifest.Title = "Frozen <connector>"
	manifest.Desktop = genericShareExample().Desktop
	attachment := sharecontract.Resource{ID: "0123456789abcdef01234567", Name: "report & notes.pdf", MIMEType: "application/pdf", Size: 6}
	digest := sha256.Sum256([]byte("report"))
	attachment.SHA256 = hex.EncodeToString(digest[:])
	manifest.Resources = append(manifest.Resources, attachment)
	bodies[attachment.ID] = []byte("report")
	record := decodeGenericRecord(t, postGenericShare(t, server, manifest, bodies))
	base := sharecontract.PublicPath + record.ID
	for _, query := range []string{"", "?action=desktop"} {
		response := performConversationShareRequest(server, http.MethodGet, base+query, nil, "")
		body := response.Body.String()
		for _, expected := range []string{
			`href="` + cfg.BrandID + "://share/" + record.ID + `?action=desktop"`,
			`>在 Test &amp; Product 中打开</a>`, `>下载 Test &amp; Product</a>`,
			`href="https://product.example.test/download?platform=desktop&amp;source=share"`,
			`resourceId=` + attachment.ID + `">report &amp; notes.pdf</a>`,
		} {
			if response.Code != http.StatusOK || !strings.Contains(body, expected) {
				t.Fatal("missing share action or attachment", expected, response.Code)
			}
		}
		for _, resource := range manifest.Resources[:len(manifest.Resources)-1] {
			if strings.Contains(body, "resourceId="+resource.ID) {
				t.Fatal("package dependency shown as a download", resource.Path)
			}
		}
		if strings.Contains(body, "<connector>") || strings.Contains(body, "<script") || query == "" && strings.Contains(body, "<h1>") {
			t.Fatal("unsafe or duplicate outer content")
		}
	}
	server.Config.ProductDownloadPageURL = ""
	response := performConversationShareRequest(server, http.MethodGet, base, nil, "")
	if strings.Contains(response.Body.String(), "下载") || !strings.Contains(response.Body.String(), "在 Test &amp; Product 中打开") {
		t.Fatal("missing download configuration changed the open action")
	}
}
