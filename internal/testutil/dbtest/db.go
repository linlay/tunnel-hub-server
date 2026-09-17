package dbtest

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"example.invalid/tunnel-hub-server/internal/store"
	"example.invalid/tunnel-hub-server/internal/testutil/mysqltest"
)

const TypeEnvironment = "TEST_DATABASE_TYPE"

func Open(t *testing.T) *store.DB {
	t.Helper()
	var (
		db  *store.DB
		err error
	)
	if os.Getenv(TypeEnvironment) == "sqlite" {
		db, err = store.OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "relay.sqlite"))
	} else {
		db, err = store.Open(context.Background(), mysqltest.NewConfig(t))
	}
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate database: %v", err)
	}
	return db
}
