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
	"sort"
	"strings"
	"testing"
)

func TestEmbeddedAssetSetDirectoryMatchesContentHash(t *testing.T) {
	sets, err := fs.ReadDir(embeddedFiles, "files")
	if err != nil {
		t.Fatal(err)
	}
	if len(sets) != 1 {
		t.Fatalf("embedded conversation export asset sets=%d want=1", len(sets))
	}
	for _, set := range sets {
		if !set.IsDir() || len(set.Name()) != 64 {
			t.Fatalf("invalid asset-set entry %q", set.Name())
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

func TestHandlerServesCurrentTemplateWithoutCaching(t *testing.T) {
	handler := NewHandler()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, TemplatePublicPath, nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), snapshotMarker) {
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

func TestBundleRendersHTMLSafeSnapshotWithCurrentAssets(t *testing.T) {
	bundle := NewBundle()
	snapshot := []byte(`{"version":1,"title":"</script>&  "}`)
	html, err := bundle.Render(snapshot, "https://share.example.test")
	if err != nil {
		t.Fatal(err)
	}
	body := string(html)
	if strings.Contains(body, `</script>&`) ||
		!strings.Contains(body, `\u003c/script\u003e\u0026\u2028\u2029`) {
		t.Fatalf("snapshot was not HTML escaped: %q", body)
	}
	if strings.Contains(body, assetOriginMarker) || strings.Contains(body, snapshotMarker) {
		t.Fatal("rendered page still contains template markers")
	}
	if !strings.Contains(body, "https://share.example.test/assets/conversation-export/"+bundle.assetSet+"/runtime.js") {
		t.Fatal("rendered page does not reference the current asset set")
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
