package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"example.invalid/tunnel-hub-server/internal/shareassets"
)

func TestShareTemplateAPIAuthReadOnlyAndUnknownUsage(t *testing.T) {
	server, db := newAdminTestServer(t)
	bundle := shareassets.NewManagedBundle(t.TempDir())
	server.SetShareTemplates(bundle)
	current := bundle.CurrentAssetSet()
	versions, _ := bundle.Versions()
	if err := db.StartShareTemplateObservation(t.Context(), []string{current}, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/admin/share-templates", "/api/admin/share-templates/" + current + "/shares"} {
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("unauthorized=%d", rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, authedAdminRequest(http.MethodGet, "/api/admin/share-templates?days=7", ""))
	var response struct {
		Items []shareTemplateItem `json:"items"`
		Days  int                 `json:"days"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 200 || response.Days != 7 || len(response.Items) != len(versions) || response.Items[0].Usage == nil || response.Items[0].Usage.RenderCount != 0 || response.Items[1].Usage != nil || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("response=%s", rec.Body.String())
	}
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"POST", "/api/admin/share-templates", 405},
		{"DELETE", "/api/admin/share-templates/" + current, 405},
		{"GET", "/api/admin/share-templates?days=0", 400},
		{"GET", "/api/admin/share-templates?days=7&days=30", 400},
		{"GET", "/api/admin/share-templates?other=true", 400},
		{"GET", "/api/admin/share-templates/unknown/shares", 404},
		{"GET", "/api/admin/share-templates/" + current + "/shares", 200},
	} {
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, authedAdminRequest(tc.method, tc.path, ""))
		if rec.Code != tc.status {
			t.Fatalf("%s %s = %d %s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, authedAdminRequest(http.MethodGet, "/api/admin/share-templates", ""))
	if rec.Code != 500 {
		t.Fatal("storage failure was represented as zero usage")
	}
}

func TestShareTemplatePageShellAndLocalSession(t *testing.T) {
	server, db := newAdminTestServer(t)
	server.SetShareTemplates(shareassets.NewManagedBundle(t.TempDir()))
	for _, path := range []string{"/api/admin/share-templates/view", "/api/admin/share-templates/view/app.js", "/api/admin/share-templates/view/style.css"} {
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") || rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("shell=%d", rec.Code)
		}
	}
	if _, _, err := db.EnsureAdminUser(t.Context(), "admin", "secret"); err != nil {
		t.Fatal(err)
	}
	login := httptest.NewRecorder()
	server.ServeHTTP(login, httptest.NewRequest(http.MethodPost, "/api/admin/login", strings.NewReader(`{"username":"admin","password":"secret"}`)))
	if login.Code != 200 {
		t.Fatal(login.Body.String())
	}
	req := httptest.NewRequest(http.MethodGet, "/api/admin/share-templates", nil)
	req.AddCookie(login.Result().Cookies()[0])
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("existing cookie auth failed: %s", rec.Body.String())
	}
}
