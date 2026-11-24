package main

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"example.invalid/tunnel-hub-server/internal/admin"
	"example.invalid/tunnel-hub-server/internal/config"
	desktopapi "example.invalid/tunnel-hub-server/internal/desktop"
	"example.invalid/tunnel-hub-server/internal/shareassets"
	"example.invalid/tunnel-hub-server/internal/testutil/dbtest"
)

func TestShareTemplateAssemblyKeepsRecoveryAvailable(t *testing.T) {
	t.Setenv("TEST_DATABASE_TYPE", "sqlite")
	db := dbtest.Open(t)
	root := t.TempDir()
	templates := filepath.Join(root, "templates")
	initial := shareassets.NewManagedBundle(templates).CurrentAssetSet()
	if err := os.WriteFile(filepath.Join(templates, "current"), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	adm := &admin.Server{DB: db, Logger: logger}
	desktop := &desktopapi.Server{DB: db, Logger: logger}
	bundle := configureShareTemplates(config.RelayConfig{ConversationShareResourceDir: root}, db, logger, adm, desktop)
	w := httptest.NewRecorder()
	bundle.ServeHTTP(w, httptest.NewRequest("GET", shareassets.TemplatePublicPath, nil))
	if w.Code != 503 {
		t.Fatal(w.Code)
	}
	w = httptest.NewRecorder()
	adm.ServeHTTP(w, httptest.NewRequest("GET", "/api/admin/share-templates/view", nil))
	if w.Code != 200 {
		t.Fatal("recovery page", w.Code)
	}
	if err := bundle.Activate(initial); err != nil {
		t.Fatal(err)
	}
	html, id, err := bundle.RenderVersion([]byte(`{"version":1}`), "https://share.example.test", "example", "Example", "", true)
	if err != nil || len(html) == 0 || id != initial {
		t.Fatal(id, err)
	}
}
