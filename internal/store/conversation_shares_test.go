package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestConversationShareCreateReadAndRevoke(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	snapshot := []byte(`{"schemaVersion":1,"title":"Release plan","createdAt":1700000000000,"updatedAt":1700000001000,"entries":[{"type":"message","role":"user","content":"hello"}]}`)
	share, err := db.CreateConversationShare(ctx, "owner-a", "Release plan", snapshot)
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
	if string(found.SnapshotJSON) != string(snapshot) || found.OwnerUserID != "owner-a" {
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
