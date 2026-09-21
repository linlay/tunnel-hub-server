package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSQLiteSchemaLifecycleAndConnectionSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.sqlite")
	db, err := OpenSQLite(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	for query, want := range map[string]string{
		`PRAGMA foreign_keys`: "1",
		`PRAGMA journal_mode`: "wal",
		`PRAGMA busy_timeout`: "5000",
		`PRAGMA user_version`: "2",
	} {
		var got string
		if err := db.sql.QueryRow(query).Scan(&got); err != nil || !strings.EqualFold(got, want) {
			t.Fatalf("%s = %q, %v; want %q", query, got, err, want)
		}
	}
	user, err := db.CreateAdminUser(context.Background(), "admin", "password")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = OpenSQLite(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetAdminUser(context.Background(), user.ID); err != nil {
		t.Fatalf("reopened database lost data: %v", err)
	}
	if _, err := db.CreateRoute(context.Background(), "missing.example.test", "http://localhost", true, "missing-token"); err == nil {
		t.Fatal("foreign key was not enforced")
	}
}

func TestSQLiteRejectsUnsupportedSchemas(t *testing.T) {
	t.Run("unversioned historical schema", func(t *testing.T) {
		db, err := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "relay.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		if _, err := db.sql.Exec(`CREATE TABLE legacy_data (id INTEGER PRIMARY KEY)`); err != nil {
			t.Fatal(err)
		}
		if err := db.Migrate(context.Background()); err == nil || !strings.Contains(err.Error(), "unsupported unversioned schema") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("unknown schema version", func(t *testing.T) {
		db, err := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "relay.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		if _, err := db.sql.Exec(`PRAGMA user_version = 3`); err != nil {
			t.Fatal(err)
		}
		if err := db.Migrate(context.Background()); err == nil || !strings.Contains(err.Error(), "version 3") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestSQLiteCanceledInitializationCanRetry(t *testing.T) {
	db, err := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "relay.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := db.Migrate(ctx); err == nil {
		t.Fatal("canceled initialization succeeded")
	}
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("retry initialization: %v", err)
	}
}

func TestSQLiteFailedSchemaInitializationRollsBack(t *testing.T) {
	db, err := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "relay.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	originalSchema := sqliteSchema
	sqliteSchema = `CREATE TABLE should_rollback (id INTEGER PRIMARY KEY); INVALID SQL`
	t.Cleanup(func() { sqliteSchema = originalSchema })
	if err := db.Migrate(context.Background()); err == nil {
		t.Fatal("invalid schema initialization succeeded")
	}
	var tables, version int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'should_rollback'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if err := db.sql.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if tables != 0 || version != 0 {
		t.Fatalf("failed initialization left tables=%d version=%d", tables, version)
	}
	sqliteSchema = originalSchema
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("retry initialization: %v", err)
	}
}

func TestSQLiteDuplicateKeyClassification(t *testing.T) {
	db := openSQLiteTestDB(t)
	if _, err := db.CreateAdminUser(context.Background(), "admin", "password"); err != nil {
		t.Fatal(err)
	}
	_, err := db.CreateAdminUser(context.Background(), "admin", "password")
	if !db.isDuplicateKey(err) || !db.isDuplicateKey(fmt.Errorf("wrapped: %w", err)) {
		t.Fatalf("unique error not recognized: %v", err)
	}
	if _, err := db.sql.Exec(`INSERT INTO conversation_shares (id, owner_user_id, conversation_id, snapshot_version, snapshot_json, created_at, expires_at, single_use) VALUES ('bad', 'owner', 'chat', 1, '{}', ?, ?, 1)`, time.Now(), time.Now().Add(time.Hour)); err == nil || db.isDuplicateKey(err) {
		t.Fatalf("check constraint classification = %v", err)
	}
}

func TestSQLiteConcurrentWritesPreserveInvariants(t *testing.T) {
	databases := openSQLiteTestDBPair(t)
	db := databases[0]
	ctx := context.Background()
	const workers = 16
	results := make(chan RegisterDesktopDeviceResult, workers)
	runConcurrent(t, workers, func(i int) error {
		result, err := databases[i%len(databases)].RegisterDesktopDevice(ctx, RegisterDesktopDeviceInput{
			OwnerUserID: "owner", DeviceID: "device", PublicHost: fmt.Sprintf("d%d.m.example.test", i),
		})
		if err == nil {
			results <- result
		}
		return err
	})
	close(results)
	created := 0
	for result := range results {
		if result.Created {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("created = %d", created)
	}
	share, err := db.CreateConversationShare(ctx, "owner", "chat", 1, []byte(`{"version":1}`), time.Now(), nil, true)
	if err != nil {
		t.Fatal(err)
	}
	acquireResults := make(chan error, workers)
	var acquireWait sync.WaitGroup
	for i := 0; i < workers; i++ {
		acquireWait.Add(1)
		go func(database *DB, index int) {
			defer acquireWait.Done()
			digest := sha256.Sum256([]byte(fmt.Sprintf("browser-%d", index)))
			_, _, err := database.AccessPublicConversationShare(ctx, share.ID, time.Now(), nil, digest[:])
			acquireResults <- err
		}(databases[i%len(databases)], i)
	}
	acquireWait.Wait()
	close(acquireResults)
	acquired, missing := 0, 0
	for err := range acquireResults {
		if err == nil {
			acquired++
		} else if errors.Is(err, ErrNotFound) {
			missing++
		} else {
			t.Fatal(err)
		}
	}
	if acquired != 1 || missing != workers-1 {
		t.Fatalf("acquired=%d missing=%d", acquired, missing)
	}

	first, err := db.CreateAdminUser(ctx, "first", "password")
	if err != nil {
		t.Fatal(err)
	}
	second, err := db.CreateAdminUser(ctx, "second", "password")
	if err != nil {
		t.Fatal(err)
	}
	disableResults := make(chan error, 2)
	var wait sync.WaitGroup
	for i, id := range []string{first.ID, second.ID} {
		wait.Add(1)
		go func(database *DB) {
			defer wait.Done()
			_, err := database.DisableAdminUser(ctx, id)
			disableResults <- err
		}(databases[i])
	}
	wait.Wait()
	close(disableResults)
	succeeded, protected := 0, 0
	for err := range disableResults {
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

func TestSQLiteWebAppFailureRollsBackRoute(t *testing.T) {
	db := openSQLiteTestDB(t)
	ctx := context.Background()
	if _, err := db.RegisterDesktopDevice(ctx, RegisterDesktopDeviceInput{OwnerUserID: "owner", DeviceID: "device", PublicHost: "device.m.example.test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`
		CREATE TRIGGER reject_notes BEFORE INSERT ON desktop_webapps
		WHEN NEW.name = 'notes'
		BEGIN
			SELECT RAISE(ABORT, 'reject notes');
		END
	`); err != nil {
		t.Fatal(err)
	}
	_, err := db.RegisterDesktopWebApp(ctx, RegisterDesktopWebAppInput{OwnerUserID: "owner", DeviceID: "device", Name: "notes", PublicHost: "notes-wa.example.test", TargetURL: "http://localhost", Active: true})
	if err == nil {
		t.Fatal("expected WebApp insert failure")
	}
	routes, err := db.ListRoutes(ctx)
	if err != nil || len(routes) != 0 {
		t.Fatalf("orphan routes = %d, error = %v", len(routes), err)
	}
}

func TestSQLiteCanceledLockWaitLeavesNoPartialWrite(t *testing.T) {
	databases := openSQLiteTestDBPair(t)
	lock, err := databases[0].beginWriteTx(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = databases[1].RegisterDesktopDevice(ctx, RegisterDesktopDeviceInput{OwnerUserID: "owner", DeviceID: "device", PublicHost: "device.m.example.test"})
	if err == nil {
		t.Fatal("write succeeded while another connection held the write lock")
	}
	if err := lock.Rollback(); err != nil {
		t.Fatal(err)
	}
	devices, err := databases[0].ListDesktopDevices(context.Background())
	if err != nil || len(devices) != 0 {
		t.Fatalf("devices = %+v, error = %v", devices, err)
	}
}

func TestSQLiteTrafficSearchAndAggregateTime(t *testing.T) {
	db := openSQLiteTestDB(t)
	now := time.Date(2026, 9, 17, 1, 2, 3, 123456000, time.UTC)
	if err := db.RecordTrafficEvent(context.Background(), TrafficEvent{ObjectType: "webapp", PublicHost: "Demo.Example.Test", Kind: "http", Path: "/Hello", OccurredAt: now}); err != nil {
		t.Fatal(err)
	}
	events, err := db.ListTrafficEvents(context.Background(), 10, "webapp", "demo.example")
	if err != nil || len(events) != 1 {
		t.Fatalf("events = %+v, error = %v", events, err)
	}
	stats, err := db.TrafficTotals(context.Background())
	if err != nil || stats.LastAt == nil || !stats.LastAt.Equal(now) {
		t.Fatalf("stats = %+v, error = %v", stats, err)
	}
}

func TestSQLiteConversationAccessTimeDoesNotRegress(t *testing.T) {
	db := openSQLiteTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 17, 1, 2, 3, 123456000, time.UTC)
	share, err := db.CreateConversationShare(ctx, "owner", "chat", 1, []byte(`{"version":1}`), now, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RecordConversationShareAccess(ctx, share.ID, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordConversationShareAccess(ctx, share.ID, now); err != nil {
		t.Fatal(err)
	}
	listed, err := db.ListConversationShares(ctx, "owner", now)
	if err != nil || len(listed) != 1 || listed[0].LastAccessedAt == nil || !listed[0].LastAccessedAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("shares = %+v, error = %v", listed, err)
	}
}

func openSQLiteTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "relay.sqlite"))
	return finishOpeningTestDB(t, db, err)
}

func openSQLiteTestDBPair(t *testing.T) [2]*DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "relay.sqlite")
	var databases [2]*DB
	for i := range databases {
		db, err := OpenSQLite(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		databases[i] = db
		t.Cleanup(func() { _ = db.Close() })
		if err := db.Migrate(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	return databases
}
