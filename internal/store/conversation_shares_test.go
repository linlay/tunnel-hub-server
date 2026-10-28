package store

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSharesAreIndependentPermanentSnapshots(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	snapshot := []byte(`{"version":1,"title":"Release plan"}`)
	first, err := db.CreateConversationShare(ctx, "owner-a", "chat-a", 1, snapshot, now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := db.CreateConversationShare(ctx, "owner-a", "chat-a", 1, snapshot, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID || !strings.HasPrefix(first.ID, "share_") || len(first.ID) < 30 {
		t.Fatal("shares did not get independent opaque IDs")
	}
	snapshot[0] = 'x'
	found, err := db.AccessPublicConversationShare(ctx, first.ID)
	if err != nil || bytes.Equal(found.SnapshotJSON, snapshot) || found.Title != "Release plan" {
		t.Fatal("snapshot did not freeze", err)
	}
	if err := db.RecordConversationShareAccess(ctx, first.ID, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordConversationShareAccess(ctx, first.ID, now); err != nil {
		t.Fatal(err)
	}
	listed, err := db.ListConversationShares(ctx, "owner-a")
	if err != nil || len(listed) != 2 || listed[0].ID != second.ID || listed[1].LastAccessedAt == nil || !listed[1].LastAccessedAt.Equal(databaseTime(now.Add(time.Hour))) {
		t.Fatalf("list metadata %+v %v", listed, err)
	}
	if len(listed[0].SnapshotJSON) != 0 || len(listed[0].DescriptionJSON) != 0 {
		t.Fatal("list read content BLOBs")
	}
	if others, err := db.ListShares(ctx, "other"); err != nil || len(others) != 0 {
		t.Fatal("cross-owner list", err)
	}
	if err := db.RevokeConversationShare(ctx, first.ID, "other", now); !errors.Is(err, ErrNotFound) {
		t.Fatal("cross-owner revoke", err)
	}
	if err := db.RevokeConversationShare(ctx, first.ID, "owner-a", now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AccessPublicConversationShare(ctx, first.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("revoked snapshot read", err)
	}
	if _, err := db.AccessPublicConversationShare(ctx, second.ID); err != nil {
		t.Fatal("independent snapshot affected", err)
	}
	if err := db.RevokeConversationShare(ctx, first.ID, "owner-a", now); !errors.Is(err, ErrNotFound) {
		t.Fatal("repeat revoke", err)
	}
}

func TestGenericShareDoesNotRequireConversationOrSnapshotV1(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	id, err := NewConversationShareID()
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.CreateShareWithResources(ctx, ConversationShare{ID: id, OwnerUserID: "owner", Kind: "static-report", Title: "Report", SnapshotVersion: 3, SnapshotJSON: []byte(`{"revision":3}`), DescriptionJSON: []byte(`{"protocolVersion":1}`), CreatedAt: time.Now()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.AccessPublicConversationShare(ctx, id)
	if err != nil || got.ConversationID != "" || got.SnapshotVersion != 3 || got.Kind != "static-report" {
		t.Fatal("generic share rejected", err)
	}
	if items, err := db.ListShares(ctx, "owner"); err != nil || len(items) != 1 || items[0].Title != "Report" {
		t.Fatal("generic list", err)
	}
	if items, err := db.ListConversationShares(ctx, "owner"); err != nil || len(items) != 0 {
		t.Fatal("legacy list included generic share", err)
	}
}

func TestConversationShareCreateValidatesSnapshot(t *testing.T) {
	db := openTestDB(t)
	for _, tc := range []struct {
		name, owner, conversation string
		version                   int
		body                      []byte
	}{
		{"owner", "", "chat", 1, []byte(`{}`)},
		{"conversation", "owner", "", 1, []byte(`{}`)},
		{"long conversation", "owner", strings.Repeat("x", 256), 1, []byte(`{}`)},
		{"version", "owner", "chat", 3, []byte(`{}`)},
		{"empty", "owner", "chat", 1, nil},
		{"invalid JSON", "owner", "chat", 1, []byte(`x`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := db.CreateConversationShare(context.Background(), tc.owner, tc.conversation, tc.version, tc.body, time.Now()); err == nil {
				t.Fatal("invalid snapshot accepted")
			}
		})
	}
}

func timePointer(value time.Time) *time.Time { return &value }

func TestConversationShareLookupDoesNotRequireGeneratedPrefix(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	snapshot := []byte(`{"version":1,"title":"opaque"}`)
	_, err := db.sql.ExecContext(ctx, `INSERT INTO conversation_shares(id,owner_user_id,conversation_id,snapshot_version,snapshot_json,created_at) VALUES(?,?,?,?,?,?)`, "opaque-abc_123", "owner", "chat", 1, snapshot, databaseTime(time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	found, err := db.AccessPublicConversationShare(ctx, "opaque-abc_123")
	if err != nil || !bytes.Equal(found.SnapshotJSON, snapshot) {
		t.Fatal("prefixless share changed", err)
	}
}
