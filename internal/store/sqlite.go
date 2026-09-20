package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

//go:embed schema_sqlite.sql
var sqliteSchema string

const sqliteSchemaVersion = 2

func OpenSQLite(ctx context.Context, path string) (*DB, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("SQLite database path is required")
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve SQLite database path: %w", err)
	}
	parent, err := os.Stat(filepath.Dir(absolutePath))
	if err != nil {
		return nil, fmt.Errorf("inspect SQLite database directory: %w", err)
	}
	if !parent.IsDir() {
		return nil, errors.New("SQLite database parent is not a directory")
	}

	databaseURL := &url.URL{Scheme: "file", Path: filepath.ToSlash(absolutePath)}
	query := url.Values{}
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "foreign_keys(1)")
	query.Add("_pragma", "journal_mode(WAL)")
	query.Set("_time_format", "sqlite")
	query.Set("_timezone", "UTC")
	query.Set("_txlock", "immediate")
	databaseURL.RawQuery = query.Encode()

	pool, err := sql.Open("sqlite", databaseURL.String())
	if err != nil {
		return nil, fmt.Errorf("open SQLite: %w", err)
	}
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	startup, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.PingContext(startup); err != nil {
		_ = pool.Close()
		return nil, fmt.Errorf("ping SQLite: %w", err)
	}
	return &DB{sql: pool, database: databaseSQLite}, nil
}

func (db *DB) migrateSQLite(ctx context.Context) error {
	tx, err := db.beginWriteTx(ctx)
	if err != nil {
		return fmt.Errorf("initialize SQLite schema: %w", err)
	}
	defer tx.Rollback()

	var version int
	if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read SQLite schema version: %w", err)
	}
	switch version {
	case 0:
		var tables int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`).Scan(&tables); err != nil {
			return fmt.Errorf("inspect SQLite schema: %w", err)
		}
		if tables != 0 {
			return errors.New("SQLite database has an unsupported unversioned schema; use a new database file")
		}
		for _, statement := range strings.Split(sqliteSchema, ";") {
			if strings.TrimSpace(statement) == "" {
				continue
			}
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("initialize SQLite schema: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, `PRAGMA user_version = 2`); err != nil {
			return fmt.Errorf("write SQLite schema version: %w", err)
		}
	case 1:
		if _, err := tx.ExecContext(ctx, `CREATE TABLE conversation_share_attachments (
			share_id TEXT NOT NULL, attachment_id TEXT NOT NULL, name TEXT NOT NULL,
			mime_type TEXT NOT NULL, size_bytes INTEGER NOT NULL, sha256 TEXT NOT NULL,
			body BLOB NOT NULL, PRIMARY KEY (share_id, attachment_id),
			FOREIGN KEY (share_id) REFERENCES conversation_shares(id) ON DELETE CASCADE
		)`); err != nil {
			return fmt.Errorf("migrate SQLite conversation share attachments: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `CREATE TABLE conversation_share_claims (
			share_id TEXT PRIMARY KEY, token_hash BLOB NOT NULL, expires_at TIMESTAMP NOT NULL,
			FOREIGN KEY (share_id) REFERENCES conversation_shares(id) ON DELETE CASCADE
		)`); err != nil {
			return fmt.Errorf("migrate SQLite conversation share claims: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `PRAGMA user_version = 2`); err != nil {
			return fmt.Errorf("write SQLite schema version: %w", err)
		}
		if err := verifySQLiteSchema(ctx, tx); err != nil {
			return err
		}
	case sqliteSchemaVersion:
		if err := verifySQLiteSchema(ctx, tx); err != nil {
			return err
		}
	default:
		return fmt.Errorf("SQLite schema version %d is not supported", version)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit SQLite schema: %w", err)
	}
	return nil
}

func verifySQLiteSchema(ctx context.Context, tx *sql.Tx) error {
	const expectedTables = 14
	var tables int
	err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name IN (
			'admin_users', 'admin_sessions', 'tunnel_tokens', 'routes',
			'desktop_devices', 'desktop_webapps', 'agent_sessions', 'desktop_sessions',
			'events', 'conversation_shares', 'conversation_share_access', 'traffic_events',
			'conversation_share_attachments', 'conversation_share_claims'
		)
	`).Scan(&tables)
	if err != nil {
		return fmt.Errorf("verify SQLite schema: %w", err)
	}
	if tables != expectedTables {
		return errors.New("SQLite schema is incomplete")
	}
	return nil
}

func isSQLiteDuplicateKey(err error) bool {
	var databaseError *sqlite.Error
	if !errors.As(err, &databaseError) {
		return false
	}
	return databaseError.Code() == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY || databaseError.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE
}

func scanSQLiteTrafficStatsWithKey(row rowScanner, key *string) (TrafficStats, error) {
	var stats TrafficStats
	var lastAt any
	if err := row.Scan(key, &stats.RequestCount, &stats.BytesIn, &stats.BytesOut, &lastAt); err != nil {
		return TrafficStats{}, err
	}
	parsed, ok, err := parseSQLiteNullableTime(lastAt)
	if err != nil {
		return TrafficStats{}, err
	}
	if ok {
		stats.LastAt = &parsed
	}
	return stats, nil
}

func scanSQLiteTrafficStatsNoKey(row rowScanner) (TrafficStats, error) {
	var stats TrafficStats
	var lastAt any
	if err := row.Scan(&stats.RequestCount, &stats.BytesIn, &stats.BytesOut, &lastAt); err != nil {
		return TrafficStats{}, err
	}
	parsed, ok, err := parseSQLiteNullableTime(lastAt)
	if err != nil {
		return TrafficStats{}, err
	}
	if ok {
		stats.LastAt = &parsed
	}
	return stats, nil
}

func parseSQLiteNullableTime(value any) (time.Time, bool, error) {
	switch typed := value.(type) {
	case nil:
		return time.Time{}, false, nil
	case time.Time:
		return typed.UTC(), true, nil
	case string:
		return parseSQLiteTimeString(typed)
	case []byte:
		return parseSQLiteTimeString(string(typed))
	default:
		return time.Time{}, false, fmt.Errorf("unsupported SQLite time value %T", value)
	}
}

func parseSQLiteTimeString(value string) (time.Time, bool, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false, nil
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
	} {
		parsed, err := time.Parse(layout, value)
		if err == nil {
			return parsed.UTC(), true, nil
		}
	}
	return time.Time{}, false, fmt.Errorf("parse SQLite time %q", value)
}
