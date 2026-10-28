package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSQLiteConversationShareConcurrentReadsAndAttachmentRevocation(t *testing.T) {
	ctx := context.Background()
	db, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "shares.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	body := []byte("<h1>report</h1>")
	digest := sha256.Sum256(body)
	shareID, err := NewConversationShareID()
	if err != nil {
		t.Fatal(err)
	}
	share, err := db.CreateConversationShareWithResources(ctx, shareID, "owner", "chat", ConversationSnapshotVersion, []byte(`{"version":1}`),
		[]ConversationShareResource{{ID: "0123456789abcdef01234567", Name: "报告.html", MIMEType: "text/html", Size: int64(len(body)), SHA256: hex.EncodeToString(digest[:])}},
		now)
	if err != nil {
		t.Fatal(err)
	}
	var successes int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := db.AccessPublicConversationShare(ctx, share.ID)
			if err == nil {
				mu.Lock()
				successes++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if successes != 8 {
		t.Fatalf("reads=%d want=8", successes)
	}
	resource, err := db.ReadPublicConversationShareResource(ctx, share.ID, "0123456789abcdef01234567")
	if err != nil || resource.Size != int64(len(body)) {
		t.Fatalf("resource=%+v %v", resource, err)
	}
	if _, err := db.ReadPublicConversationShareResource(ctx, share.ID, "0123456789abcdef01234567"); err != nil {
		t.Fatalf("permanent resource read=%v", err)
	}
	if err := db.RevokeConversationShare(ctx, share.ID, "owner", now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ReadPublicConversationShareResource(ctx, share.ID, "0123456789abcdef01234567"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked attachment=%v", err)
	}
}

func TestSQLiteConversationShareResourceMetadataTransactionRollsBack(t *testing.T) {
	ctx := context.Background()
	db, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "shares.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.ExecContext(ctx, `CREATE TRIGGER reject_test_resource
		BEFORE INSERT ON conversation_share_resources
		WHEN NEW.resource_id = 'ffffffffffffffffffffffff'
		BEGIN SELECT RAISE(ABORT, 'reject test resource'); END`); err != nil {
		t.Fatal(err)
	}
	shareID, err := NewConversationShareID()
	if err != nil {
		t.Fatal(err)
	}
	resource := ConversationShareResource{
		ID: "ffffffffffffffffffffffff", Name: "report.pdf", MIMEType: "application/pdf",
		Size: 1, SHA256: strings.Repeat("a", 64),
	}
	if _, err := db.CreateConversationShareWithResources(ctx, shareID, "owner", "chat", ConversationSnapshotVersion,
		[]byte(`{"version":1}`), []ConversationShareResource{resource}, time.Now()); err == nil {
		t.Fatal("resource metadata failure unexpectedly committed")
	}
	var count int
	if err := db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversation_shares WHERE id = ?`, shareID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rolled-back share count=%d error=%v", count, err)
	}
}

func TestShareRevocationMetadataCleanupCanRetry(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	id, err := NewConversationShareID()
	if err != nil {
		t.Fatal(err)
	}
	resource := ConversationShareResource{ID: "0123456789abcdef01234567", Name: "report.txt", MIMEType: "text/plain", Size: 0, SHA256: strings.Repeat("a", 64)}
	if _, err := db.CreateConversationShareWithResources(ctx, id, "owner", "chat", 1, []byte(`{}`), []ConversationShareResource{resource}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := db.RevokeConversationShare(ctx, id, "owner", time.Now()); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := db.DeleteConversationShareResourceMetadata(canceled, id); err == nil {
		t.Fatal("canceled cleanup unexpectedly succeeded")
	}
	if ids, err := db.ConversationShareResourceCleanupIDs(ctx); err != nil || len(ids) != 1 || ids[0] != id {
		t.Fatal("retry metadata lost", err)
	}
	if _, err := db.ReadPublicConversationShareResource(ctx, id, resource.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("failed cleanup reopened resource", err)
	}
	if err := db.DeleteConversationShareResourceMetadata(ctx, id); err != nil {
		t.Fatal(err)
	}
	if ids, err := db.ConversationShareResourceCleanupIDs(ctx); err != nil || len(ids) != 0 {
		t.Fatal("cleanup retry failed", err)
	}
}
