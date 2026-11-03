package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestConversationShareCreateReadExpireAndRevoke(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, time.August, 17, 1, 2, 3, 0, time.UTC)
	expiresAt := now.Add(720 * time.Hour)
	snapshot := []byte(`{"version":1,"title":"Release plan"}`)
	share, err := db.CreateConversationShare(ctx, "owner-a", "chat-a", 1, snapshot, now, &expiresAt, false)
	if err != nil {
		t.Fatalf("create share: %v", err)
	}
	if !strings.HasPrefix(share.ID, "share_") || len(share.ID) < 30 {
		t.Fatalf("share id is not opaque: %q", share.ID)
	}
	if !share.CreatedAt.Equal(now) || share.ExpiresAt == nil || !share.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("unexpected timestamps: %#v", share)
	}
	if share.ConversationID != "chat-a" || share.LastAccessedAt != nil {
		t.Fatalf("unexpected share metadata: %#v", share)
	}
	listed, err := db.ListConversationShares(ctx, "owner-a", now)
	if err != nil || len(listed) != 1 || listed[0].ID != share.ID || listed[0].LastAccessedAt != nil {
		t.Fatalf("initial list=%#v err=%v", listed, err)
	}
	if otherOwner, err := db.ListConversationShares(ctx, "owner-b", now); err != nil || len(otherOwner) != 0 {
		t.Fatalf("other owner list=%#v err=%v", otherOwner, err)
	}
	found, _, err := db.AccessPublicConversationShare(ctx, share.ID, expiresAt.Add(-time.Nanosecond), nil, nil)
	if err != nil {
		t.Fatalf("get share: %v", err)
	}
	if string(found.SnapshotJSON) != string(snapshot) || found.SnapshotVersion != 1 {
		t.Fatalf("unexpected public share: %#v", found)
	}
	accessedAt := now.Add(time.Minute)
	if err := db.RecordConversationShareAccess(ctx, share.ID, accessedAt); err != nil {
		t.Fatalf("record access: %v", err)
	}
	listed, err = db.ListConversationShares(ctx, "owner-a", accessedAt)
	if err != nil || len(listed) != 1 || listed[0].LastAccessedAt == nil || !listed[0].LastAccessedAt.Equal(accessedAt) {
		t.Fatalf("accessed list=%#v err=%v", listed, err)
	}
	if _, _, err := db.AccessPublicConversationShare(ctx, share.ID, expiresAt, nil, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("share must expire at the boundary, got %v", err)
	}
	if expired, err := db.ListConversationShares(ctx, "owner-a", expiresAt); err != nil || len(expired) != 0 {
		t.Fatalf("expired list=%#v err=%v", expired, err)
	}
	if err := db.RevokeConversationShare(ctx, share.ID, "owner-b", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other owner revoke error=%v", err)
	}
	if err := db.RevokeConversationShare(ctx, share.ID, "owner-a", now); err != nil {
		t.Fatalf("revoke share: %v", err)
	}
	if revoked, err := db.ListConversationShares(ctx, "owner-a", now); err != nil || len(revoked) != 0 {
		t.Fatalf("revoked list=%#v err=%v", revoked, err)
	}
	if _, _, err := db.AccessPublicConversationShare(ctx, share.ID, now, nil, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked share should be hidden, got %v", err)
	}
	if err := db.RevokeConversationShare(ctx, share.ID, "owner-a", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second revoke error=%v", err)
	}
}

func TestConversationSharePermanentRemainsReadableUntilRevoked(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, time.August, 17, 1, 2, 3, 0, time.UTC)
	share, err := db.CreateConversationShare(
		ctx,
		"owner-a",
		"chat-permanent",
		1,
		[]byte(`{"version":1,"title":"permanent"}`),
		now,
		nil,
		false,
	)
	if err != nil {
		t.Fatalf("create permanent share: %v", err)
	}
	if share.ExpiresAt != nil {
		t.Fatalf("permanent share expiration=%v", share.ExpiresAt)
	}
	if _, _, err := db.AccessPublicConversationShare(ctx, share.ID, now.Add(100*365*24*time.Hour), nil, nil); err != nil {
		t.Fatalf("permanent share should remain readable: %v", err)
	}
	if err := db.RevokeConversationShare(ctx, share.ID, "owner-a", now); err != nil {
		t.Fatalf("revoke permanent share: %v", err)
	}
	if _, _, err := db.AccessPublicConversationShare(ctx, share.ID, now, nil, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked permanent share should be hidden, got %v", err)
	}
}

func TestConversationShareListReturnsAllOwnerConversationsNewestFirst(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, time.August, 17, 1, 2, 3, 0, time.UTC)
	expiresAt := now.Add(time.Hour)
	first, err := db.CreateConversationShare(
		ctx, "owner-a", "chat-a", 1,
		[]byte(`{"version":1,"title":"first"}`), now, &expiresAt, false,
	)
	if err != nil {
		t.Fatalf("create first share: %v", err)
	}
	second, err := db.CreateConversationShare(
		ctx, "owner-a", "chat-b", 1,
		[]byte(`{"version":1,"title":"second"}`), now.Add(time.Minute), &expiresAt, false,
	)
	if err != nil {
		t.Fatalf("create second share: %v", err)
	}
	if _, err := db.CreateConversationShare(
		ctx, "owner-b", "chat-c", 1,
		[]byte(`{"version":1,"title":"other owner"}`), now.Add(2*time.Minute), &expiresAt, false,
	); err != nil {
		t.Fatalf("create other owner share: %v", err)
	}

	listed, err := db.ListConversationShares(ctx, "owner-a", now)
	if err != nil {
		t.Fatalf("list shares: %v", err)
	}
	if len(listed) != 2 || listed[0].ID != second.ID || listed[1].ID != first.ID {
		t.Fatalf("shares=%#v", listed)
	}
	if listed[0].ConversationID != "chat-b" || listed[1].ConversationID != "chat-a" {
		t.Fatalf("conversation order=%q, %q", listed[0].ConversationID, listed[1].ConversationID)
	}
}

func TestConversationShareCreateValidatesSnapshot(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC()
	for _, tc := range []struct {
		name           string
		owner          string
		conversationID string
		version        int
		snapshot       []byte
		expiresAt      *time.Time
		singleUse      bool
	}{
		{name: "owner", conversationID: "chat-a", version: 1, snapshot: []byte("x"), expiresAt: timePointer(now.Add(time.Hour))},
		{name: "conversation empty", owner: "owner", version: 1, snapshot: []byte("x"), expiresAt: timePointer(now.Add(time.Hour))},
		{name: "conversation", owner: "owner", conversationID: strings.Repeat("x", MaxConversationShareConversationIDBytes+1), version: 1, snapshot: []byte("x"), expiresAt: timePointer(now.Add(time.Hour))},
		{name: "version", owner: "owner", conversationID: "chat-a", version: 3, snapshot: []byte("x"), expiresAt: timePointer(now.Add(time.Hour))},
		{name: "empty", owner: "owner", conversationID: "chat-a", version: 1, expiresAt: timePointer(now.Add(time.Hour))},
		{name: "expiration", owner: "owner", conversationID: "chat-a", version: 1, snapshot: []byte("x"), expiresAt: timePointer(now)},
		{name: "single use expiration", owner: "owner", conversationID: "chat-a", version: 1, snapshot: []byte("x"), expiresAt: timePointer(now.Add(time.Hour)), singleUse: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := db.CreateConversationShare(context.Background(), tc.owner, tc.conversationID, tc.version, tc.snapshot, now, tc.expiresAt, tc.singleUse); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func timePointer(value time.Time) *time.Time {
	return &value
}

func TestConversationShareLookupDoesNotRequireGeneratedPrefix(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	snapshot := []byte(`{"version":1,"title":"opaque"}`)
	_, err := db.sql.ExecContext(ctx, `
		INSERT INTO conversation_shares (
			id, owner_user_id, conversation_id, snapshot_version, snapshot_json, created_at, expires_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)
	`, "opaque-abc_123", "owner-a", "chat-a", 1, snapshot, now, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("insert prefixless share: %v", err)
	}
	found, _, err := db.AccessPublicConversationShare(ctx, "opaque-abc_123", now, nil, nil)
	if err != nil || string(found.SnapshotJSON) != string(snapshot) {
		t.Fatalf("prefixless lookup=%#v err=%v", found, err)
	}
}
