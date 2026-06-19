package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/linlay/zenmind-tunnel-server/internal/auth"
)

func TestRouteCRUDAndHostNormalization(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	token := createTestToken(t, db, "laptop")

	route, err := db.CreateRoute(ctx, "App.Example.COM:443", "http://127.0.0.1:3000", true, token.ID)
	if err != nil {
		t.Fatalf("create route: %v", err)
	}
	if route.PublicHost != "app.example.com" {
		t.Fatalf("host was not normalized: %q", route.PublicHost)
	}
	if route.TokenID != token.ID {
		t.Fatalf("route token id = %q", route.TokenID)
	}

	found, err := db.GetActiveRouteByHost(ctx, "app.example.com")
	if err != nil {
		t.Fatalf("get route: %v", err)
	}
	if found.TargetURL != "http://127.0.0.1:3000" {
		t.Fatalf("unexpected target: %q", found.TargetURL)
	}

	updated, err := db.UpdateRoute(ctx, route.ID, "api.example.com", "http://127.0.0.1:8080", false, token.ID)
	if err != nil {
		t.Fatalf("update route: %v", err)
	}
	if updated.Active {
		t.Fatal("route should be inactive")
	}
	if _, err := db.GetActiveRouteByHost(ctx, "api.example.com"); err != ErrNotFound {
		t.Fatalf("inactive route should not match, got %v", err)
	}
}

func TestUnassignedRoutesAreNotActive(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if _, err := db.CreateRoute(ctx, "legacy.example.com", "http://127.0.0.1:3000", true, ""); err != nil {
		t.Fatalf("create legacy route: %v", err)
	}
	if _, err := db.GetRouteByHost(ctx, "legacy.example.com"); err != nil {
		t.Fatalf("legacy route should remain listable: %v", err)
	}
	if _, err := db.GetActiveRouteByHost(ctx, "legacy.example.com"); err != ErrNotFound {
		t.Fatalf("unassigned route should not be active, got %v", err)
	}
}

func TestTokenValidation(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	raw, err := auth.NewToken()
	if err != nil {
		t.Fatalf("new token: %v", err)
	}
	token, err := db.CreateToken(ctx, "laptop", raw)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}

	found, err := db.FindActiveTokenBySecret(ctx, raw)
	if err != nil {
		t.Fatalf("find token: %v", err)
	}
	if found.ID != token.ID {
		t.Fatalf("wrong token: %s", found.ID)
	}

	if err := db.DeactivateToken(ctx, token.ID); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	if _, err := db.FindActiveTokenBySecret(ctx, raw); err != ErrNotFound {
		t.Fatalf("inactive token should not validate, got %v", err)
	}
}

func TestRegisterDesktopDeviceOwnership(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	first, err := db.RegisterDesktopDevice(ctx, RegisterDesktopDeviceInput{
		DeviceID:    "mac-mini",
		OwnerUserID: "42",
		OwnerEmail:  "desktop.test",
		PublicHost:  "mac-mini.tunnel-hub.zenmind.cc",
		TargetURL:   "http://127.0.0.1:7082",
	})
	if err != nil {
		t.Fatalf("register desktop device: %v", err)
	}
	if !first.Created || first.Device.OwnerUserID != "42" || first.AgentToken == "" {
		t.Fatalf("unexpected first registration: %+v", first)
	}

	second, err := db.RegisterDesktopDevice(ctx, RegisterDesktopDeviceInput{
		DeviceID:    "mac-mini",
		OwnerUserID: "42",
		OwnerEmail:  "desktop.test",
		PublicHost:  "mac-mini.tunnel-hub.zenmind.cc",
		TargetURL:   "http://127.0.0.1:7083",
	})
	if err != nil {
		t.Fatalf("update desktop device: %v", err)
	}
	if second.Created || second.Token.ID != first.Token.ID || second.AgentToken != "" || second.Device.TargetURL != "http://127.0.0.1:7083" {
		t.Fatalf("unexpected second registration: %+v", second)
	}

	_, err = db.RegisterDesktopDevice(ctx, RegisterDesktopDeviceInput{
		DeviceID:    "mac-mini",
		OwnerUserID: "43",
		OwnerEmail:  "other.test",
		PublicHost:  "mac-mini.tunnel-hub.zenmind.cc",
		TargetURL:   "http://127.0.0.1:7999",
	})
	if !errors.Is(err, ErrDesktopDeviceOwnerMismatch) {
		t.Fatalf("different owner should be rejected, got %v", err)
	}
	route, err := db.GetRouteByHost(ctx, "mac-mini.tunnel-hub.zenmind.cc")
	if err != nil {
		t.Fatalf("get route: %v", err)
	}
	if route.TargetURL != "http://127.0.0.1:7083" {
		t.Fatalf("different owner changed route: %+v", route)
	}
}

func TestRegisterDesktopDeviceClaimsLegacyDevice(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	token := createTestToken(t, db, "desktop:legacy")
	route, err := db.CreateRoute(ctx, "legacy.tunnel-hub.zenmind.cc", "http://127.0.0.1:7082", true, token.ID)
	if err != nil {
		t.Fatalf("create route: %v", err)
	}
	now := time.Now().UTC()
	secretHash, err := auth.HashSecret("legacy-secret")
	if err != nil {
		t.Fatalf("hash legacy secret: %v", err)
	}
	if _, err := db.sql.ExecContext(ctx, `
		INSERT INTO desktop_devices (device_id, device_secret_hash, token_id, route_id, public_host, target_url, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, "legacy", secretHash, token.ID, route.ID, route.PublicHost, route.TargetURL, now, now); err != nil {
		t.Fatalf("insert legacy device: %v", err)
	}

	result, err := db.RegisterDesktopDevice(ctx, RegisterDesktopDeviceInput{
		DeviceID:    "legacy",
		OwnerUserID: "42",
		OwnerEmail:  "desktop.test",
		PublicHost:  "legacy.tunnel-hub.zenmind.cc",
		TargetURL:   "http://127.0.0.1:7083",
	})
	if err != nil {
		t.Fatalf("claim legacy device: %v", err)
	}
	if result.Device.OwnerUserID != "42" || result.Token.ID != token.ID || result.Device.TargetURL != "http://127.0.0.1:7083" {
		t.Fatalf("unexpected legacy claim: %+v", result)
	}

	_, err = db.RegisterDesktopDevice(ctx, RegisterDesktopDeviceInput{
		DeviceID:    "legacy",
		OwnerUserID: "43",
		OwnerEmail:  "other.test",
		PublicHost:  "legacy.tunnel-hub.zenmind.cc",
		TargetURL:   "http://127.0.0.1:7999",
	})
	if !errors.Is(err, ErrDesktopDeviceOwnerMismatch) {
		t.Fatalf("claimed legacy device should reject different owner, got %v", err)
	}
}

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func createTestToken(t *testing.T, db *DB, name string) TunnelToken {
	t.Helper()
	raw, err := auth.NewToken()
	if err != nil {
		t.Fatalf("new token: %v", err)
	}
	token, err := db.CreateToken(context.Background(), name, raw)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	return token
}
