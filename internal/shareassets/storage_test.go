package shareassets

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testPackage(t *testing.T, change string, bootstrapSuffix ...string) (map[string][]byte, string) {
	t.Helper()
	raw, _ := embeddedFiles.ReadFile("conversation-assets.json")
	var m assetManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.Build == nil {
		m.Build = &BuildSource{}
	}
	html, _ := embeddedFiles.ReadFile("conversation.template.html")
	normalized, err := normalizeVersion(html, m.AssetSet)
	if err != nil {
		t.Fatal(err)
	}
	normalized = append(normalized, change...)
	records := map[string]string{}
	data := map[string][]byte{}
	for i, f := range m.Files {
		body, err := embeddedFiles.ReadFile("files/" + m.AssetSet + "/" + f.Path)
		if err != nil {
			t.Fatal(err)
		}
		if f.Path == "html-preview-bootstrap.js" && len(bootstrapSuffix) > 0 {
			body = append(body, strings.Join(bootstrapSuffix, "")...)
			m.Files[i].Bytes = int64(len(body))
			m.Files[i].SHA256 = digestHex(body)
			m.Files[i].Integrity = fileIntegrity(body)
		}
		records[f.Path] = digestHex(body)
		data[f.Path] = body
	}
	id := packageDigest(normalized, records)
	html = bytes.ReplaceAll(normalized, []byte(assetSetMarker), []byte(id))
	m.AssetSet = id
	raw, _ = json.Marshal(m)
	out := map[string][]byte{"conversation.template.html": html, "conversation-assets.json": raw}
	for name, body := range data {
		out["assets/"+id+"/"+name] = body
	}
	return out, id
}
func zipBytes(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var out bytes.Buffer
	z := zip.NewWriter(&out)
	for name, data := range files {
		w, err := z.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
func uploadTest(t *testing.T, b *Bundle, files map[string][]byte) (string, bool, error) {
	t.Helper()
	data := zipBytes(t, files)
	return b.Upload(bytes.NewReader(data), int64(len(data)))
}
func TestPackageDigestContract(t *testing.T) {
	html := `<meta name="conversation-export-asset-set" content="` + assetSetMarker + `" />` + "\n" + `<link href="__CONVERSATION_EXPORT_ASSET_ORIGIN__/assets/conversation-export/` + assetSetMarker + `/runtime.css">` + "\n" + `<script src="__CONVERSATION_EXPORT_ASSET_ORIGIN__/assets/conversation-export/` + assetSetMarker + `/runtime.js"></script>` + "\n"
	got := packageDigest([]byte(html), map[string]string{"runtime.js": digestHex([]byte("hello\n")), "runtime.css": digestHex([]byte("body{}\n"))})
	if got != "daa33d629a573d1819a4fb5f32f570e59cb57f9c509de32a2feca53658eed02a" {
		t.Fatal(got)
	}
}

func TestHTMLPreviewStaysWithParentVersionAfterActivation(t *testing.T) {
	root := t.TempDir()
	b := NewManagedBundle(root)
	initial := b.CurrentAssetSet()
	files, next := testPackage(t, "<!--next-->", "\n// different bootstrap\n")
	if _, _, err := uploadTest(t, b, files); err != nil {
		t.Fatal(err)
	}
	parent, rendered, err := b.RenderVersion([]byte(`{"version":1}`), "https://share.example.test", "test", "Test", "", false)
	if err != nil || rendered != initial || !bytes.Contains(parent, []byte(initial+"/runtime.js")) {
		t.Fatal("parent version", rendered, err)
	}
	if err := b.Activate(next); err != nil {
		t.Fatal(err)
	}
	b = NewManagedBundle(root) // History must also survive a restart.
	assertPreview := func(requested, expected string) {
		t.Helper()
		body, err := b.RenderHTMLPreview("https://share.example.test", requested)
		if err != nil || !bytes.Contains(body, []byte(PublicPathPrefix+expected+"/html-preview-bootstrap.js")) {
			t.Fatalf("preview %s: %s %v", requested, body, err)
		}
		rec := httptest.NewRecorder()
		b.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, PublicPathPrefix+expected+"/html-preview-bootstrap.js", nil))
		if rec.Code != http.StatusOK || !bytes.Contains(body, []byte(`integrity="`+fileIntegrity(rec.Body.Bytes())+`"`)) {
			t.Fatal("bootstrap bytes and SRI do not match", rec.Code)
		}
	}
	assertPreview(initial, initial)
	assertPreview(next, next)
	legacy := NewBundle().assetSet
	assertPreview(legacy, legacy) // Old embedded resource URL, independent of current.
	assertPreview("", legacy)     // Unversioned clients never follow a new bootstrap protocol.
	for _, unknown := range []string{strings.Repeat("0", 64), "current", "../current"} {
		if _, err := b.RenderHTMLPreview("https://share.example.test", unknown); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("missing version fell back to current: %s %v", unknown, err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "current"), []byte("missing\n"), 0600); err != nil {
		t.Fatal(err)
	}
	b = NewManagedBundle(root)
	assertPreview(initial, initial) // Broken current does not break an already opened page.
	assertPreview("", legacy)
}
func TestManagedSeedPreservesEmbeddedRenderer(t *testing.T) {
	root := t.TempDir()
	embedded := NewBundle()
	b := NewManagedBundle(root)
	initial := b.CurrentAssetSet()
	active, err := b.selected()
	if err != nil {
		t.Fatal(err)
	}
	// The old source manifest need not use the upload format. The persisted
	// seed must satisfy it without importing any newer WebClient resources.
	_, version, err := loadDiskPackage(filepath.Join(root, initial), initial)
	if err != nil || !version.CanActivate || version.Build == nil {
		t.Fatal(version, err)
	}
	oldHTML, err := normalizeVersion(embedded.template, embedded.assetSet)
	if err != nil {
		t.Fatal(err)
	}
	newHTML, err := normalizeVersion(active.template, initial)
	if err != nil || !bytes.Equal(oldHTML, newHTML) {
		t.Fatal("seed changed renderer HTML", err)
	}
	for name := range active.assetPaths {
		want, err := fs.ReadFile(embedded.files, embedded.assetSet+"/"+name)
		if err != nil {
			t.Fatal(err)
		}
		got, err := fs.ReadFile(active.files, initial+"/"+name)
		if err != nil || !bytes.Equal(want, got) {
			t.Fatal("seed changed resource", name, err)
		}
	}
	files, id := testPackage(t, "<!--independent release-->")
	if _, _, err = uploadTest(t, b, files); err != nil {
		t.Fatal(err)
	}
	if err = b.Activate(id); err != nil {
		t.Fatal(err)
	}
	b = NewManagedBundle(root)
	if b.CurrentAssetSet() != id {
		t.Fatal("restart reset uploaded selection")
	}
	for _, historical := range []string{embedded.assetSet, initial} {
		w := httptest.NewRecorder()
		b.ServeHTTP(w, httptest.NewRequest("GET", PublicPathPrefix+historical+"/runtime.js", nil))
		want, _ := fs.ReadFile(embedded.files, embedded.assetSet+"/runtime.js")
		if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), want) {
			t.Fatal("historical resource changed", historical, w.Code)
		}
	}
	if err = b.Activate(initial); err != nil {
		t.Fatal("cannot roll back to seed", err)
	}
}
func TestManagedLifecycleAndFirstSource(t *testing.T) {
	root := t.TempDir()
	b := NewManagedBundle(root)
	initial := b.CurrentAssetSet()
	if initial == "" {
		t.Fatal(b.managed.err)
	}
	files, id := testPackage(t, "<!-- changed HTML -->")
	if got, exists, err := uploadTest(t, b, files); err != nil || exists || got != id {
		t.Fatal(got, exists, err)
	}
	if b.CurrentAssetSet() != initial || NewManagedBundle(root).CurrentAssetSet() != initial {
		t.Fatal("upload activated")
	}
	original, err := os.ReadFile(filepath.Join(root, id, "conversation-assets.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m assetManifest
	_ = json.Unmarshal(files["conversation-assets.json"], &m)
	commit := strings.Repeat("b", 40)
	m.Build.Commit = &commit
	m.Build.Dirty = nil
	files["conversation-assets.json"], _ = json.Marshal(m)
	if _, exists, err := uploadTest(t, b, files); err != nil || !exists {
		t.Fatal(exists, err)
	}
	after, _ := os.ReadFile(filepath.Join(root, id, "conversation-assets.json"))
	if !bytes.Equal(original, after) {
		t.Fatal("source replaced")
	}
	if err = b.Activate(id); err != nil {
		t.Fatal(err)
	}
	reopened := NewManagedBundle(root)
	if reopened.CurrentAssetSet() != id {
		t.Fatal("lost selection")
	}
	if err = reopened.Activate(initial); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, id, "conversation.template.html"), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err = uploadTest(t, b, files); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	reopened = NewManagedBundle(root)
	if _, err = reopened.selected(); err != nil {
		t.Fatal("noncurrent corruption affected current")
	}
}
func TestRejectInvalidPackages(t *testing.T) {
	root := t.TempDir()
	b := NewManagedBundle(root)
	initial := b.CurrentAssetSet()
	mutations := map[string]func(map[string][]byte, string){
		"traversal": func(f map[string][]byte, id string) { f["../escape"] = []byte("bad") },
		"extra":     func(f map[string][]byte, id string) { f["extra"] = []byte("bad") },
		"missing":   func(f map[string][]byte, id string) { delete(f, "assets/"+id+"/runtime.css") },
		"digest":    func(f map[string][]byte, id string) { f["assets/"+id+"/runtime.css"] = []byte("bad") },
		"version": func(f map[string][]byte, id string) {
			f["conversation.template.html"] = bytes.Replace(f["conversation.template.html"], []byte(id), []byte(strings.Repeat("a", 64)), 1)
		},
		"sri": func(f map[string][]byte, id string) {
			var m assetManifest
			_ = json.Unmarshal(f["conversation-assets.json"], &m)
			m.Files[0].Integrity = "wrong"
			f["conversation-assets.json"], _ = json.Marshal(m)
		},
		"no-build": func(f map[string][]byte, id string) {
			var m assetManifest
			_ = json.Unmarshal(f["conversation-assets.json"], &m)
			m.Build = nil
			f["conversation-assets.json"], _ = json.Marshal(m)
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			files, id := testPackage(t, "<!--bad test-->")
			mutate(files, id)
			if _, _, err := uploadTest(t, b, files); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	for _, mode := range []fs.FileMode{os.ModeSymlink | 0777, os.ModeDevice | 0600} {
		var buf bytes.Buffer
		z := zip.NewWriter(&buf)
		h := &zip.FileHeader{Name: "special"}
		h.SetMode(mode)
		w, _ := z.CreateHeader(h)
		_, _ = w.Write([]byte("outside"))
		_ = z.Close()
		if _, _, err := b.Upload(bytes.NewReader(buf.Bytes()), int64(buf.Len())); err == nil {
			t.Fatal("special accepted")
		}
	}
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	_, _ = z.Create("same")
	_, _ = z.Create("same")
	_ = z.Close()
	if _, _, err := b.Upload(bytes.NewReader(buf.Bytes()), int64(buf.Len())); err == nil {
		t.Fatal("duplicate accepted")
	}
	if _, _, err := b.Upload(bytes.NewReader(nil), MaxUploadBytes+1); !errors.Is(err, ErrTooLarge) {
		t.Fatal(err)
	}
	if _, _, err := uploadTest(t, b, map[string][]byte{"bomb": make([]byte, maxFileBytes+1)}); !errors.Is(err, ErrTooLarge) {
		t.Fatal(err)
	}
	if b.CurrentAssetSet() != initial {
		t.Fatal("selection changed")
	}
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Fatal("leaked stage", e.Name())
		}
	}
}
func TestCurrentErrorsAndExplicitRecovery(t *testing.T) {
	for _, scenario := range []string{"missing", "illegal", "missing-target", "corrupt-current"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			b := NewManagedBundle(root)
			initial := b.CurrentAssetSet()
			files, id := testPackage(t, "<!--good-->")
			_, _, err := uploadTest(t, b, files)
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "missing":
				_ = os.Remove(filepath.Join(root, "current"))
			case "illegal":
				_ = os.WriteFile(filepath.Join(root, "current"), []byte("bad"), 0600)
			case "missing-target":
				_ = os.WriteFile(filepath.Join(root, "current"), []byte(strings.Repeat("f", 64)), 0600)
			case "corrupt-current":
				_ = os.WriteFile(filepath.Join(root, initial, "conversation.template.html"), []byte("bad"), 0600)
			}
			b = NewManagedBundle(root)
			if _, err = b.selected(); err == nil {
				t.Fatal("silently recovered")
			}
			w := httptest.NewRecorder()
			b.ServeHTTP(w, httptest.NewRequest(http.MethodGet, PublicPathPrefix+id+"/runtime.js", nil))
			if w.Code != 200 {
				t.Fatal(w.Code)
			}
			w = httptest.NewRecorder()
			b.ServeHTTP(w, httptest.NewRequest(http.MethodGet, TemplatePublicPath, nil))
			if w.Code != 503 {
				t.Fatal(w.Code)
			}
			if err = b.Activate(id); err != nil {
				t.Fatal(err)
			}
			if _, err = b.selected(); err != nil {
				t.Fatal(err)
			}
		})
	}
	root := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(root, []byte("x"), 0600)
	b := NewManagedBundle(root)
	if b.UploadEnabled() {
		t.Fatal("file is writable store")
	}
	if _, err := b.Versions(); err == nil {
		t.Fatal("missing root error")
	}
}
func TestDurabilityFailures(t *testing.T) {
	for _, step := range []string{"file-sync", "directory-sync", "package-rename", "package-parent-sync", "current-write", "current-sync", "current-rename", "current-parent-sync"} {
		t.Run(step, func(t *testing.T) {
			root := t.TempDir()
			b := NewManagedBundle(root)
			initial := b.CurrentAssetSet()
			files, id := testPackage(t, "<!--fault-->")
			if strings.HasPrefix(step, "current") {
				if _, _, err := uploadTest(t, b, files); err != nil {
					t.Fatal(err)
				}
			}
			b.managed.before = func(at string) error {
				if at == step {
					return errors.New("injected")
				}
				return nil
			}
			var err error
			if strings.HasPrefix(step, "current") {
				err = b.Activate(id)
			} else {
				_, _, err = uploadTest(t, b, files)
			}
			if err == nil {
				t.Fatal("false success")
			}
			if step == "current-parent-sync" {
				if _, err = b.selected(); err == nil {
					t.Fatal("uncertain output served")
				}
			} else if b.CurrentAssetSet() != initial {
				t.Fatal("selection changed")
			}
			if step == "package-parent-sync" {
				if _, err = os.Stat(filepath.Join(root, id)); err != nil {
					t.Fatal("committed data removed")
				}
			}
			b.managed.before = nil
			if _, _, err = uploadTest(t, b, files); err != nil {
				t.Fatal(err)
			}
			if err = b.Activate(id); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestConcurrentManagedOperations(t *testing.T) {
	b := NewManagedBundle(t.TempDir())
	initial := b.CurrentAssetSet()
	files, id := testPackage(t, "<!--concurrent-->")
	data := zipBytes(t, files)
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := b.Upload(bytes.NewReader(data), int64(len(data))); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, target := range []string{id, initial} {
				if err := b.Activate(target); err != nil {
					t.Error(err)
				}
				_, _ = b.Versions()
				html, rendered, err := b.RenderVersion([]byte(`{"version":1}`), "https://share.example.test", "example", "Example", "", true)
				if err != nil || !bytes.Contains(html, []byte(rendered+"/runtime.js")) || !bytes.Contains(html, []byte(rendered+"/runtime.css")) {
					t.Error("mixed render", rendered, err)
				}
			}
		}()
	}
	wg.Wait()
}

func TestUploadRealWebClientZIP(t *testing.T) {
	name := os.Getenv("CONVERSATION_EXPORT_TEST_ZIP")
	if name == "" {
		t.Skip("real ZIP supplied by integration command")
	}
	f, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	b := NewManagedBundle(root)
	initial := b.CurrentAssetSet()
	id, exists, err := b.Upload(f, info.Size())
	if err != nil || id == "" || b.CurrentAssetSet() != initial {
		t.Fatal(id, exists, err)
	}
	if duplicate, exists, err := b.Upload(f, info.Size()); err != nil || !exists || duplicate != id {
		t.Fatal(duplicate, exists, err)
	}
	if err = b.Activate(id); err != nil {
		t.Fatal(err)
	}
	b = NewManagedBundle(root)
	html, rendered, err := b.RenderVersion([]byte(`{"version":1}`), "https://share.example.test", "example", "Example", "", true)
	if err != nil || rendered != id || !bytes.Contains(html, []byte(id+"/runtime.js")) {
		t.Fatal("real ZIP not selected after restart", rendered, err)
	}
	if err = b.Activate(initial); err != nil {
		t.Fatal(err)
	}
	// All resources of the already rendered HTML remain available after switching
	// current: lazy JS, CSS, fonts and icons, not just the entrypoint scripts.
	published := b.managed.bundles[id]
	for name := range published.assetPaths {
		want, err := fs.ReadFile(published.files, id+"/"+name)
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		b.ServeHTTP(w, httptest.NewRequest("GET", PublicPathPrefix+id+"/"+name, nil))
		if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), want) || !strings.Contains(w.Header().Get("Cache-Control"), "immutable") {
			t.Fatal(name, w.Code)
		}
	}
	preview, err := b.RenderHTMLPreview("https://share.example.test", id)
	if err != nil || !bytes.Contains(preview, []byte(id+"/html-preview-bootstrap.js")) {
		t.Fatal("real ZIP preview changed after switching current", err)
	}
}

type slowResourceWriter struct {
	*httptest.ResponseRecorder
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *slowResourceWriter) Write(data []byte) (int, error) {
	w.once.Do(func() { close(w.started); <-w.release })
	return w.ResponseRecorder.Write(data)
}
func TestSlowResourceDoesNotLockActivation(t *testing.T) {
	b := NewManagedBundle(t.TempDir())
	initial := b.CurrentAssetSet()
	files, id := testPackage(t, "<!--slow-->")
	if _, _, err := uploadTest(t, b, files); err != nil {
		t.Fatal(err)
	}
	w := &slowResourceWriter{ResponseRecorder: httptest.NewRecorder(), started: make(chan struct{}), release: make(chan struct{})}
	finished := make(chan struct{})
	go func() {
		b.ServeHTTP(w, httptest.NewRequest("GET", PublicPathPrefix+initial+"/runtime.js", nil))
		close(finished)
	}()
	<-w.started
	activated := make(chan error, 1)
	go func() { activated <- b.Activate(id) }()
	select {
	case err := <-activated:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(10 * time.Second):
		t.Error("activation held by slow response")
	}
	close(w.release)
	<-finished
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
}

func TestMissingDeclaredResourceDisablesSelectedTemplate(t *testing.T) {
	root := t.TempDir()
	b := NewManagedBundle(root)
	id := b.CurrentAssetSet()
	w := httptest.NewRecorder()
	b.ServeHTTP(w, httptest.NewRequest("GET", PublicPathPrefix+id+"/not-declared.js", nil))
	if _, err := b.selected(); err != nil {
		t.Fatal("unknown URL poisoned current")
	}
	if err := os.Remove(filepath.Join(root, id, "assets", id, "runtime.css")); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	b.ServeHTTP(w, httptest.NewRequest("GET", PublicPathPrefix+id+"/runtime.css", nil))
	if w.Code != 404 {
		t.Fatal(w.Code)
	}
	if _, err := b.selected(); err == nil {
		t.Fatal("known broken template stayed active")
	}
}
