package shareassets

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	first, _, err := bundle.RenderVersion(snapshot, "https://share.example.test", "example", "Example", "", true)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := bundle.RenderVersion(snapshot, "https://share.example.test", "example", "Example", "", true)
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
	// These pre-upload resource-only sets are fixed legacy byte baselines.
	// Later full-package IDs cannot be recomputed without their historical HTML.
	legacySets := map[string]bool{
		"055d7f779a2c178ea6dcb03b0123745f126b673aff7f8f9a216e8956cb660235": true,
		"0a3285bfd83dc57f4cc00d2effa202988ea6d9735835cf6951009b74600f02e2": true,
		"194486663943b1ce381cac3d06018da1e4589b77e9ed6fff6b66ce1c30ba2228": true,
		"34cc34ac97f501fd89a9d05ee5764dc080741b8a968a6a28c68ad0a6551f8048": true,
		"3d641cbc1310d58d0335aa17f746a99061e5d30e7bc1b5fa16de095a6b3fc80f": true,
		"517e5e91336a7810a30a985f37666deded49d85bbabc5dd6d83559d3aa00bbee": true,
		"536661c61fb309fe8e31ce8716e5a7ffe74d4ebaa6aaf4b0de1d71fed161a1c8": true,
		"6bf7d89bf7f11ad0513428d0b2b3bdc0db9b59733d05a38b699b2b47fcf45c44": true,
		"748f7dee897a48399833b093368054d29b4a607f0300d0e68bbdf53b463c2afe": true,
		"958e38fff76f9ee34a58fe41909203b755b6ed3668f698594d80cbbaa2449f2f": true,
		"aa527449c30f06c4de12325adc9d48e3459125e5efa2c42ea578ac186ffeebee": true,
		"c7aa11284fd73021b4238ecd00977e0e161dad4c3376dfd2c2603315b568dcac": true,
		"db95305cb9a40cd5fd4afbe5a85805f94845f12b267c25734a06f586453fffc9": true,
		"ee3d5abec9a1fba71612cc4e96d051adebd09186d6eda17f1333ecfa445bae43": true,
		"f9bdc1237af18ddd2a3449a9694cc4e274fdf4d2e0f12762e81735aa27a3fc9b": true,
	}
	currentSetFound := false
	for _, set := range sets {
		if !set.IsDir() || len(set.Name()) != 64 {
			t.Fatalf("invalid asset-set entry %q", set.Name())
		}
		if set.Name() == manifest.AssetSet {
			currentSetFound = true
			managed := NewManagedBundle(t.TempDir())
			if _, err := managed.selected(); err != nil {
				t.Fatal(err)
			}
		}
		if !legacySets[set.Name()] {
			continue
		}
		delete(legacySets, set.Name())
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
	if len(legacySets) != 0 {
		t.Fatal("legacy resources were removed", legacySets)
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

func TestBundleHTMLPreviewUsesOnlyTheVersionedIntegrityCheckedBootstrap(t *testing.T) {
	bundle := NewBundle()
	body, err := bundle.RenderHTMLPreview("https://share.example.test", bundle.assetSet)
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
	if _, err := bundle.RenderHTMLPreview("https://share.example.test/path", bundle.assetSet); err == nil {
		t.Fatal("non-origin URL accepted")
	}
	req := httptest.NewRequest(http.MethodGet, PublicPathPrefix+bundle.assetSet+"/html-preview-bootstrap.js", nil)
	rec := httptest.NewRecorder()
	bundle.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("Access-Control-Allow-Origin") != "*" || rec.Header().Get("Set-Cookie") != "" {
		t.Fatalf("anonymous bootstrap headers: %d %v", rec.Code, rec.Header())
	}
}

func TestUnversionedPreviewCompatibilityMatchesEmbeddedHistory(t *testing.T) {
	bundle := NewBundle()
	want, err := fs.ReadFile(bundle.files, bundle.assetSet+"/html-preview-bootstrap.js")
	if err != nil {
		t.Fatal(err)
	}
	versions, err := bundle.Versions()
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range versions {
		body, err := fs.ReadFile(bundle.files, version.AssetSet+"/html-preview-bootstrap.js")
		if errors.Is(err, fs.ErrNotExist) {
			continue // Predates HTML preview support.
		}
		if err != nil || !bytes.Equal(body, want) {
			t.Fatalf("unversioned preview compatibility changed for %s: %v", version.AssetSet, err)
		}
	}
}

func TestBundleRendersHTMLSafeSnapshotWithCurrentAssets(t *testing.T) {
	bundle := NewBundle()
	snapshot := []byte(`{"version":1,"title":"</script>&  "}`)
	html, _, err := bundle.RenderVersion(snapshot, "https://share.example.test", "zenmind", "ZenMind", "", true)
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
		page, _, err := bundle.RenderVersion([]byte(`{"version":1}`), "https://share.example.test", "example", "Example", "", allowed)
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
		page, _, err := bundle.RenderVersion(snapshot, "https://share.example.test", brand.id, brand.name, "", true)
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
		if _, _, err := bundle.RenderVersion(snapshot, "https://share.example.test", id, "Valid", "", true); err == nil {
			t.Fatalf("accepted unsafe brand scheme %q", id)
		}
	}
	if _, _, err := bundle.RenderVersion(snapshot, "https://share.example.test", "valid", " ", "", true); err == nil {
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
	page, _, err := bundle.RenderVersion(
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
		if _, _, err := bundle.RenderVersion(snapshot, "https://share.example.test", "zenmind", "ZenMind", value, true); err == nil {
			t.Fatalf("accepted unsafe product download page %q", value)
		}
	}

	page, _, err = bundle.RenderVersion(snapshot, "https://share.example.test", "zenmind", "ZenMind", "", true)
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
