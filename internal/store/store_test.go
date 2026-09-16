package store

import (
	"context"
	"example.invalid/tunnel-hub-server/internal/testutil/mysqltest"
	"testing"
	"time"
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

func TestConversationShareSchemaRejectsSingleUseWithExpiration(t *testing.T) {
	assertConversationShareSingleUseExpirationConstraint(t, openTestDB(t), "share_invalid_fresh_once")
}

func assertConversationShareSingleUseExpirationConstraint(t *testing.T, db *DB, id string) {
	t.Helper()
	now := time.Date(2026, time.August, 17, 1, 2, 3, 0, time.UTC)
	if _, err := db.sql.Exec(`
		INSERT INTO conversation_shares (
			id, owner_user_id, conversation_id, snapshot_version,
			snapshot_json, created_at, expires_at, single_use
		) VALUES (?, 'owner-a', 'chat-a', 1, '{"version":1}', ?, ?, 1)
	`, id, now, now.Add(24*time.Hour)); err == nil {
		t.Fatal("database accepted a single-use share with an expiration")
	}
}

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(context.Background(), mysqltest.NewConfig(t))
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
