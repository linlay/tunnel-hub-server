package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func genericTestShare(t *testing.T) ConversationShare {
	t.Helper()
	id, err := NewConversationShareID()
	if err != nil {
		t.Fatal(err)
	}
	return ConversationShare{ID: id, OwnerUserID: "owner", Kind: "report", Title: "Report", SnapshotVersion: 3, SnapshotJSON: []byte(`{"revision":3}`), CreatedAt: time.Now()}
}

func TestGenericShareResourceFailureRollsBackContent(t *testing.T) {
	db := openTestDB(t)
	query := `ALTER TABLE share_resources ADD CONSTRAINT test_reject_generic_resource CHECK (resource_id <> 'ffffffffffffffffffffffff')`
	if db.database == databaseSQLite {
		query = `CREATE TRIGGER reject_generic_resource BEFORE INSERT ON share_resources
		WHEN NEW.resource_id = 'ffffffffffffffffffffffff'
		BEGIN SELECT RAISE(ABORT, 'reject generic resource'); END`
	}
	if _, err := db.sql.Exec(query); err != nil {
		t.Fatal(err)
	}
	share := genericTestShare(t)
	resource := ConversationShareResource{ID: "ffffffffffffffffffffffff", Name: "report.txt", MIMEType: "text/plain", Size: 1, SHA256: strings.Repeat("a", 64)}
	if _, err := db.CreateShareWithResources(t.Context(), share, []ConversationShareResource{resource}); err == nil {
		t.Fatal("resource failure committed the share")
	}
	var count int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM shares WHERE id=?`, share.ID).Scan(&count); err != nil || count != 0 {
		t.Fatal("content was not rolled back", count, err)
	}
}

func TestGenericShareRevocationAndMetadataCleanupCanRetry(t *testing.T) {
	db := openTestDB(t)
	share := genericTestShare(t)
	resource := ConversationShareResource{ID: "0123456789abcdef01234567", Name: "report.txt", MIMEType: "text/plain", SHA256: strings.Repeat("a", 64), Path: "report.txt"}
	if _, err := db.CreateShareWithResources(t.Context(), share, []ConversationShareResource{resource}); err != nil {
		t.Fatal(err)
	}
	if err := db.RevokeShare(t.Context(), share.ID, "other-owner", time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatal("cross-owner revocation", err)
	}
	if got, err := db.AccessPublicShare(t.Context(), share.ID); err != nil || got.Kind != share.Kind || got.SnapshotVersion != 3 || got.ConversationID != "" {
		t.Fatal("generic content changed", err)
	}
	if err := db.RevokeShare(t.Context(), share.ID, share.OwnerUserID, time.Now()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := db.DeleteShareResourceMetadata(ctx, share.ID); err == nil {
		t.Fatal("canceled cleanup succeeded")
	}
	if ids, err := db.ShareResourceCleanupIDs(t.Context()); err != nil || len(ids) != 1 || ids[0] != share.ID {
		t.Fatal("cleanup lost retry metadata", ids, err)
	}
	if _, err := db.ReadPublicShareResource(t.Context(), share.ID, resource.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("failed cleanup reopened the resource", err)
	}
	if err := db.DeleteShareResourceMetadata(t.Context(), share.ID); err != nil {
		t.Fatal(err)
	}
	if ids, err := db.ShareResourceCleanupIDs(t.Context()); err != nil || len(ids) != 0 {
		t.Fatal("cleanup retry failed", ids, err)
	}
}
