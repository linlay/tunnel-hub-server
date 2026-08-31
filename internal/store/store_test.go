package store

import (
	"context"
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

func TestMigrateDesktopIdentityPreservesAgentAndDesktopData(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Now().UTC()
	legacySchema := `
		CREATE TABLE tunnel_tokens (id TEXT PRIMARY KEY, name TEXT NOT NULL, token_hash TEXT NOT NULL, token_prefix TEXT NOT NULL, active BOOLEAN NOT NULL, created_at TIMESTAMP NOT NULL, last_used_at TIMESTAMP);
		CREATE TABLE routes (id TEXT PRIMARY KEY, public_host TEXT NOT NULL UNIQUE, target_url TEXT NOT NULL, token_id TEXT, active BOOLEAN NOT NULL, created_at TIMESTAMP NOT NULL, updated_at TIMESTAMP NOT NULL);
		CREATE TABLE desktop_devices (device_id TEXT PRIMARY KEY, display_device_id TEXT, device_name TEXT, owner_user_id TEXT, owner_email TEXT, owner_name TEXT, device_secret_hash TEXT, token_id TEXT, route_id TEXT, public_host TEXT NOT NULL UNIQUE, target_url TEXT, created_at TIMESTAMP NOT NULL, updated_at TIMESTAMP NOT NULL);
		CREATE TABLE desktop_webapps (id TEXT PRIMARY KEY, device_id TEXT NOT NULL, name TEXT NOT NULL, route_id TEXT NOT NULL, public_host TEXT NOT NULL UNIQUE, target_url TEXT NOT NULL, active BOOLEAN NOT NULL, created_at TIMESTAMP NOT NULL, updated_at TIMESTAMP NOT NULL);
		CREATE TABLE agent_sessions (id TEXT PRIMARY KEY, token_id TEXT NOT NULL, remote_addr TEXT NOT NULL, connected_at TIMESTAMP NOT NULL, disconnected_at TIMESTAMP);
		CREATE TABLE traffic_events (id INTEGER PRIMARY KEY AUTOINCREMENT, object_type TEXT NOT NULL, public_host TEXT NOT NULL DEFAULT '', route_id TEXT, token_id TEXT, session_id TEXT, kind TEXT NOT NULL, method TEXT NOT NULL DEFAULT '', path TEXT NOT NULL DEFAULT '', status_code INTEGER NOT NULL DEFAULT 0, bytes_in INTEGER NOT NULL DEFAULT 0, bytes_out INTEGER NOT NULL DEFAULT 0, error TEXT NOT NULL DEFAULT '', occurred_at TIMESTAMP NOT NULL);
	`
	if _, err := db.sql.Exec(legacySchema); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO tunnel_tokens VALUES (?, ?, ?, ?, 1, ?, NULL)`, []any{"desktop-token", "desktop", "hash", "desk", now}},
		{`INSERT INTO tunnel_tokens VALUES (?, ?, ?, ?, 1, ?, NULL)`, []any{"agent-token", "agent", "hash", "agent", now}},
		{`INSERT INTO routes VALUES (?, ?, ?, ?, 1, ?, ?)`, []any{"webapp-route", "abcdefghijk23-wa.example.test", "http://127.0.0.1:5173", "desktop-token", now, now}},
		{`INSERT INTO routes VALUES (?, ?, ?, ?, 1, ?, ?)`, []any{"legacy-route", "legacy.example.test", "http://127.0.0.1:3000", "desktop-token", now, now}},
		{`INSERT INTO routes VALUES (?, ?, ?, ?, 1, ?, ?)`, []any{"agent-route", "agent.example.test", "http://127.0.0.1:8080", "agent-token", now, now}},
		{`INSERT INTO desktop_devices VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, []any{"device-key", "mac-lan", "Mac LAN", "user-1", "one@example.test", "Owner", "secret", "desktop-token", "legacy-route", "desk.m.example.test", "http://127.0.0.1:7082", now, now}},
		{`INSERT INTO desktop_webapps VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)`, []any{"webapp", "device-key", "notes", "webapp-route", "abcdefghijk23-wa.example.test", "http://127.0.0.1:5173", now, now}},
		{`INSERT INTO agent_sessions VALUES (?, ?, ?, ?, NULL)`, []any{"desktop-session", "desktop-token", "127.0.0.1", now}},
		{`INSERT INTO agent_sessions VALUES (?, ?, ?, ?, NULL)`, []any{"agent-session", "agent-token", "127.0.0.2", now}},
		{`INSERT INTO traffic_events (object_type, public_host, token_id, session_id, kind, bytes_in, bytes_out, occurred_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, []any{"desktop", "desk.m.example.test", "desktop-token", "desktop-session", "websocket", 3, 5, now}},
	} {
		if _, err := db.sql.Exec(statement.query, statement.args...); err != nil {
			t.Fatalf("seed legacy data: %v", err)
		}
	}
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("idempotent migrate: %v", err)
	}
	if hasTokenColumn, err := db.tableHasColumn(context.Background(), "desktop_devices", "token_id"); err != nil || hasTokenColumn {
		t.Fatalf("desktop token column remains: has=%v err=%v", hasTokenColumn, err)
	}
	tokens, err := db.ListTokens(context.Background())
	if err != nil || len(tokens) != 1 || tokens[0].ID != "agent-token" {
		t.Fatalf("tokens = %+v, %v", tokens, err)
	}
	desktopSessions, err := db.ListDesktopSessions(context.Background(), 10)
	if err != nil || len(desktopSessions) != 1 || desktopSessions[0].ID != "desktop-session" {
		t.Fatalf("desktop sessions = %+v, %v", desktopSessions, err)
	}
	agentSessions, err := db.ListAgentSessions(context.Background(), 10)
	if err != nil || len(agentSessions) != 1 || agentSessions[0].ID != "agent-session" {
		t.Fatalf("agent sessions = %+v, %v", agentSessions, err)
	}
	webappRoute, err := db.GetActiveDesktopWebAppRouteByHost(context.Background(), "abcdefghijk23-wa.example.test")
	if err != nil || webappRoute.Route.TokenID != "" || !webappRoute.Route.Active {
		t.Fatalf("webapp route = %+v, %v", webappRoute, err)
	}
	legacyRoute, err := db.GetRouteByHost(context.Background(), "legacy.example.test")
	if err != nil || legacyRoute.Active || legacyRoute.TokenID != "" {
		t.Fatalf("legacy route = %+v, %v", legacyRoute, err)
	}
	events, err := db.ListTrafficEvents(context.Background(), 10, "desktop", "")
	if err != nil || len(events) != 1 || events[0].DeviceID != "device-key" || events[0].TokenID != "" {
		t.Fatalf("traffic = %+v, %v", events, err)
	}
}

func TestMigrateConversationSharesDefaultsExistingRowsToReusable(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Date(2026, time.August, 17, 1, 2, 3, 0, time.UTC)
	if _, err := db.sql.Exec(`
		CREATE TABLE conversation_shares (
			id TEXT PRIMARY KEY,
			owner_user_id TEXT NOT NULL,
			conversation_id TEXT NOT NULL,
			document_version INTEGER NOT NULL,
			html_document BLOB NOT NULL,
			created_at TIMESTAMP NOT NULL,
			expires_at TIMESTAMP,
			revoked_at TIMESTAMP
		);
		INSERT INTO conversation_shares (
			id, owner_user_id, conversation_id, document_version,
			html_document, created_at, expires_at
		) VALUES ('share_legacy', 'owner-a', 'chat-a', 1, '<p>legacy</p>', ?, ?);
	`, now, now.Add(24*time.Hour)); err != nil {
		t.Fatalf("seed legacy conversation share: %v", err)
	}
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("idempotent migrate: %v", err)
	}
	assertConversationShareSingleUseExpirationConstraint(t, db, "share_invalid_migrated_once")
	share, err := db.AcquirePublicConversationShare(context.Background(), "share_legacy", now)
	if err != nil || share.SingleUse || string(share.HTMLDocument) != "<p>legacy</p>" {
		t.Fatalf("legacy share=%#v err=%v", share, err)
	}
	if _, err := db.AcquirePublicConversationShare(context.Background(), "share_legacy", now); err != nil {
		t.Fatalf("legacy share must remain reusable: %v", err)
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
			id, owner_user_id, conversation_id, document_version,
			html_document, created_at, expires_at, single_use
		) VALUES (?, 'owner-a', 'chat-a', 1, '<p>invalid</p>', ?, ?, 1)
	`, id, now, now.Add(24*time.Hour)); err == nil {
		t.Fatal("database accepted a single-use share with an expiration")
	}
}

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}
