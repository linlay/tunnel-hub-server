package store

import (
	"context"
	"database/sql"
)

// Migrate creates only the current schema. Existing tables are never rewritten.
func (db *DB) Migrate(ctx context.Context) error {
	if db.database == databaseSQLite {
		return db.migrateSQLite(ctx)
	}
	return db.migrateMySQL(ctx)
}

func (db *DB) beginWriteTx(ctx context.Context) (*sql.Tx, error) {
	if db.database == databaseSQLite {
		return db.sql.BeginTx(ctx, nil)
	}
	return db.sql.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
}

func (db *DB) forUpdateClause() string {
	if db.database == databaseSQLite {
		return ""
	}
	return " FOR UPDATE"
}

func (db *DB) castID(column string) string {
	if db.database == databaseSQLite {
		return "CAST(" + column + " AS TEXT)"
	}
	return "CAST(" + column + " AS CHAR)"
}

func (db *DB) trafficSearchClause() string {
	if db.database == databaseSQLite {
		return "(public_host LIKE ? OR route_id LIKE ? OR token_id LIKE ? OR device_id LIKE ? OR session_id LIKE ? OR kind LIKE ? OR method LIKE ? OR path LIKE ? OR error LIKE ?)"
	}
	return "(public_host COLLATE utf8mb4_0900_as_ci LIKE ? OR route_id COLLATE utf8mb4_0900_as_ci LIKE ? OR token_id COLLATE utf8mb4_0900_as_ci LIKE ? OR device_id COLLATE utf8mb4_0900_as_ci LIKE ? OR session_id COLLATE utf8mb4_0900_as_ci LIKE ? OR kind COLLATE utf8mb4_0900_as_ci LIKE ? OR method COLLATE utf8mb4_0900_as_ci LIKE ? OR path COLLATE utf8mb4_0900_as_ci LIKE ? OR error COLLATE utf8mb4_0900_as_ci LIKE ?)"
}

func (db *DB) isDuplicateKey(err error) bool {
	if db.database == databaseSQLite {
		return isSQLiteDuplicateKey(err)
	}
	return isMySQLDuplicateKey(err)
}

func (db *DB) desktopHostError(err error) error {
	if db.isDuplicateKey(err) {
		return ErrDesktopDeviceHostConflict
	}
	return err
}
