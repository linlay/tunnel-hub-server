package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"example.invalid/tunnel-hub-server/internal/testutil/mysqltest"
)

func TestAgentRouteAndTokenRemainSupported(t *testing.T) {
	db := openTestDB(t)
	token, err := db.CreateToken(context.Background(), "agent", "zt_agent_secret")
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	route, err := db.CreateRoute(context.Background(), " Demo.Example.Test:443 ", "http://127.0.0.1:3000", true, token.ID)
	if err != nil {
		t.Fatalf("create route: %v", err)
	}
	if route.PublicHost != "demo.example.test" || route.TokenID != token.ID {
		t.Fatalf("route = %+v", route)
	}
	if _, err := db.FindActiveTokenBySecret(context.Background(), "zt_agent_secret"); err != nil {
		t.Fatalf("find token: %v", err)
	}
}

func TestRegisterDesktopDeviceUsesOwnerAndDeviceID(t *testing.T) {
	db := openTestDB(t)
	first, err := db.RegisterDesktopDevice(context.Background(), RegisterDesktopDeviceInput{DeviceID: "mac-lan", DeviceName: "Mac LAN", OwnerUserID: "user-1", OwnerEmail: "one@example.test", PublicHost: "a.m.example.test"})
	if err != nil {
		t.Fatalf("register first: %v", err)
	}
	if !first.Created || first.Device.DeviceKey == "" {
		t.Fatalf("first = %+v", first)
	}
	second, err := db.RegisterDesktopDevice(context.Background(), RegisterDesktopDeviceInput{DeviceID: "mac-lan", DeviceName: "Renamed", OwnerUserID: "user-1", PublicHost: "ignored.m.example.test"})
	if err != nil {
		t.Fatalf("register second: %v", err)
	}
	if second.Created || second.Device.DeviceKey != first.Device.DeviceKey || second.Device.PublicHost != first.Device.PublicHost {
		t.Fatalf("idempotent registration failed: first=%+v second=%+v", first, second)
	}
	other, err := db.RegisterDesktopDevice(context.Background(), RegisterDesktopDeviceInput{DeviceID: "mac-lan", DeviceName: "Other", OwnerUserID: "user-2", PublicHost: "b.m.example.test"})
	if err != nil {
		t.Fatalf("register other owner: %v", err)
	}
	if other.Device.DeviceKey == first.Device.DeviceKey || other.Device.PublicHost == first.Device.PublicHost {
		t.Fatalf("owners shared device identity: first=%+v other=%+v", first, other)
	}
	resolved, err := db.GetDesktopDeviceByOwnerAndID(context.Background(), "user-1", "mac-lan")
	if err != nil || resolved.DeviceKey != first.Device.DeviceKey {
		t.Fatalf("owner lookup = %+v, %v", resolved, err)
	}
}

func TestDesktopWebAppRouteUsesDeviceJoinWithoutToken(t *testing.T) {
	db := openTestDB(t)
	device, err := db.RegisterDesktopDevice(context.Background(), RegisterDesktopDeviceInput{DeviceID: "mac-lan", OwnerUserID: "user-1", PublicHost: "a.m.example.test"})
	if err != nil {
		t.Fatalf("register device: %v", err)
	}
	created, err := db.RegisterDesktopWebApp(context.Background(), RegisterDesktopWebAppInput{OwnerUserID: "user-1", DeviceID: "mac-lan", Name: "notes", PublicHost: "abcdefghijk23-wa.example.test", TargetURL: "http://127.0.0.1:5173", Active: true})
	if err != nil {
		t.Fatalf("register webapp: %v", err)
	}
	if created.Route.TokenID != "" || !created.Route.Active {
		t.Fatalf("webapp route = %+v", created.Route)
	}
	joined, err := db.GetActiveDesktopWebAppRouteByHost(context.Background(), "ABCDEFGHIJK23-WA.EXAMPLE.TEST:443")
	if err != nil {
		t.Fatalf("join route: %v", err)
	}
	if joined.Device.DeviceKey != device.Device.DeviceKey || joined.Route.ID != created.Route.ID {
		t.Fatalf("joined route = %+v", joined)
	}
	byID, err := db.GetActiveDesktopWebAppRouteByID(t.Context(), created.WebApp.ID)
	if err != nil || byID.Device.OwnerUserID != "user-1" || byID.Route.ID != created.Route.ID {
		t.Fatal("binding lookup lost ownership", err)
	}
	owned, err := db.GetActiveOwnedDesktopWebAppRoute(t.Context(), "user-1", "mac-lan", "notes")
	if err != nil || owned.WebApp.ID != created.WebApp.ID {
		t.Fatal("publication read changed identity", err)
	}
	if _, err := db.GetActiveOwnedDesktopWebAppRoute(t.Context(), "another-user", "mac-lan", "notes"); err != ErrNotFound {
		t.Fatal("foreign publication was visible", err)
	}
	if _, err := db.sql.ExecContext(t.Context(), `DELETE FROM desktop_webapps WHERE id=?`, created.WebApp.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetActiveDesktopWebAppRouteByID(t.Context(), created.WebApp.ID); err != ErrNotFound {
		t.Fatal("removed publication remained readable", err)
	}
}

func TestDesktopSessionsAndTrafficUseDeviceIdentity(t *testing.T) {
	db := openTestDB(t)
	registered, err := db.RegisterDesktopDevice(context.Background(), RegisterDesktopDeviceInput{DeviceID: "mac-lan", OwnerUserID: "user-1", PublicHost: "a.m.example.test"})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	session, err := db.CreateDesktopSession(context.Background(), registered.Device, "127.0.0.1")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := db.RecordTrafficEvent(context.Background(), TrafficEvent{ObjectType: "desktop", DeviceID: registered.Device.DeviceKey, SessionID: session.ID, Kind: "websocket", BytesIn: 3, BytesOut: 5}); err != nil {
		t.Fatalf("record traffic: %v", err)
	}
	stats, err := db.TrafficStatsByDevice(context.Background())
	if err != nil || stats[registered.Device.DeviceKey].BytesOut != 5 {
		t.Fatalf("stats = %+v, %v", stats, err)
	}
}

func TestShareSchemaHasNoRetiredLifecycleFields(t *testing.T) {
	db := openTestDB(t)
	var query string
	if db.database == databaseSQLite {
		query = `SELECT COUNT(*) FROM pragma_table_info('conversation_shares') WHERE name IN ('expires_at','single_use')`
	} else {
		query = `SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='conversation_shares' AND column_name IN ('expires_at','single_use')`
	}
	var count int
	if err := db.sql.QueryRow(query).Scan(&count); err != nil || count != 0 {
		t.Fatal("retired share fields remain", count, err)
	}
	if db.database == databaseSQLite {
		query = `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='conversation_share_claims'`
	} else {
		query = `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name='conversation_share_claims'`
	}
	if err := db.sql.QueryRow(query).Scan(&count); err != nil || count != 0 {
		t.Fatal("claim table remains", count, err)
	}
}

func openTestDB(t *testing.T) *DB {
	t.Helper()
	if os.Getenv("TEST_DATABASE_TYPE") == "sqlite" {
		db, err := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "relay.sqlite"))
		return finishOpeningTestDB(t, db, err)
	}
	return openMySQLTestDB(t)
}

func openMySQLTestDB(t *testing.T) *DB {
	t.Helper()
	if os.Getenv("TEST_DATABASE_TYPE") == "sqlite" {
		t.Skip("MySQL-specific integration test")
	}
	db, err := Open(context.Background(), mysqltest.NewConfig(t))
	return finishOpeningTestDB(t, db, err)
}

func finishOpeningTestDB(t *testing.T, db *DB, err error) *DB {
	t.Helper()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}
