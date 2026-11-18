package shareassets

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestVersionInventorySeparatesCurrentAndHistory(t *testing.T) {
	current, historical := strings.Repeat("b", 64), strings.Repeat("a", 64)
	bundle := &Bundle{assetSet: current, files: fstest.MapFS{
		current + "/runtime.js":      &fstest.MapFile{Data: []byte("one")},
		current + "/fonts/test.woff": &fstest.MapFile{Data: []byte("two")},
		historical + "/runtime.js":   &fstest.MapFile{Data: []byte("old!")},
	}}
	versions, err := bundle.Versions()
	if err != nil || len(versions) != 2 || !versions[0].Current || versions[0].SizeBytes != 6 || versions[0].FileCount != 2 || versions[1].Current || versions[1].SizeBytes != 4 {
		t.Fatalf("versions=%+v err=%v", versions, err)
	}
}

func TestAssetUsageCountsOnlySuccessfulGET(t *testing.T) {
	bundle := NewBundle()
	assetPath := PublicPathPrefix + findEmbeddedAsset(t, "/runtime.js")
	assetSet := strings.Split(strings.TrimPrefix(assetPath, PublicPathPrefix), "/")[0]
	var observed []string
	bundle.SetResourceObserver(func(_ context.Context, set string) { observed = append(observed, set) })
	for _, tc := range []struct {
		method, path, byteRange string
		count                   int
	}{
		{http.MethodHead, assetPath, "", 0},
		{http.MethodGet, PublicPathPrefix + assetSet + "/missing.js", "", 0},
		{http.MethodGet, assetPath, "bytes=999999999999-", 0},
		{http.MethodGet, assetPath, "bytes=0-3", 1},
		{http.MethodGet, assetPath, "", 2},
		{http.MethodPost, assetPath, "", 2},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		if tc.byteRange != "" {
			req.Header.Set("Range", tc.byteRange)
		}
		rec := httptest.NewRecorder()
		bundle.ServeHTTP(rec, req)
		if len(observed) != tc.count {
			t.Fatalf("%s %s status=%d observed=%v", tc.method, tc.byteRange, rec.Code, observed)
		}
	}
	for _, set := range observed {
		if set != assetSet {
			t.Fatal("recorded current version instead of requested version")
		}
	}
	bundle.ServeHTTP(&failedAssetWriter{HeaderMap: make(http.Header)}, httptest.NewRequest(http.MethodGet, assetPath, nil))
	if len(observed) != 2 {
		t.Fatal("failed response counted")
	}
}

type failedAssetWriter struct{ HeaderMap http.Header }

func (w *failedAssetWriter) Header() http.Header       { return w.HeaderMap }
func (w *failedAssetWriter) WriteHeader(int)           {}
func (w *failedAssetWriter) Write([]byte) (int, error) { return 0, errors.New("disconnected") }
