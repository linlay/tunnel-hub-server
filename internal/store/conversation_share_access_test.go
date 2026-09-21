package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestSQLiteConversationShareClaimAndAttachmentLifecycle(t *testing.T) {
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
	share, err := db.CreateConversationShareWithAttachments(ctx, "owner", "chat", ConversationSnapshotVersion, []byte(`{"version":1}`),
		[]ConversationShareAttachment{{ID: "0123456789abcdef01234567", Name: "报告.html", MIMEType: "text/html", Size: int64(len(body)), SHA256: "sha256", Body: body}},
		now, nil, true)
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
			_, claimed, err := db.AccessPublicConversationShare(ctx, share.ID, now, nil, digest[:])
			if err == nil && claimed {
				mu.Lock()
				successes++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if successes != 1 {
		t.Fatalf("claims=%d want=1", successes)
	}
	if _, _, err := db.AccessPublicConversationShare(ctx, share.ID, now, nil, digest[:]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second browser access=%v", err)
	}
	if _, claimed, err := db.AccessPublicConversationShare(ctx, share.ID, now.Add(time.Minute), digest[:], nil); err != nil || claimed {
		t.Fatalf("session continuation=%v %t", err, claimed)
	}
	attachment, err := db.ReadPublicConversationShareAttachment(ctx, share.ID, "0123456789abcdef01234567", now.Add(time.Minute), digest[:])
	if err != nil || string(attachment.Body) != string(body) {
		t.Fatalf("attachment=%+v %v", attachment, err)
	}
	if _, err := db.ReadPublicConversationShareAttachment(ctx, share.ID, "0123456789abcdef01234567", now.Add(31*time.Minute), digest[:]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired session=%v", err)
	}
	if err := db.RevokeConversationShare(ctx, share.ID, "owner", now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ReadPublicConversationShareAttachment(ctx, share.ID, "0123456789abcdef01234567", now.Add(2*time.Minute), digest[:]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked attachment=%v", err)
	}
}
