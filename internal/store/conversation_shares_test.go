package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestConversationShareCreateReadAndRevoke(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	eventStream := []byte("event: message\ndata: {\"seq\":1,\"type\":\"chat.start\",\"shareVersion\":1,\"chatName\":\"Release plan\",\"timestamp\":1700000000000}\n\nevent: message\ndata: [DONE]\n\n")
	share, err := db.CreateConversationShare(ctx, "owner-a", "Release plan", eventStream)
	if err != nil {
		t.Fatalf("create share: %v", err)
	}
	if !strings.HasPrefix(share.ID, "share_") || len(share.ID) < 30 {
		t.Fatalf("share id is not opaque: %q", share.ID)
	}
	found, err := db.GetPublicConversationShare(ctx, share.ID)
	if err != nil {
		t.Fatalf("get share: %v", err)
	}
	if string(found.EventStream) != string(eventStream) || found.StreamVersion != 1 || found.OwnerUserID != "owner-a" {
		t.Fatalf("unexpected share: %#v", found)
	}
	if err := db.RevokeConversationShare(ctx, share.ID, "owner-b"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other owner revoke error=%v", err)
	}
	if err := db.RevokeConversationShare(ctx, share.ID, "owner-a"); err != nil {
		t.Fatalf("revoke share: %v", err)
	}
	if _, err := db.GetPublicConversationShare(ctx, share.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked share should be hidden, got %v", err)
	}
}

func TestConversationShareHidesLegacySnapshotRows(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	_, err := db.sql.ExecContext(ctx, `
		INSERT INTO conversation_shares (id, owner_user_id, title, stream_version, event_stream, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, "share_legacy", "owner-a", "Legacy", 0, []byte(`{"schemaVersion":1}`), time.Now().UTC())
	if err != nil {
		t.Fatalf("insert legacy share: %v", err)
	}
	if _, err := db.GetPublicConversationShare(ctx, "share_legacy"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("legacy share should be hidden, got %v", err)
	}
}

func TestConversationShareMigrationMarksExistingRowsAsLegacy(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if _, err := db.sql.ExecContext(ctx, `
		CREATE TABLE conversation_shares (
			id TEXT PRIMARY KEY,
			owner_user_id TEXT NOT NULL,
			title TEXT NOT NULL,
			snapshot_json BLOB NOT NULL,
			created_at TIMESTAMP NOT NULL,
			revoked_at TIMESTAMP
		);
		INSERT INTO conversation_shares (id, owner_user_id, title, snapshot_json, created_at)
		VALUES ('share_legacy', 'owner-a', 'Legacy', '{"schemaVersion":1}', CURRENT_TIMESTAMP);
	`); err != nil {
		t.Fatalf("seed legacy schema: %v", err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate legacy schema: %v", err)
	}
	var version int
	if err := db.sql.QueryRowContext(ctx, `SELECT stream_version FROM conversation_shares WHERE id = 'share_legacy'`).Scan(&version); err != nil {
		t.Fatalf("read migrated version: %v", err)
	}
	if version != 0 {
		t.Fatalf("legacy stream version=%d want=0", version)
	}
	columns, err := db.tableColumns(ctx, "conversation_shares")
	if err != nil {
		t.Fatalf("read migrated columns: %v", err)
	}
	if !columns["event_stream"] || columns["snapshot_json"] {
		t.Fatalf("migration did not rename snapshot_json in place: %#v", columns)
	}
	if _, err := db.GetPublicConversationShare(ctx, "share_legacy"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("migrated legacy share should be hidden, got %v", err)
	}
}

func TestConversationShareLookupDoesNotRequireGeneratedPrefix(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	eventStream := []byte("event: message\ndata: [DONE]\n\n")
	_, err := db.sql.ExecContext(ctx, `
		INSERT INTO conversation_shares (id, owner_user_id, title, stream_version, event_stream, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, "opaque-abc_123", "owner-a", "Opaque", 1, eventStream, time.Now().UTC())
	if err != nil {
		t.Fatalf("insert prefixless share: %v", err)
	}
	found, err := db.GetPublicConversationShare(ctx, "opaque-abc_123")
	if err != nil || string(found.EventStream) != string(eventStream) {
		t.Fatalf("prefixless lookup=%#v err=%v", found, err)
	}
}
