package store

import (
	"context"
	"database/sql"
	"fmt"
)

// Migrate creates the current schema and performs the one-way legacy share-resource migration.
func (db *DB) Migrate(ctx context.Context, conversationShareResourceDir ...string) error {
	resourceDir := ""
	if len(conversationShareResourceDir) > 0 {
		resourceDir = conversationShareResourceDir[0]
	}
	if db.database == databaseSQLite {
		return db.migrateSQLite(ctx, resourceDir)
	}
	return db.migrateMySQL(ctx, resourceDir)
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

// Validate required columns as well as CREATE IF NOT EXISTS, which cannot repair
// a database previously modified by the retired destructive migration.
func (db *DB) verifyShareTables(ctx context.Context) error {
	for _, query := range []string{
		`SELECT id,owner_user_id,conversation_id,snapshot_version,snapshot_json,created_at,expires_at,revoked_at,single_use FROM conversation_shares WHERE 1=0`,
		`SELECT share_id,token_hash,expires_at FROM conversation_share_claims WHERE 1=0`,
		`SELECT id,owner_user_id,conversation_id,kind,title,description_json,snapshot_version,snapshot_json,created_at,revoked_at FROM shares WHERE 1=0`,
		`SELECT share_id,resource_id,name,mime_type,size_bytes,sha256,logical_path FROM share_resources WHERE 1=0`,
		`SELECT share_id,last_accessed_at FROM share_access WHERE 1=0`,
	} {
		rows, err := db.sql.QueryContext(ctx, query)
		if err != nil {
			return fmt.Errorf("incompatible share schema; restore a matching database and resource backup before switching versions: %w", err)
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	return nil
}
