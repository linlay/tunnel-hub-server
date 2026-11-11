package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"example.invalid/tunnel-hub-server/internal/sharefiles"
	"example.invalid/tunnel-hub-server/internal/testutil/mysqltest"
)

// These fixtures are the unmodified main schemas. Start without the extension,
// then exercise the legacy DAO before and after repeated initialization.
func openMainShareDB(t *testing.T) *DB {
	t.Helper()
	var db *DB
	var err error
	fixture := "schema_mysql_legacy.sql"
	if os.Getenv("TEST_DATABASE_TYPE") == "sqlite" {
		db, err = OpenSQLite(t.Context(), filepath.Join(t.TempDir(), "main.sqlite"))
		fixture = "schema_sqlite_legacy.sql"
	} else {
		db, err = Open(t.Context(), mysqltest.NewConfig(t))
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	schema, err := os.ReadFile(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range strings.Split(string(schema), ";") {
		if strings.TrimSpace(query) != "" {
			if _, err := db.sql.Exec(query); err != nil {
				t.Fatal(err)
			}
		}
	}
	if db.database == databaseSQLite {
		if _, err := db.sql.Exec(`PRAGMA user_version = 4`); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func TestShareExtensionPreservesMainWritesClaimsAndResources(t *testing.T) {
	db := openMainShareDB(t)
	ctx := t.Context()
	now := databaseTime(time.Now())
	expires := now.Add(time.Hour)
	snapshot := []byte("{ \"version\":1,\"title\":\"unchanged\" }\n")
	permanent, err := db.CreateConversationShare(ctx, "owner", "chat", 1, snapshot, now, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	timed, err := db.CreateConversationShare(ctx, "owner", "chat", 1, snapshot, now, &expires, false)
	if err != nil {
		t.Fatal(err)
	}
	once, err := db.CreateConversationShare(ctx, "owner", "chat", 1, snapshot, now, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	claim := sha256.Sum256([]byte("original browser session"))
	if _, claimed, err := db.AccessPublicConversationShare(ctx, once.ID, now, nil, claim[:]); err != nil || !claimed {
		t.Fatal("initial claim", claimed, err)
	}
	var originalDDL string
	if db.database == databaseMySQL {
		var table string
		if err := db.sql.QueryRow(`SHOW CREATE TABLE conversation_shares`).Scan(&table, &originalDDL); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err := db.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if db.database == databaseMySQL {
		var table, after string
		if err := db.sql.QueryRow(`SHOW CREATE TABLE conversation_shares`).Scan(&table, &after); err != nil || after != originalDDL {
			t.Fatal("main table definition changed", err)
		}
	}
	for _, id := range []string{permanent.ID, timed.ID} {
		got, _, err := db.AccessPublicConversationShare(ctx, id, now, nil, nil)
		if err != nil || !bytes.Equal(got.SnapshotJSON, snapshot) {
			t.Fatal("legacy share changed during initialization", err)
		}
	}
	if _, claimed, err := db.AccessPublicConversationShare(ctx, once.ID, now.Add(time.Minute), claim[:], nil); err != nil || claimed {
		t.Fatal("original browser lost its claim", claimed, err)
	}
	if _, _, err := db.AccessPublicConversationShare(ctx, once.ID, now.Add(time.Minute), nil, claim[:]); !errors.Is(err, ErrNotFound) {
		t.Fatal("another browser acquired an existing claim", err)
	}
	if _, _, err := db.AccessPublicConversationShare(ctx, timed.ID, expires, nil, nil); !errors.Is(err, ErrNotFound) {
		t.Fatal("expired share reopened", err)
	}

	body := []byte("generic resource")
	hash := sha256.Sum256(body)
	id, err := NewConversationShareID()
	if err != nil {
		t.Fatal(err)
	}
	resource := ConversationShareResource{ID: "0123456789abcdef01234567", Name: "report.txt", MIMEType: "text/plain", Size: int64(len(body)), SHA256: hex.EncodeToString(hash[:]), Path: "report.txt"}
	_, err = db.CreateShareWithResources(ctx, ConversationShare{ID: id, OwnerUserID: "owner", Kind: "report", Title: "Report", SnapshotVersion: 3, SnapshotJSON: []byte(`{"revision":3}`), CreatedAt: now}, []ConversationShareResource{resource})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.AccessPublicConversationShare(ctx, id, now, nil, nil); !errors.Is(err, ErrNotFound) {
		t.Fatal("generic record leaked into main storage", err)
	}
	root := t.TempDir()
	oldFiles, err := sharefiles.New(root)
	if err != nil {
		t.Fatal(err)
	}
	newFiles, err := sharefiles.New(filepath.Join(root, "v2"))
	if err != nil {
		t.Fatal(err)
	}
	stage, err := newFiles.Begin()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := stage.Abort(); err != nil {
			t.Error(err)
		}
	})
	if _, err := stage.Write(resource.ID, bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatal(err)
	}
	if err := stage.Commit(id); err != nil {
		t.Fatal(err)
	}
	owned, err := db.ConversationShareResourceIDs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Force every directory old enough for the original main cleanup.
	if err := oldFiles.CleanupOrphans(owned, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := newFiles.Verify(id, resource.ID, resource.Size, resource.SHA256); err != nil {
		t.Fatal("main cleanup removed a generic resource", err)
	}
	// The unmodified legacy DAO can continue writing after the extension exists.
	later, err := db.CreateConversationShare(ctx, "owner", "later", 1, snapshot, now.Add(time.Minute), nil, false)
	if err != nil {
		t.Fatal("main write after extension", err)
	}
	if err := db.RevokeConversationShare(ctx, permanent.ID, "owner", now); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.AccessPublicConversationShare(ctx, later.ID, now, nil, nil); err != nil {
		t.Fatal("main's new record disappeared", err)
	}
	if _, err := db.ReadPublicShareResourceByPath(ctx, id, resource.Path); err != nil {
		t.Fatal("generic resource lost after returning to new version", err)
	}
	if _, err := db.AccessPublicShare(ctx, id); err != nil {
		t.Fatal(err)
	}
}

func TestShareExtensionRejectsPreviouslyDestructiveSchema(t *testing.T) {
	db := openMainShareDB(t)
	if _, err := db.sql.Exec(`ALTER TABLE conversation_shares RENAME COLUMN expires_at TO retired_expires_at`); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(context.Background()); err == nil {
		t.Fatal("silently accepted a schema that main cannot read")
	}
}
