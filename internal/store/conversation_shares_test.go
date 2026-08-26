package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConversationShareCreateReadExpireAndRevoke(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, time.August, 17, 1, 2, 3, 0, time.UTC)
	expiresAt := now.Add(720 * time.Hour)
	html := []byte("<!doctype html><title>Release plan</title>")
	share, err := db.CreateConversationShare(ctx, "owner-a", "chat-a", ConversationDocumentVersion, html, now, &expiresAt, false)
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
	listed, err := db.ListConversationShares(ctx, "owner-a", "chat-a", now)
	if err != nil || len(listed) != 1 || listed[0].ID != share.ID || listed[0].LastAccessedAt != nil {
		t.Fatalf("initial list=%#v err=%v", listed, err)
	}
	if otherOwner, err := db.ListConversationShares(ctx, "owner-b", "chat-a", now); err != nil || len(otherOwner) != 0 {
		t.Fatalf("other owner list=%#v err=%v", otherOwner, err)
	}
	found, err := db.AcquirePublicConversationShare(ctx, share.ID, expiresAt.Add(-time.Nanosecond))
	if err != nil {
		t.Fatalf("get share: %v", err)
	}
	if string(found.HTMLDocument) != string(html) || found.DocumentVersion != ConversationDocumentVersion {
		t.Fatalf("unexpected public share: %#v", found)
	}
	accessedAt := now.Add(time.Minute)
	if err := db.RecordConversationShareAccess(ctx, share.ID, accessedAt); err != nil {
		t.Fatalf("record access: %v", err)
	}
	listed, err = db.ListConversationShares(ctx, "owner-a", "chat-a", accessedAt)
	if err != nil || len(listed) != 1 || listed[0].LastAccessedAt == nil || !listed[0].LastAccessedAt.Equal(accessedAt) {
		t.Fatalf("accessed list=%#v err=%v", listed, err)
	}
	if _, err := db.AcquirePublicConversationShare(ctx, share.ID, expiresAt); !errors.Is(err, ErrNotFound) {
		t.Fatalf("share must expire at the boundary, got %v", err)
	}
	if expired, err := db.ListConversationShares(ctx, "owner-a", "chat-a", expiresAt); err != nil || len(expired) != 0 {
		t.Fatalf("expired list=%#v err=%v", expired, err)
	}
	if err := db.RevokeConversationShare(ctx, share.ID, "owner-b", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other owner revoke error=%v", err)
	}
	if err := db.RevokeConversationShare(ctx, share.ID, "owner-a", now); err != nil {
		t.Fatalf("revoke share: %v", err)
	}
	if revoked, err := db.ListConversationShares(ctx, "owner-a", "chat-a", now); err != nil || len(revoked) != 0 {
		t.Fatalf("revoked list=%#v err=%v", revoked, err)
	}
	if _, err := db.AcquirePublicConversationShare(ctx, share.ID, now); !errors.Is(err, ErrNotFound) {
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
		ConversationDocumentVersion,
		[]byte("<p>permanent</p>"),
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
	if _, err := db.AcquirePublicConversationShare(ctx, share.ID, now.Add(100*365*24*time.Hour)); err != nil {
		t.Fatalf("permanent share should remain readable: %v", err)
	}
	if err := db.RevokeConversationShare(ctx, share.ID, "owner-a", now); err != nil {
		t.Fatalf("revoke permanent share: %v", err)
	}
	if _, err := db.AcquirePublicConversationShare(ctx, share.ID, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked permanent share should be hidden, got %v", err)
	}
}

func TestConversationShareCreateValidatesDocument(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC()
	for _, tc := range []struct {
		name           string
		owner          string
		conversationID string
		version        int
		document       []byte
		expiresAt      *time.Time
		singleUse      bool
	}{
		{name: "owner", conversationID: "chat-a", version: 1, document: []byte("x"), expiresAt: timePointer(now.Add(time.Hour))},
		{name: "conversation empty", owner: "owner", version: 1, document: []byte("x"), expiresAt: timePointer(now.Add(time.Hour))},
		{name: "conversation", owner: "owner", conversationID: strings.Repeat("x", MaxConversationShareConversationIDBytes+1), version: 1, document: []byte("x"), expiresAt: timePointer(now.Add(time.Hour))},
		{name: "version", owner: "owner", conversationID: "chat-a", version: 2, document: []byte("x"), expiresAt: timePointer(now.Add(time.Hour))},
		{name: "empty", owner: "owner", conversationID: "chat-a", version: 1, expiresAt: timePointer(now.Add(time.Hour))},
		{name: "expiration", owner: "owner", conversationID: "chat-a", version: 1, document: []byte("x"), expiresAt: timePointer(now)},
		{name: "single use expiration", owner: "owner", conversationID: "chat-a", version: 1, document: []byte("x"), expiresAt: timePointer(now.Add(time.Hour)), singleUse: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := db.CreateConversationShare(context.Background(), tc.owner, tc.conversationID, tc.version, tc.document, now, tc.expiresAt, tc.singleUse); err == nil {
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
	html := []byte("<p>opaque</p>")
	_, err := db.sql.ExecContext(ctx, `
		INSERT INTO conversation_shares (
			id, owner_user_id, conversation_id, document_version, html_document, created_at, expires_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)
	`, "opaque-abc_123", "owner-a", "chat-a", ConversationDocumentVersion, html, now, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("insert prefixless share: %v", err)
	}
	found, err := db.AcquirePublicConversationShare(ctx, "opaque-abc_123", now)
	if err != nil || string(found.HTMLDocument) != string(html) {
		t.Fatalf("prefixless lookup=%#v err=%v", found, err)
	}
}

func TestConversationShareSingleUseIsAtomicallyDeletedOnFirstAcquire(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, time.August, 17, 1, 2, 3, 0, time.UTC)
	html := []byte("<!doctype html><title>Read once</title>")
	share, err := db.CreateConversationShare(
		ctx,
		"owner-a",
		"chat-once",
		ConversationDocumentVersion,
		html,
		now,
		nil,
		true,
	)
	if err != nil {
		t.Fatalf("create single-use share: %v", err)
	}
	if !share.SingleUse || share.ExpiresAt != nil {
		t.Fatalf("unexpected single-use share: %#v", share)
	}

	const readers = 16
	results := make(chan error, readers)
	var wait sync.WaitGroup
	wait.Add(readers)
	for range readers {
		go func() {
			defer wait.Done()
			acquired, acquireErr := db.AcquirePublicConversationShare(ctx, share.ID, now)
			if acquireErr == nil && (string(acquired.HTMLDocument) != string(html) || !acquired.SingleUse) {
				acquireErr = errors.New("acquired single-use document does not match")
			}
			results <- acquireErr
		}()
	}
	wait.Wait()
	close(results)

	successes := 0
	notFound := 0
	for result := range results {
		switch {
		case result == nil:
			successes++
		case errors.Is(result, ErrNotFound):
			notFound++
		default:
			t.Fatalf("acquire error: %v", result)
		}
	}
	if successes != 1 || notFound != readers-1 {
		t.Fatalf("successes=%d notFound=%d", successes, notFound)
	}
	listed, err := db.ListConversationShares(ctx, "owner-a", "chat-once", now)
	if err != nil || len(listed) != 0 {
		t.Fatalf("single-use share remains after acquire: shares=%#v err=%v", listed, err)
	}
	var remaining int
	if err := db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversation_shares WHERE id = ?`, share.ID).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("single-use row was not deleted: count=%d err=%v", remaining, err)
	}
}
