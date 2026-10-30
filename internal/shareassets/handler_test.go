package shareassets

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func responseStyleNonce(t *testing.T, body string) string {
	t.Helper()
	match := regexp.MustCompile(`name="conversation-export-style-nonce" content="([A-Za-z0-9_-]{43})"`).FindStringSubmatch(body)
	if len(match) != 2 || !strings.Contains(body, "'nonce-"+match[1]+"'") {
		t.Fatal("style nonce is missing or does not match the CSP")
	}
	if regexp.MustCompile(`style-src-elem[^;]*'unsafe-inline'`).MatchString(body) {
		t.Fatal("arbitrary inline style elements must remain blocked")
	}
	return match[1]
}

func TestStyleNonceIsUniquePerRenderAndDoesNotRewriteSnapshot(t *testing.T) {
	bundle := NewBundle()
	snapshot := []byte(`{"version":1,"title":"` + styleNonceMarker + `"}`)
	first, err := bundle.Render(snapshot, "https://share.example.test", "example", "Example", "", true)
	if err != nil {
		t.Fatal(err)
	}
	second, err := bundle.Render(snapshot, "https://share.example.test", "example", "Example", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if responseStyleNonce(t, string(first)) == responseStyleNonce(t, string(second)) {
		t.Fatal("style nonce was reused across responses")
	}
	if !strings.Contains(string(first), `"title":"`+styleNonceMarker+`"`) {
		t.Fatal("nonce replacement must not modify user snapshot content")
	}
	if strings.Count(string(bundle.template), styleNonceMarker) != 2 {
		t.Fatal("rendering mutated the shared template")
	}
}

func TestDownloadedTemplateHasFreshNonceForExistingLocalExportClients(t *testing.T) {
	handler := NewHandler()
	nonces := make(map[string]bool)
	for range 2 {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, TemplatePublicPath, nil))
		body := response.Body.String()
		if response.Code != http.StatusOK || strings.Contains(body, styleNonceMarker) ||
			!strings.Contains(body, snapshotMarker) || !strings.Contains(body, assetOriginMarker) ||
			!strings.Contains(body, localBrandIDMarker) {
			t.Fatal("downloaded template must resolve the nonce and preserve existing assembly markers")
		}
		nonce := responseStyleNonce(t, body)
		if nonces[nonce] {
			t.Fatal("downloaded templates reused a nonce")
		}
		nonces[nonce] = true
	}
}

func TestStyleNonceFailsClosedForMissingOrExtraPlaceholders(t *testing.T) {
	for _, count := range []int{0, 1, 3} {
		if _, err := templateWithStyleNonce([]byte(strings.Repeat(styleNonceMarker, count))); err == nil {
			t.Fatalf("accepted %d nonce placeholders", count)
		}
	}
	response := httptest.NewRecorder()
	serveTemplate(response, httptest.NewRequest(http.MethodGet, TemplatePublicPath, nil), nil)
	if response.Code != http.StatusInternalServerError {
		t.Fatal("invalid template was served without a nonce")
	}
}

func TestEmbeddedAssetSetDirectoryMatchesContentHash(t *testing.T) {
	sets, err := fs.ReadDir(embeddedFiles, "files")
	if err != nil {
		t.Fatal(err)
	}
	var manifest assetManifest
	manifestBytes, err := embeddedFiles.ReadFile("conversation-assets.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	currentSetFound := false
	for _, set := range sets {
		if !set.IsDir() || len(set.Name()) != 64 {
			t.Fatalf("invalid asset-set entry %q", set.Name())
		}
		if set.Name() == manifest.AssetSet {
			currentSetFound = true
		}
		root := path.Join("files", set.Name())
		var filenames []string
		if err := fs.WalkDir(embeddedFiles, root, func(filename string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if !entry.IsDir() {
				filenames = append(filenames, strings.TrimPrefix(filename, root+"/"))
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		sort.Strings(filenames)
		digest := sha256.New()
		_, _ = fmt.Fprint(digest, "conversation-export-assets\x00")
		for _, filename := range filenames {
			content, err := fs.ReadFile(embeddedFiles, path.Join(root, filename))
			if err != nil {
				t.Fatal(err)
			}
			fileDigest := sha256.Sum256(content)
			_, _ = fmt.Fprintf(digest, "%s\x00%s\n", filename, hex.EncodeToString(fileDigest[:]))
		}
		if got := hex.EncodeToString(digest.Sum(nil)); got != set.Name() {
			t.Fatalf("asset set directory=%s content hash=%s", set.Name(), got)
		}
	}
	if !currentSetFound {
		t.Fatalf("current asset set %s is missing", manifest.AssetSet)
	}
}

func TestHandlerServesImmutableCrossOriginAssets(t *testing.T) {
	runtimePath := findEmbeddedAsset(t, "/runtime.js")
	handler := NewHandler()

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, PublicPathPrefix+runtimePath, nil))
	if response.Code != http.StatusOK || response.Body.Len() == 0 {
		t.Fatalf("GET status=%d bytes=%d", response.Code, response.Body.Len())
	}
	for name, want := range map[string]string{
		"Content-Type":                 "application/javascript; charset=utf-8",
		"Cache-Control":                "public, max-age=31536000, immutable",
		"Access-Control-Allow-Origin":  "*",
		"Cross-Origin-Resource-Policy": "cross-origin",
		"X-Content-Type-Options":       "nosniff",
	} {
		if got := response.Header().Get(name); got != want {
			t.Fatalf("%s=%q want=%q", name, got, want)
		}
	}

	head := httptest.NewRecorder()
	handler.ServeHTTP(head, httptest.NewRequest(http.MethodHead, PublicPathPrefix+runtimePath, nil))
	if head.Code != http.StatusOK || head.Body.Len() != 0 || head.Header().Get("Content-Length") == "" {
		t.Fatalf("HEAD status=%d bytes=%d headers=%v", head.Code, head.Body.Len(), head.Header())
	}
}

func TestHandlerStillServesHistoricalAssetSets(t *testing.T) {
	currentSet := NewBundle().assetSet
	sets, err := fs.ReadDir(embeddedFiles, "files")
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler()
	historicalSets := 0
	for _, set := range sets {
		if set.Name() == currentSet {
			continue
		}
		historicalSets++
		assetPath := PublicPathPrefix + set.Name() + "/runtime.js"
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, assetPath, nil))
		if response.Code != http.StatusOK || response.Body.Len() == 0 {
			t.Fatalf("historical asset %s: status=%d bytes=%d", assetPath, response.Code, response.Body.Len())
		}
	}
	if historicalSets == 0 {
		t.Fatal("expected at least one historical asset set")
	}
}

func TestHandlerServesSVGAsAnImage(t *testing.T) {
	brandIconPath := findEmbeddedAsset(t, ".svg")
	response := httptest.NewRecorder()
	NewHandler().ServeHTTP(
		response,
		httptest.NewRequest(http.MethodGet, PublicPathPrefix+brandIconPath, nil),
	)
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "image/svg+xml" {
		t.Fatalf("status=%d content-type=%q", response.Code, response.Header().Get("Content-Type"))
	}
}

func TestHandlerServesCurrentTemplateWithoutCaching(t *testing.T) {
	handler := NewHandler()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, TemplatePublicPath, nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), snapshotMarker) ||
		!strings.Contains(response.Body.String(), localBrandIDMarker) {
		t.Fatalf("GET status=%d body=%q", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control=%q", got)
	}
	if got := response.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("Access-Control-Allow-Origin=%q", got)
	}
}

func TestHandlerServesEveryCurrentManifestAsset(t *testing.T) {
	var manifest assetManifest
	content, err := embeddedFiles.ReadFile("conversation-assets.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(content, &manifest); err != nil {
		t.Fatal(err)
	}
	bundle := NewBundle()
	for _, file := range manifest.Files {
		response := httptest.NewRecorder()
		requestPath := PublicPathPrefix + manifest.AssetSet + "/" + file.Path
		bundle.ServeHTTP(response, httptest.NewRequest(http.MethodGet, requestPath, nil))
		if response.Code != http.StatusOK || response.Body.Len() == 0 {
			t.Fatalf("asset=%s status=%d bytes=%d", file.Path, response.Code, response.Body.Len())
		}
	}
}

func TestBundleHTMLPreviewUsesOnlyTheCurrentIntegrityCheckedBootstrap(t *testing.T) {
	bundle := NewBundle()
	body, err := bundle.RenderHTMLPreview("https://share.example.test")
	if err != nil {
		t.Fatal(err)
	}
	markup := string(body)
	for _, expected := range []string{
		`name="conversation-share-origin" content="https://share.example.test"`,
		PublicPathPrefix + bundle.assetSet + "/html-preview-bootstrap.js",
		`crossorigin="anonymous"`, `integrity="` + bundle.htmlPreviewIntegrity + `"`,
	} {
		if !strings.Contains(markup, expected) {
			t.Fatalf("bootstrap is missing %s", expected)
		}
	}
	if strings.Contains(markup, "runtime.js") || strings.Count(markup, "<script") != 1 {
		t.Fatal("guest shell contains a second runtime")
	}
	if _, err := bundle.RenderHTMLPreview("https://share.example.test/path"); err == nil {
		t.Fatal("non-origin URL accepted")
	}
	req := httptest.NewRequest(http.MethodGet, PublicPathPrefix+bundle.assetSet+"/html-preview-bootstrap.js", nil)
	rec := httptest.NewRecorder()
	bundle.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("Access-Control-Allow-Origin") != "*" || rec.Header().Get("Set-Cookie") != "" {
		t.Fatalf("anonymous bootstrap headers: %d %v", rec.Code, rec.Header())
	}
}

func TestBundleRendersHTMLSafeSnapshotWithCurrentAssets(t *testing.T) {
	bundle := NewBundle()
	snapshot := []byte(`{"version":1,"title":"</script>&  "}`)
	html, err := bundle.Render(snapshot, "https://share.example.test", "zenmind", "ZenMind", "", true)
	if err != nil {
		t.Fatal(err)
	}
	body := string(html)
	if strings.Contains(body, `</script>&`) ||
		!strings.Contains(body, `\u003c/script\u003e\u0026\u2028\u2029`) {
		t.Fatalf("snapshot was not HTML escaped: %q", body)
	}
	if strings.Contains(body, assetOriginMarker) || strings.Contains(body, snapshotMarker) ||
		strings.Contains(body, localBrandIDMarker) {
		t.Fatal("rendered page still contains template markers")
	}
	if !strings.Contains(body, "https://share.example.test/assets/conversation-export/"+bundle.assetSet+"/runtime.js") {
		t.Fatal("rendered page does not reference the current asset set")
	}
}

func TestBundleRendersReshareCapabilityWithoutChangingLocalTemplate(t *testing.T) {
	bundle := NewBundle()
	for _, allowed := range []bool{true, false} {
		page, err := bundle.Render([]byte(`{"version":1}`), "https://share.example.test", "example", "Example", "", allowed)
		if err != nil {
			t.Fatal(err)
		}
		marker := fmt.Sprintf(`<meta name="conversation-export-can-reshare" content="%t" />`, allowed)
		if strings.Count(string(page), marker) != 1 {
			t.Fatalf("reshare capability missing: %t", allowed)
		}
	}
	if bytes := strings.Count(string(bundle.template), canReshareMeta); bytes != 1 {
		t.Fatal("local template must keep resharing disabled")
	}
}

func TestBundleInjectsOnlyValidatedPublicBrand(t *testing.T) {
	bundle := NewBundle()
	snapshot := []byte(`{"version":1,"title":"shared"}`)
	for _, brand := range []struct{ id, name string }{
		{"zenmind", `ZenMind "<&>`},
		{"cutej", "CuteJ"},
	} {
		page, err := bundle.Render(snapshot, "https://share.example.test", brand.id, brand.name, "", true)
		if err != nil {
			t.Fatal(err)
		}
		body := string(page)
		if strings.Contains(body, publicBrandMeta) || !strings.Contains(body, `&#34;openScheme&#34;`) {
			t.Fatalf("brand metadata absent for %s", brand.id)
		}
		if strings.Contains(body, `"<&>`) || !strings.Contains(body, brand.id) {
			t.Fatalf("brand metadata not escaped for %s", brand.id)
		}
	}
	for _, id := range []string{"", "bad:scheme", "UPPER", "a/b", "javascript", "https"} {
		if _, err := bundle.Render(snapshot, "https://share.example.test", id, "Valid", "", true); err == nil {
			t.Fatalf("accepted unsafe brand scheme %q", id)
		}
	}
	if _, err := bundle.Render(snapshot, "https://share.example.test", "valid", " ", "", true); err == nil {
		t.Fatal("accepted empty product name")
	}
	if !strings.Contains(string(bundle.template), publicBrandMeta) {
		t.Fatal("downloadable template must keep brand metadata empty")
	}
	if !strings.Contains(string(bundle.template), localBrandIDMarker) {
		t.Fatal("downloadable template must keep the local brand marker")
	}
}

func TestBundleInjectsOnlyValidatedProductDownloadPage(t *testing.T) {
	bundle := NewBundle()
	snapshot := []byte(`{"version":1,"title":"shared"}`)
	page, err := bundle.Render(
		snapshot,
		"https://share.example.test",
		"zenmind",
		"ZenMind",
		"https://download.example.test/product?source=share&campaign=desktop#install",
		true,
	)
	if err != nil {
		t.Fatal(err)
	}
	body := string(page)
	if !strings.Contains(body, `&#34;downloadPageUrl&#34;`) ||
		!strings.Contains(body, `https://download.example.test/product?source=share\u0026campaign=desktop#install`) {
		t.Fatal("download page metadata absent or not escaped")
	}

	for _, value := range []string{
		"http://download.example.test/product",
		"javascript:alert(1)",
		"/relative/download",
		"https://user:secret@download.example.test/product",
	} {
		if _, err := bundle.Render(snapshot, "https://share.example.test", "zenmind", "ZenMind", value, true); err == nil {
			t.Fatalf("accepted unsafe product download page %q", value)
		}
	}

	page, err = bundle.Render(snapshot, "https://share.example.test", "zenmind", "ZenMind", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(page), "downloadPageUrl") {
		t.Fatal("empty product download page must be omitted")
	}
}

func TestHandlerRejectsInvalidPathsAndMethods(t *testing.T) {
	handler := NewHandler()
	for _, requestPath := range []string{
		PublicPathPrefix,
		PublicPathPrefix + "not-a-hash/runtime.js",
		PublicPathPrefix + "v1/" + strings.Repeat("a", 64) + "/runtime.js",
		PublicPathPrefix + strings.Repeat("a", 64) + "/../runtime.js",
		PublicPathPrefix + strings.Repeat("a", 64) + "/missing.js",
		PublicPathPrefix + strings.Repeat("b", 64) + "/runtime.js",
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, requestPath, nil))
		if response.Code != http.StatusNotFound {
			t.Fatalf("path=%q status=%d", requestPath, response.Code)
		}
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, PublicPathPrefix+findEmbeddedAsset(t, "/runtime.js"), nil))
	if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("POST status=%d allow=%q", response.Code, response.Header().Get("Allow"))
	}
}

func findEmbeddedAsset(t *testing.T, suffix string) string {
	t.Helper()
	var found string
	err := fs.WalkDir(embeddedFiles, "files", func(filename string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && strings.HasSuffix(filename, suffix) {
			found = strings.TrimPrefix(filename, "files/")
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found == "" {
		t.Fatalf("embedded asset with suffix %q not found", suffix)
	}
	return found
}
