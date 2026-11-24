package admin

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"example.invalid/tunnel-hub-server/internal/shareassets"
)

func templateUploadFixture(t *testing.T, root, change, commit string) ([]byte, string) {
	t.Helper()
	selected, err := os.ReadFile(filepath.Join(root, "current"))
	if err != nil {
		t.Fatal(err)
	}
	old := strings.TrimSpace(string(selected))
	base := filepath.Join(root, old)
	raw, err := os.ReadFile(filepath.Join(base, "conversation-assets.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err = json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	html, err := os.ReadFile(filepath.Join(base, "conversation.template.html"))
	if err != nil {
		t.Fatal(err)
	}
	normalized := strings.ReplaceAll(string(html), old, "__CONVERSATION_EXPORT_ASSET_SET__") + change
	digest := sha256.Sum256([]byte(normalized))
	records := []string{"conversation.template.html\x00" + hex.EncodeToString(digest[:]) + "\n"}
	for _, item := range manifest["files"].([]any) {
		f := item.(map[string]any)
		records = append(records, "assets/"+f["path"].(string)+"\x00"+f["sha256"].(string)+"\n")
	}
	sort.Strings(records)
	digest = sha256.Sum256([]byte("conversation-export-package\x00" + strings.Join(records, "")))
	id := hex.EncodeToString(digest[:])
	manifest["assetSet"] = id
	if commit != "" {
		manifest["build"] = map[string]any{"commit": commit, "dirty": true}
	}
	raw, _ = json.Marshal(manifest)
	var out bytes.Buffer
	z := zip.NewWriter(&out)
	write := func(name string, data []byte) {
		w, err := z.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	write("conversation-assets.json", raw)
	write("conversation.template.html", []byte(strings.ReplaceAll(normalized, "__CONVERSATION_EXPORT_ASSET_SET__", id)))
	for _, item := range manifest["files"].([]any) {
		name := item.(map[string]any)["path"].(string)
		data, err := os.ReadFile(filepath.Join(base, "assets", old, name))
		if err != nil {
			t.Fatal(err)
		}
		write("assets/"+id+"/"+name, data)
	}
	if err = z.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes(), id
}
func templateWriteRequest(path string, data []byte) *http.Request {
	r := authedAdminRequest("POST", path, "")
	r.Body = io.NopCloser(bytes.NewReader(data))
	r.ContentLength = int64(len(data))
	r.Header.Set("Content-Type", "application/zip")
	r.Header.Set("X-Template-Action", "1")
	return r
}
func TestTemplateUploadActivateAndRecovery(t *testing.T) {
	server, _ := newAdminTestServer(t)
	root := t.TempDir()
	bundle := shareassets.NewManagedBundle(root)
	server.SetShareTemplates(bundle)
	initial := bundle.CurrentAssetSet()
	temp := t.TempDir()
	t.Setenv("TMPDIR", temp)
	data, id := templateUploadFixture(t, root, "<!--new HTML-->", strings.Repeat("a", 40))
	w := httptest.NewRecorder()
	server.ServeHTTP(w, templateWriteRequest("/api/admin/share-templates/upload", data))
	if w.Code != 200 || bundle.CurrentAssetSet() != initial || !strings.Contains(w.Body.String(), id) {
		t.Fatal(w.Code, w.Body.String())
	}
	// Re-upload same bytes with different provenance, retaining the first record.
	second, same := templateUploadFixture(t, root, "<!--new HTML-->", strings.Repeat("b", 40))
	if same != id {
		t.Fatal("source changed identity")
	}
	w = httptest.NewRecorder()
	server.ServeHTTP(w, templateWriteRequest("/api/admin/share-templates/upload", second))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"alreadyExists":true`) {
		t.Fatal(w.Code, w.Body.String())
	}
	versions, _ := bundle.Versions()
	for _, v := range versions {
		if v.AssetSet == id && (v.Build == nil || v.Build.Commit == nil || *v.Build.Commit != strings.Repeat("a", 40)) {
			t.Fatal("lost first source")
		}
	}
	w = httptest.NewRecorder()
	server.ServeHTTP(w, templateWriteRequest("/api/admin/share-templates/"+id+"/activate", nil))
	if w.Code != 200 || bundle.CurrentAssetSet() != id {
		t.Fatal(w.Code, w.Body.String())
	}
	if err := os.WriteFile(filepath.Join(root, id, "conversation.template.html"), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	bundle = shareassets.NewManagedBundle(root)
	server.SetShareTemplates(bundle)
	w = httptest.NewRecorder()
	server.ServeHTTP(w, authedAdminRequest("GET", "/api/admin/share-templates", ""))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"error"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	server.ServeHTTP(w, templateWriteRequest("/api/admin/share-templates/"+initial+"/activate", nil))
	if w.Code != 200 || bundle.CurrentAssetSet() != initial {
		t.Fatal(w.Code, w.Body.String())
	}
	if entries, err := os.ReadDir(temp); err != nil || len(entries) != 0 {
		t.Fatal("temporary ZIP leak", entries, err)
	}
}
func TestTemplateWriteBoundaries(t *testing.T) {
	server, _ := newAdminTestServer(t)
	root := t.TempDir()
	bundle := shareassets.NewManagedBundle(root)
	server.SetShareTemplates(bundle)
	initial := bundle.CurrentAssetSet()
	for _, tc := range []struct {
		name   string
		status int
		mutate func(*http.Request)
	}{
		{"anonymous", 401, func(r *http.Request) { r.Header.Del("Authorization") }},
		{"missing-action", 403, func(r *http.Request) { r.Header.Del("X-Template-Action") }},
		{"cross-origin", 403, func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }},
		{"forwarded-origin", 403, func(r *http.Request) {
			r.Header.Set("Origin", "https://evil.example")
			r.Header.Set("X-Forwarded-Host", "evil.example")
			r.Header.Set("X-Forwarded-Proto", "https")
		}},
		{"method", 405, func(r *http.Request) { r.Method = "GET" }},
		{"media", 415, func(r *http.Request) { r.Header.Set("Content-Type", "application/json") }},
		{"size", 413, func(r *http.Request) { r.ContentLength = shareassets.MaxUploadBytes + 1 }},
		{"bad-zip", 400, func(r *http.Request) {}},
		{"query", 400, func(r *http.Request) { r.URL.RawQuery = "assetSet=other" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := templateWriteRequest("/api/admin/share-templates/upload", []byte("bad"))
			tc.mutate(r)
			w := httptest.NewRecorder()
			server.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatal(w.Code, w.Body.String())
			}
		})
	}
	for _, id := range []string{strings.Repeat("f", 64), "bad"} {
		w := httptest.NewRecorder()
		server.ServeHTTP(w, templateWriteRequest("/api/admin/share-templates/"+id+"/activate", nil))
		if w.Code != 404 && w.Code != 405 {
			t.Fatal(w.Code)
		}
	}
	// Unknown-length bodies are limited during streaming too.
	r := templateWriteRequest("/api/admin/share-templates/upload", nil)
	r.ContentLength = -1
	r.Body = io.NopCloser(io.LimitReader(zeroReader{}, shareassets.MaxUploadBytes+1))
	w := httptest.NewRecorder()
	server.ServeHTTP(w, r)
	if w.Code != 413 {
		t.Fatal(w.Code, w.Body.String())
	}
	if bundle.CurrentAssetSet() != initial {
		t.Fatal("rejected request changed current")
	}
	if err := os.Chmod(root, 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(root, 0700)
	if bundle.UploadEnabled() {
		t.Fatal("read-only root enabled")
	}
	w = httptest.NewRecorder()
	valid, _ := templateUploadFixture(t, root, "", "")
	server.ServeHTTP(w, templateWriteRequest("/api/admin/share-templates/upload", valid))
	if w.Code != 503 {
		t.Fatal(fmt.Sprint(w.Code, w.Body.String()))
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }
