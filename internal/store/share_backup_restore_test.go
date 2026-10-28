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
)

// Rehearse with an offline synthetic SQLite database and its matching resources.
// A restored old backup is migrated before serving; retired links never reopen.
func TestSQLiteShareBackupRestorePreservesPermanentAndReappliesRevocation(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	livePath := filepath.Join(root, "live.sqlite")
	resources := filepath.Join(root, "live-resources")
	db, err := OpenSQLite(ctx, livePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, statement := range strings.Split(legacySQLiteSchema(t), ";") {
		if strings.TrimSpace(statement) != "" {
			if _, err := db.sql.ExecContext(ctx, statement); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := db.sql.ExecContext(ctx, `PRAGMA user_version=4`); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	snapshot := []byte("{ \"version\":1,\"title\":\"frozen permanent\" }\n")
	for _, record := range []struct {
		id               string
		expires, revoked *time.Time
		once             int
	}{{"permanent", nil, nil, 0}, {"once", nil, nil, 1}, {"future", timePointer(now.Add(100 * 365 * 24 * time.Hour)), nil, 0}, {"revoked", nil, &now, 0}} {
		if _, err := db.sql.ExecContext(ctx, `INSERT INTO conversation_shares(id,owner_user_id,conversation_id,snapshot_version,snapshot_json,created_at,expires_at,revoked_at,single_use) VALUES(?,?,?,1,?,?,?,?,?)`, record.id, "owner", "chat", snapshot, now, record.expires, record.revoked, record.once); err != nil {
			t.Fatal(err)
		}
	}
	const resourceID = "0123456789abcdef01234567"
	body := []byte("frozen permanent resource")
	digest := sha256.Sum256(body)
	if _, err := db.sql.ExecContext(ctx, `INSERT INTO conversation_share_resources(share_id,resource_id,name,mime_type,size_bytes,sha256) VALUES('permanent',?,'overview.txt','text/plain',?,?)`, resourceID, len(body), hex.EncodeToString(digest[:])); err != nil {
		t.Fatal(err)
	}
	files, err := sharefiles.New(resources)
	if err != nil {
		t.Fatal(err)
	}
	stage, err := files.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer stage.Abort()
	if _, err := stage.Write(resourceID, bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatal(err)
	}
	if err := stage.Commit("permanent"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	backupDB, err := os.ReadFile(livePath)
	if err != nil {
		t.Fatal(err)
	}
	backupHash := sha256.Sum256(backupDB)
	backupPath := filepath.Join(root, "backup.sqlite")
	if err := os.WriteFile(backupPath, backupDB, 0600); err != nil {
		t.Fatal(err)
	}
	backupResources := filepath.Join(root, "backup-resources")
	if err := os.CopyFS(backupResources, os.DirFS(resources)); err != nil {
		t.Fatal(err)
	}
	verify := func(resourceRoot string) {
		t.Helper()
		if err := db.Migrate(ctx, resourceRoot); err != nil {
			t.Fatal(err)
		}
		share, err := db.AccessPublicConversationShare(ctx, "permanent")
		if err != nil || !bytes.Equal(share.SnapshotJSON, snapshot) {
			t.Fatal("permanent identity/bytes changed", err)
		}
		for _, id := range []string{"once", "future", "revoked"} {
			if _, err := db.AccessPublicConversationShare(ctx, id); !errors.Is(err, ErrNotFound) {
				t.Fatalf("retired link reopened: %s (%v)", id, err)
			}
		}
		got, err := os.ReadFile(filepath.Join(resourceRoot, "shares", "permanent", "resources", resourceID))
		if err != nil || sha256.Sum256(got) != digest {
			t.Fatal("resource backup mismatched", err)
		}
	}
	db, err = OpenSQLite(ctx, livePath)
	if err != nil {
		t.Fatal(err)
	}
	verify(resources)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	restoredPath := filepath.Join(root, "restored.sqlite")
	if err := os.WriteFile(restoredPath, backupDB, 0600); err != nil {
		t.Fatal(err)
	}
	restoredResources := filepath.Join(root, "restored-resources")
	if err := os.CopyFS(restoredResources, os.DirFS(backupResources)); err != nil {
		t.Fatal(err)
	}
	db, err = OpenSQLite(ctx, restoredPath)
	if err != nil {
		t.Fatal(err)
	}
	verify(restoredResources)
	original, err := os.ReadFile(backupPath)
	if err != nil || sha256.Sum256(original) != backupHash {
		t.Fatal("rehearsal modified the backup", err)
	}
}
