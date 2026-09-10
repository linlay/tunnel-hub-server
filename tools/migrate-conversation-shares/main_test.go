package main

import (
	"bytes"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestExtractSnapshot(t *testing.T) {
	html := []byte(`<!doctype html><script id="conversation-snapshot" type="application/json">
{"version":1,"title":"safe \\u003c/script\\u003e"}
</script><script src="runtime.js"></script>`)
	snapshot, err := extractSnapshot(html)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte(`{"version":1,"title":"safe \\u003c/script\\u003e"}`)
	if !bytes.Equal(snapshot, want) {
		t.Fatalf("snapshot=%q want=%q", snapshot, want)
	}
}

func TestExtractSnapshotRejectsMissingDuplicateAndInvalid(t *testing.T) {
	valid := `<script id="conversation-snapshot" type="application/json">{"version":1}</script>`
	for _, html := range []string{
		`<p>missing</p>`,
		valid + valid,
		`<script id="conversation-snapshot" type="application/json">broken</script>`,
		`<script id="conversation-snapshot" type="application/json">{"version":2}</script>`,
	} {
		if _, err := extractSnapshot([]byte(html)); err == nil {
			t.Fatalf("accepted %q", html)
		}
	}
}

func TestRunMigratesAllRowsAndCreatesLegacyBackup(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "relay.db")
	backupPath := filepath.Join(t.TempDir(), "relay.backup.db")
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	createdAt := time.Date(2026, 9, 10, 1, 2, 3, 0, time.UTC)
	if _, err := db.Exec(`
		CREATE TABLE conversation_shares (
			id TEXT PRIMARY KEY, owner_user_id TEXT NOT NULL, conversation_id TEXT NOT NULL,
			document_version INTEGER NOT NULL, html_document BLOB NOT NULL,
			created_at TIMESTAMP NOT NULL, expires_at TIMESTAMP, revoked_at TIMESTAMP,
			single_use INTEGER NOT NULL DEFAULT 0
		);
		CREATE TABLE conversation_share_access (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			share_id TEXT NOT NULL REFERENCES conversation_shares(id) ON DELETE CASCADE,
			accessed_at TIMESTAMP NOT NULL
		);
	`); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		id       string
		snapshot string
	}{
		{"share_a", `{"version":1,"title":"A"}`},
		{"share_b", `{"version":1,"title":"B"}`},
	} {
		html := `<html><script id="conversation-snapshot" type="application/json">` + row.snapshot + `</script></html>`
		if _, err := db.Exec(`
			INSERT INTO conversation_shares (
				id, owner_user_id, conversation_id, document_version, html_document,
				created_at, expires_at, revoked_at, single_use
			) VALUES (?, 'user_1', 'conversation_1', 1, ?, ?, NULL, NULL, 0)
		`, row.id, []byte(html), createdAt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO conversation_share_access (share_id, accessed_at) VALUES ('share_a', ?)`, createdAt); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if err := run(databasePath, backupPath); err != nil {
		t.Fatal(err)
	}

	assertColumns(t, databasePath, []string{"snapshot_version", "snapshot_json"}, []string{"document_version", "html_document"})
	assertColumns(t, backupPath, []string{"document_version", "html_document"}, []string{"snapshot_version", "snapshot_json"})
	migrated, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	var shareCount, accessCount int
	if err := migrated.QueryRow(`SELECT COUNT(*) FROM conversation_shares`).Scan(&shareCount); err != nil {
		t.Fatal(err)
	}
	if err := migrated.QueryRow(`SELECT COUNT(*) FROM conversation_share_access`).Scan(&accessCount); err != nil {
		t.Fatal(err)
	}
	if shareCount != 2 || accessCount != 1 {
		t.Fatalf("shares=%d accesses=%d", shareCount, accessCount)
	}
	var snapshotVersion int
	var snapshot []byte
	if err := migrated.QueryRow(`SELECT snapshot_version, snapshot_json FROM conversation_shares WHERE id = 'share_a'`).Scan(&snapshotVersion, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshotVersion != 1 || string(snapshot) != `{"version":1,"title":"A"}` {
		t.Fatalf("version=%d snapshot=%s", snapshotVersion, snapshot)
	}
}

func TestRunRollsBackEveryRowWhenOneSnapshotIsInvalid(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "relay.db")
	backupPath := filepath.Join(t.TempDir(), "relay.backup.db")
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		CREATE TABLE conversation_shares (
			id TEXT PRIMARY KEY, owner_user_id TEXT NOT NULL, conversation_id TEXT NOT NULL,
			document_version INTEGER NOT NULL, html_document BLOB NOT NULL,
			created_at TIMESTAMP NOT NULL, expires_at TIMESTAMP, revoked_at TIMESTAMP,
			single_use INTEGER NOT NULL DEFAULT 0
		);
	`); err != nil {
		t.Fatal(err)
	}
	createdAt := time.Date(2026, 9, 10, 1, 2, 3, 0, time.UTC)
	for _, row := range []struct {
		id   string
		html string
	}{
		{"share_valid", `<script id="conversation-snapshot" type="application/json">{"version":1}</script>`},
		{"share_invalid", `<script id="conversation-snapshot" type="application/json">{"version":2}</script>`},
	} {
		if _, err := db.Exec(`
			INSERT INTO conversation_shares (
				id, owner_user_id, conversation_id, document_version, html_document,
				created_at, expires_at, revoked_at, single_use
			) VALUES (?, 'user_1', 'conversation_1', 1, ?, ?, NULL, NULL, 0)
		`, row.id, []byte(row.html), createdAt); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if err := run(databasePath, backupPath); err == nil {
		t.Fatal("migration unexpectedly succeeded")
	}
	assertColumns(t, databasePath, []string{"document_version", "html_document"}, []string{"snapshot_version", "snapshot_json"})
	assertColumns(t, backupPath, []string{"document_version", "html_document"}, []string{"snapshot_version", "snapshot_json"})
	current, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	var count int
	if err := current.QueryRow(`SELECT COUNT(*) FROM conversation_shares`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("shares=%d want=2", count)
	}
}

func assertColumns(t *testing.T, databasePath string, present, absent []string) {
	t.Helper()
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`PRAGMA table_info(conversation_shares)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		columns[name] = true
	}
	for _, name := range present {
		if !columns[name] {
			t.Errorf("column %s is missing from %s", name, databasePath)
		}
	}
	for _, name := range absent {
		if columns[name] {
			t.Errorf("column %s remains in %s", name, databasePath)
		}
	}
}
