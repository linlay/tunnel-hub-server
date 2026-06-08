package store

import (
	"context"
	"testing"

	"github.com/linlay/zenmind-tunnel-server/internal/auth"
)

func TestRouteCRUDAndHostNormalization(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	route, err := db.CreateRoute(ctx, "App.Example.COM:443", "http://127.0.0.1:3000", true)
	if err != nil {
		t.Fatalf("create route: %v", err)
	}
	if route.PublicHost != "app.example.com" {
		t.Fatalf("host was not normalized: %q", route.PublicHost)
	}

	found, err := db.GetActiveRouteByHost(ctx, "app.example.com")
	if err != nil {
		t.Fatalf("get route: %v", err)
	}
	if found.TargetURL != "http://127.0.0.1:3000" {
		t.Fatalf("unexpected target: %q", found.TargetURL)
	}

	updated, err := db.UpdateRoute(ctx, route.ID, "api.example.com", "http://127.0.0.1:8080", false)
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
