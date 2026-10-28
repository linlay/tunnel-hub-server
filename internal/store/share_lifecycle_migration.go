package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// MySQL DDL commits independently. Persist revocation while all remaining legacy
// discriminators still exist, before dropping any of them. Every step is rerunnable.
func (db *DB) migrateMySQLShareLifecycle(ctx context.Context) error {
	var predicates []string
	for _, column := range []string{"expires_at", "single_use"} {
		exists, err := db.mysqlShareColumnExists(ctx, "conversation_shares", column)
		if err != nil {
			return err
		}
		if exists {
			predicate := "expires_at IS NOT NULL"
			if column == "single_use" {
				predicate = "single_use <> 0"
			}
			predicates = append(predicates, predicate)
		}
	}
	if len(predicates) > 0 {
		if _, err := db.sql.ExecContext(ctx, `UPDATE conversation_shares SET revoked_at = ?
			WHERE revoked_at IS NULL AND (`+strings.Join(predicates, " OR ")+")", databaseTime(time.Now())); err != nil {
			return fmt.Errorf("revoke legacy non-permanent shares: %w", err)
		}
	}
	if _, err := db.sql.ExecContext(ctx, `DROP TABLE IF EXISTS conversation_share_claims`); err != nil {
		return err
	}
	var checks int
	if err := db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.table_constraints
		WHERE constraint_schema = DATABASE() AND table_name = 'conversation_shares'
		AND constraint_name = 'chk_conversation_share_single_use' AND constraint_type = 'CHECK'`).Scan(&checks); err != nil {
		return err
	}
	if checks > 0 {
		if _, err := db.sql.ExecContext(ctx, `ALTER TABLE conversation_shares DROP CHECK chk_conversation_share_single_use`); err != nil {
			return err
		}
	}
	for _, column := range []string{"single_use", "expires_at"} {
		exists, err := db.mysqlShareColumnExists(ctx, "conversation_shares", column)
		if err != nil {
			return err
		}
		if exists {
			if _, err := db.sql.ExecContext(ctx, "ALTER TABLE conversation_shares DROP COLUMN "+column); err != nil {
				return err
			}
		}
	}
	for _, addition := range []struct{ table, column, definition string }{
		{"conversation_shares", "kind", "VARCHAR(128) NOT NULL DEFAULT 'conversation'"},
		{"conversation_shares", "title", "VARCHAR(255) NOT NULL DEFAULT '历史对话分享'"},
		{"conversation_shares", "description_json", "LONGBLOB NULL"},
		{"conversation_share_resources", "logical_path", "VARCHAR(255) NULL"},
	} {
		exists, err := db.mysqlShareColumnExists(ctx, addition.table, addition.column)
		if err != nil {
			return err
		}
		if !exists {
			if _, err := db.sql.ExecContext(ctx, "ALTER TABLE "+addition.table+" ADD COLUMN "+addition.column+" "+addition.definition); err != nil {
				return err
			}
		}
	}
	var requiredConversationID int
	if err := db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.columns
		WHERE table_schema=DATABASE() AND table_name='conversation_shares' AND column_name='conversation_id' AND is_nullable='NO'`).Scan(&requiredConversationID); err != nil {
		return err
	}
	if requiredConversationID > 0 {
		if _, err := db.sql.ExecContext(ctx, `ALTER TABLE conversation_shares MODIFY conversation_id VARCHAR(255) NULL`); err != nil {
			return err
		}
	}
	var indexes int
	if err := db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.statistics
		WHERE table_schema = DATABASE() AND table_name = 'conversation_share_resources'
		AND index_name = 'idx_share_resource_path'`).Scan(&indexes); err != nil {
		return err
	}
	if indexes == 0 {
		_, err := db.sql.ExecContext(ctx, `CREATE UNIQUE INDEX idx_share_resource_path ON conversation_share_resources(share_id, logical_path)`)
		return err
	}
	return nil
}

func (db *DB) mysqlShareColumnExists(ctx context.Context, table, column string) (bool, error) {
	var count int
	err := db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.columns
		WHERE table_schema = DATABASE() AND table_name = ? AND column_name = ?`, table, column).Scan(&count)
	return count == 1, err
}

// The caller disables foreign keys before beginning the transaction and checks
// them before commit. This rebuild preserves all child rows and exact BLOB bytes.
func migrateSQLiteShareLifecycle(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `UPDATE conversation_shares SET revoked_at = ?
		WHERE revoked_at IS NULL AND (expires_at IS NOT NULL OR single_use <> 0)`, databaseTime(time.Now())); err != nil {
		return err
	}
	for _, statement := range []string{
		`DROP TABLE IF EXISTS conversation_share_claims`,
		`CREATE TABLE conversation_shares_v5 (
			id TEXT PRIMARY KEY, owner_user_id TEXT NOT NULL, conversation_id TEXT,
			kind TEXT NOT NULL DEFAULT 'conversation', title TEXT NOT NULL DEFAULT '历史对话分享',
			description_json BLOB, snapshot_version INTEGER NOT NULL, snapshot_json BLOB NOT NULL,
			created_at TIMESTAMP NOT NULL, revoked_at TIMESTAMP)`,
		`INSERT INTO conversation_shares_v5 (id,owner_user_id,conversation_id,snapshot_version,snapshot_json,created_at,revoked_at)
			SELECT id,owner_user_id,conversation_id,snapshot_version,snapshot_json,created_at,revoked_at FROM conversation_shares`,
		`DROP TABLE conversation_shares`,
		`ALTER TABLE conversation_shares_v5 RENAME TO conversation_shares`,
		`CREATE INDEX idx_conversation_shares_owner_created ON conversation_shares(owner_user_id,created_at DESC,id DESC)`,
		`ALTER TABLE conversation_share_resources ADD COLUMN logical_path TEXT`,
		`CREATE UNIQUE INDEX idx_share_resource_path ON conversation_share_resources(share_id,logical_path)`,
		`PRAGMA user_version = 5`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("migrate permanent share schema: %w", err)
		}
	}
	return nil
}
