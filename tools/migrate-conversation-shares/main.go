package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const snapshotVersion = 1

var scriptPattern = regexp.MustCompile(`(?is)<script\b[^>]*>`)
var snapshotIDPattern = regexp.MustCompile(`(?i)\bid\s*=\s*["']conversation-snapshot["']`)
var jsonTypePattern = regexp.MustCompile(`(?i)\btype\s*=\s*["']application/json["']`)

type legacyShare struct {
	id             string
	ownerUserID    string
	conversationID string
	html           []byte
	createdAt      time.Time
	expiresAt      sql.NullTime
	revokedAt      sql.NullTime
	singleUse      bool
}

func main() {
	databasePath := flag.String("db", "", "stopped Tunnel SQLite database path")
	backupPath := flag.String("backup", "", "new SQLite backup path")
	flag.Parse()
	if err := run(*databasePath, *backupPath); err != nil {
		fmt.Fprintln(os.Stderr, "conversation share migration:", err)
		os.Exit(1)
	}
}

func run(databasePath, backupPath string) error {
	databasePath = strings.TrimSpace(databasePath)
	backupPath = strings.TrimSpace(backupPath)
	if databasePath == "" || backupPath == "" {
		return errors.New("-db and -backup are required")
	}
	databasePath, err := filepath.Abs(databasePath)
	if err != nil {
		return err
	}
	backupPath, err = filepath.Abs(backupPath)
	if err != nil {
		return err
	}
	if databasePath == backupPath {
		return errors.New("backup path must differ from database path")
	}
	if _, err := os.Stat(backupPath); !errors.Is(err, os.ErrNotExist) {
		return errors.New("backup path already exists")
	}
	if err := os.MkdirAll(filepath.Dir(backupPath), 0o750); err != nil {
		return err
	}

	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA busy_timeout = 5000; PRAGMA wal_checkpoint(TRUNCATE);`); err != nil {
		return fmt.Errorf("checkpoint database: %w", err)
	}
	if _, err := db.Exec("VACUUM INTO " + sqliteString(backupPath)); err != nil {
		return fmt.Errorf("create backup: %w", err)
	}

	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := requireLegacySchema(ctx, conn); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF; BEGIN EXCLUSIVE;`); err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()

	if _, err := conn.ExecContext(ctx, newConversationSharesTableSQL); err != nil {
		return fmt.Errorf("create replacement table: %w", err)
	}
	rows, err := conn.QueryContext(ctx, `SELECT id FROM conversation_shares ORDER BY id`)
	if err != nil {
		return err
	}
	var shareIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		shareIDs = append(shareIDs, id)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}

	inserted := 0
	for _, id := range shareIDs {
		var share legacyShare
		if err := conn.QueryRowContext(ctx, `
			SELECT id, owner_user_id, conversation_id, html_document,
			       created_at, expires_at, revoked_at, single_use
			FROM conversation_shares
			WHERE id = ?
		`, id).Scan(
			&share.id, &share.ownerUserID, &share.conversationID, &share.html,
			&share.createdAt, &share.expiresAt, &share.revokedAt, &share.singleUse,
		); err != nil {
			return fmt.Errorf("read share %s: %w", id, err)
		}
		snapshot, err := extractSnapshot(share.html)
		if err != nil {
			return fmt.Errorf("share %s: %w", share.id, err)
		}
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO conversation_shares_snapshot (
				id, owner_user_id, conversation_id, snapshot_version, snapshot_json,
				created_at, expires_at, revoked_at, single_use
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, share.id, share.ownerUserID, share.conversationID, snapshotVersion, snapshot,
			share.createdAt, nullableTime(share.expiresAt), nullableTime(share.revokedAt), share.singleUse); err != nil {
			return fmt.Errorf("copy share %s: %w", share.id, err)
		}
		inserted++
	}
	sourceCount := len(shareIDs)
	if inserted != sourceCount {
		return fmt.Errorf("row count mismatch: source=%d migrated=%d", sourceCount, inserted)
	}
	if _, err := conn.ExecContext(ctx, `
		DROP TABLE conversation_shares;
		ALTER TABLE conversation_shares_snapshot RENAME TO conversation_shares;
		CREATE INDEX idx_conversation_shares_owner_conversation_created
			ON conversation_shares(owner_user_id, conversation_id, created_at DESC);
		COMMIT;
		PRAGMA foreign_keys = ON;
	`); err != nil {
		return fmt.Errorf("commit migration: %w", err)
	}
	committed = true
	var foreignKeyFailures int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&foreignKeyFailures); err != nil {
		return err
	}
	if foreignKeyFailures != 0 {
		return fmt.Errorf("foreign key check failed: %d rows", foreignKeyFailures)
	}
	fmt.Printf("Migrated %d conversation shares; backup: %s\n", inserted, backupPath)
	return nil
}

func extractSnapshot(html []byte) ([]byte, error) {
	matches := scriptPattern.FindAllIndex(html, -1)
	var opening []int
	for _, match := range matches {
		tag := html[match[0]:match[1]]
		if snapshotIDPattern.Match(tag) && jsonTypePattern.Match(tag) {
			if opening != nil {
				return nil, errors.New("multiple conversation snapshots")
			}
			opening = match
		}
	}
	if opening == nil {
		return nil, errors.New("conversation snapshot is missing")
	}
	remaining := html[opening[1]:]
	closing := bytes.Index(bytes.ToLower(remaining), []byte("</script>"))
	if closing < 0 {
		return nil, errors.New("conversation snapshot script is not closed")
	}
	snapshot := bytes.TrimSpace(remaining[:closing])
	if !json.Valid(snapshot) {
		return nil, errors.New("conversation snapshot is invalid JSON")
	}
	var envelope struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(snapshot, &envelope); err != nil || envelope.Version != snapshotVersion {
		return nil, errors.New("conversation snapshot version is unsupported")
	}
	return append([]byte(nil), snapshot...), nil
}

func requireLegacySchema(ctx context.Context, conn *sql.Conn) error {
	rows, err := conn.QueryContext(ctx, `PRAGMA table_info(conversation_shares)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return err
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, required := range []string{"document_version", "html_document", "single_use"} {
		if !columns[required] {
			return fmt.Errorf("legacy column %s is missing", required)
		}
	}
	if columns["snapshot_version"] || columns["snapshot_json"] {
		return errors.New("conversation shares are already migrated")
	}
	return nil
}

func nullableTime(value sql.NullTime) any {
	if !value.Valid {
		return nil
	}
	return value.Time
}

func sqliteString(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

const newConversationSharesTableSQL = `
CREATE TABLE conversation_shares_snapshot (
	id TEXT PRIMARY KEY,
	owner_user_id TEXT NOT NULL,
	conversation_id TEXT NOT NULL,
	snapshot_version INTEGER NOT NULL,
	snapshot_json BLOB NOT NULL,
	created_at TIMESTAMP NOT NULL,
	expires_at TIMESTAMP,
	revoked_at TIMESTAMP,
	single_use INTEGER NOT NULL DEFAULT 0 CHECK (
		single_use IN (0, 1)
		AND (single_use = 0 OR expires_at IS NULL)
	)
);`
