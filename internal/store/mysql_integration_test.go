package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"example.invalid/tunnel-hub-server/internal/testutil/mysqltest"
)

func TestMySQLSchemaInitializationCanResume(t *testing.T) {
	requireMySQLIntegration(t)
	cfg := mysqltest.NewConfig(t)
	db, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	// Simulate a previous startup that only created the first table.
	if _, err := db.sql.ExecContext(context.Background(), strings.Split(schema, ";")[0]); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := db.Migrate(ctx); err == nil {
		t.Fatal("canceled initialization succeeded")
	}
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	user, _, err := db.EnsureAdminUser(context.Background(), "admin", "password")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetAdminUser(context.Background(), user.ID); err != nil {
		t.Fatal("repeated initialization lost user", err)
	}
	if _, err := db.CreateRoute(context.Background(), "missing.example.test", "http://localhost", true, "missing-token"); err == nil {
		t.Fatal("foreign key was not enforced")
	}
	var unusedTables int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = 'admin_api_keys'`).Scan(&unusedTables); err != nil || unusedTables != 0 {
		t.Fatalf("unused tables = %d, error = %v", unusedTables, err)
	}
}

func TestMySQLConcurrentDesktopRegistrations(t *testing.T) {
	db := openMySQLTestDB(t)
	ctx := context.Background()
	const workers = 16
	results := make(chan RegisterDesktopDeviceResult, workers)
	runConcurrent(t, workers, func(i int) error {
		result, err := db.RegisterDesktopDevice(ctx, RegisterDesktopDeviceInput{OwnerUserID: "owner", DeviceID: "device", PublicHost: fmt.Sprintf("d%d.m.example.test", i)})
		if err == nil {
			results <- result
		}
		return err
	})
	close(results)
	created, host := 0, ""
	for result := range results {
		if result.Created {
			created++
		}
		if host == "" {
			host = result.Device.PublicHost
		}
		if host != result.Device.PublicHost {
			t.Fatal("concurrent registration changed the public host")
		}
	}
	if created != 1 {
		t.Fatalf("created = %d", created)
	}
	runConcurrent(t, workers, func(i int) error {
		_, err := db.RegisterDesktopWebApp(ctx, RegisterDesktopWebAppInput{OwnerUserID: "owner", DeviceID: "device", Name: "notes", PublicHost: fmt.Sprintf("app%d-wa.example.test", i), TargetURL: "http://localhost:3000", Active: true})
		return err
	})
	apps, err := db.ListDesktopWebApps(ctx)
	if err != nil || len(apps) != 1 {
		t.Fatalf("apps = %d, error = %v", len(apps), err)
	}
	routes, err := db.ListRoutes(ctx)
	if err != nil || len(routes) != 1 {
		t.Fatalf("routes = %d, error = %v", len(routes), err)
	}
	_, err = db.RegisterDesktopDevice(ctx, RegisterDesktopDeviceInput{OwnerUserID: "other", DeviceID: "device", PublicHost: host})
	if !errors.Is(err, ErrDesktopDeviceHostConflict) {
		t.Fatalf("host conflict = %v", err)
	}
}

func TestMySQLWebAppFailureRollsBackRoute(t *testing.T) {
	db := openMySQLTestDB(t)
	ctx := context.Background()
	_, err := db.RegisterDesktopDevice(ctx, RegisterDesktopDeviceInput{OwnerUserID: "owner", DeviceID: "device", PublicHost: "device.m.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	// A temporary test-only constraint forces failure after route insertion.
	if _, err := db.sql.Exec(`ALTER TABLE desktop_webapps ADD CONSTRAINT test_reject_notes CHECK (name <> 'notes')`); err != nil {
		t.Fatal(err)
	}
	_, err = db.RegisterDesktopWebApp(ctx, RegisterDesktopWebAppInput{OwnerUserID: "owner", DeviceID: "device", Name: "notes", PublicHost: "notes-wa.example.test", TargetURL: "http://localhost", Active: true})
	if err == nil {
		t.Fatal("expected WebApp insert failure")
	}
	routes, err := db.ListRoutes(ctx)
	if err != nil || len(routes) != 0 {
		t.Fatalf("orphan routes = %d, error = %v", len(routes), err)
	}
}

func TestMySQLConcurrentAdminDisable(t *testing.T) {
	db := openMySQLTestDB(t)
	ctx := context.Background()
	first, err := db.CreateAdminUser(ctx, "first", "password")
	if err != nil {
		t.Fatal(err)
	}
	second, err := db.CreateAdminUser(ctx, "second", "password")
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{first.ID, second.ID}
	results := make(chan error, 2)
	var wait sync.WaitGroup
	for _, id := range ids {
		wait.Add(1)
		go func() { defer wait.Done(); _, err := db.DisableAdminUser(ctx, id); results <- err }()
	}
	wait.Wait()
	close(results)
	succeeded, protected := 0, 0
	for err := range results {
		if err == nil {
			succeeded++
		} else if errors.Is(err, ErrLastActiveUser) {
			protected++
		} else {
			t.Fatal(err)
		}
	}
	if succeeded != 1 || protected != 1 {
		t.Fatalf("succeeded=%d protected=%d", succeeded, protected)
	}
}

func TestMySQLIdentityBoundariesAndUnchangedUpdates(t *testing.T) {
	db := openMySQLTestDB(t)
	ctx := context.Background()
	for i, owner := range []string{"Owner", "owner", strings.Repeat("中", 255)} {
		_, err := db.RegisterDesktopDevice(ctx, RegisterDesktopDeviceInput{OwnerUserID: owner, DeviceID: "device", PublicHost: fmt.Sprintf("device%d.m.example.test", i)})
		if err != nil {
			t.Fatal(err)
		}
	}
	devices, err := db.ListDesktopDevices(ctx)
	if err != nil || len(devices) != 3 {
		t.Fatalf("devices=%d error=%v", len(devices), err)
	}
	if _, err := db.RegisterDesktopDevice(ctx, RegisterDesktopDeviceInput{OwnerUserID: strings.Repeat("中", 256), DeviceID: "device", PublicHost: "long.m.example.test"}); err == nil {
		t.Fatal("oversized owner accepted")
	}
	user, err := db.CreateAdminUser(ctx, strings.Repeat("中", 255), "password")
	if err != nil {
		t.Fatal(err)
	}
	result, err := db.sql.Exec(`UPDATE admin_users SET username = username WHERE id = ?`, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		t.Fatalf("matched rows=%d error=%v", affected, err)
	}
	if _, err := db.UpdateAdminUser(ctx, user.ID, AdminUserPatch{}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateAdminUser(ctx, strings.Repeat("x", 256), "password"); err == nil {
		t.Fatal("oversized username accepted")
	}
}

func TestMySQLLargeSnapshotAndMicrosecondTimes(t *testing.T) {
	db := openMySQLTestDB(t)
	ctx := context.Background()
	prefix, suffix := []byte(`{"version":1,"title":"`), []byte(`"}`)
	snapshot := append(append(prefix, bytes.Repeat([]byte("x"), MaxConversationSnapshotBytes-len(prefix)-len(suffix))...), suffix...)
	now := time.Date(2026, 9, 16, 12, 0, 0, 123456789, time.FixedZone("offset", 8*60*60))
	share, err := db.CreateConversationShare(ctx, "owner", "chat", 1, snapshot, now, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	found, _, err := db.AccessPublicConversationShare(ctx, share.ID, now, nil, nil)
	if err != nil || !bytes.Equal(snapshot, found.SnapshotJSON) {
		t.Fatal("large snapshot round-trip failed", err)
	}
	if share.CreatedAt.Nanosecond() != 123456000 || share.CreatedAt.Location() != time.UTC {
		t.Fatal("incorrect timestamp normalization")
	}
	listed, err := db.ListConversationShares(ctx, "owner", now)
	if err != nil || len(listed) != 1 || !listed[0].CreatedAt.Equal(share.CreatedAt) {
		t.Fatal("timestamp round-trip failed", err)
	}
	if _, err := db.CreateConversationShare(ctx, "owner", "chat", 1, append(snapshot, ' '), now, nil, false); err == nil {
		t.Fatal("oversized snapshot accepted")
	}
	if err := db.RecordConversationShareAccess(ctx, share.ID, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordConversationShareAccess(ctx, share.ID, now); err != nil {
		t.Fatal(err)
	}
	listed, err = db.ListConversationShares(ctx, "owner", now)
	if err != nil || listed[0].LastAccessedAt == nil || !listed[0].LastAccessedAt.Equal(databaseTime(now.Add(time.Minute))) {
		t.Fatal("access timestamp regressed", err)
	}
}

func TestMySQLConversationShareResourceMetadataTransaction(t *testing.T) {
	db := openMySQLTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
	body := []byte("resource")
	digest := sha256.Sum256(body)
	resource := ConversationShareResource{
		ID: "0123456789abcdef01234567", Name: "report.pdf", MIMEType: "application/pdf",
		Size: int64(len(body)), SHA256: hex.EncodeToString(digest[:]),
	}
	shareID, err := NewConversationShareID()
	if err != nil {
		t.Fatal(err)
	}
	share, err := db.CreateConversationShareWithResources(ctx, shareID, "owner", "chat", ConversationSnapshotVersion,
		[]byte(`{"version":1}`), []ConversationShareResource{resource}, now, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := db.ReadPublicConversationShareResource(ctx, share.ID, resource.ID, now, nil)
	expected := resource
	expected.ShareID = share.ID
	if err != nil || stored != expected {
		t.Fatalf("stored resource=%+v error=%v", stored, err)
	}

	if _, err := db.sql.Exec(`ALTER TABLE conversation_share_resources ADD CONSTRAINT test_reject_resource CHECK (resource_id <> 'ffffffffffffffffffffffff')`); err != nil {
		t.Fatal(err)
	}
	failedID, err := NewConversationShareID()
	if err != nil {
		t.Fatal(err)
	}
	resource.ID = "ffffffffffffffffffffffff"
	if _, err := db.CreateConversationShareWithResources(ctx, failedID, "owner", "chat", ConversationSnapshotVersion,
		[]byte(`{"version":1}`), []ConversationShareResource{resource}, now, nil, false); err == nil {
		t.Fatal("resource metadata failure unexpectedly committed")
	}
	var count int
	if err := db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversation_shares WHERE id = ?`, failedID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rolled-back share count=%d error=%v", count, err)
	}
}

func TestMySQLLegacyConversationShareMigrationCanResumeAfterFileCommit(t *testing.T) {
	db := openMySQLTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
	share, err := db.CreateConversationShare(ctx, "owner", "chat", ConversationSnapshotVersion,
		[]byte(`{"version":1}`), now, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.ExecContext(ctx, `CREATE TABLE conversation_share_attachments (
		share_id VARCHAR(80) NOT NULL, attachment_id VARCHAR(24) NOT NULL,
		name VARCHAR(255) NOT NULL, mime_type VARCHAR(100) NOT NULL,
		size_bytes BIGINT NOT NULL, sha256 CHAR(64) NOT NULL, body LONGBLOB NOT NULL,
		PRIMARY KEY (share_id, attachment_id)
	)`); err != nil {
		t.Fatal(err)
	}
	body := []byte("legacy resource")
	digest := sha256.Sum256(body)
	const resourceID = "0123456789abcdef01234567"
	if _, err := db.sql.ExecContext(ctx, `INSERT INTO conversation_share_attachments
		(share_id, attachment_id, name, mime_type, size_bytes, sha256, body)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, share.ID, resourceID, "legacy.pdf", "application/pdf",
		len(body), hex.EncodeToString(digest[:]), body); err != nil {
		t.Fatal(err)
	}
	resourceDir := t.TempDir()
	preexisting := filepath.Join(resourceDir, "shares", share.ID, "resources", resourceID)
	if err := os.MkdirAll(filepath.Dir(preexisting), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(preexisting, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := db.migrateLegacyConversationShareAttachments(ctx, resourceDir); err != nil {
		t.Fatal(err)
	}
	if err := db.migrateLegacyConversationShareAttachments(ctx, resourceDir); err != nil {
		t.Fatalf("rerun completed migration: %v", err)
	}
	resource, err := db.ReadPublicConversationShareResource(ctx, share.ID, resourceID, now, nil)
	if err != nil || resource.Size != int64(len(body)) || resource.SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("migrated resource=%+v error=%v", resource, err)
	}
	var legacyTables int
	if err := db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.tables
		WHERE table_schema = DATABASE() AND table_name = 'conversation_share_attachments'`).Scan(&legacyTables); err != nil || legacyTables != 0 {
		t.Fatalf("legacy table count=%d error=%v", legacyTables, err)
	}
}

func requireMySQLIntegration(t *testing.T) {
	t.Helper()
	if os.Getenv("TEST_DATABASE_TYPE") == "sqlite" {
		t.Skip("MySQL-specific integration test")
	}
}

func runConcurrent(t *testing.T, workers int, work func(int) error) {
	t.Helper()
	start := make(chan struct{})
	results := make(chan error, workers)
	for i := range workers {
		go func() { <-start; results <- work(i) }()
	}
	close(start)
	for range workers {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
	if t.Failed() {
		t.FailNow()
	}
}
