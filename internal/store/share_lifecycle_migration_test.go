package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"example.invalid/tunnel-hub-server/internal/sharefiles"
	"example.invalid/tunnel-hub-server/internal/testutil/mysqltest"
)

func legacySQLiteSchema(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("testdata/schema_sqlite_legacy.sql")
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func openLegacyShareDB(t *testing.T, version int) *DB {
	t.Helper()
	var db *DB
	var err error
	var legacy string
	if os.Getenv("TEST_DATABASE_TYPE") == "sqlite" {
		db, err = OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "legacy.sqlite"))
		legacy = legacySQLiteSchema(t)
		if version < 4 {
			legacy = strings.Replace(legacy, "\trevoked_at TIMESTAMP,\n\tUNIQUE(owner_user_id, display_device_id)", "\tUNIQUE(owner_user_id, display_device_id)", 1)
		}
		if version < 3 {
			a := strings.Index(legacy, "CREATE TABLE IF NOT EXISTS conversation_share_resources")
			b := strings.Index(legacy[a:], ";") + a + 1
			legacy = legacy[:a] + legacy[b:]
		}
		if version == 1 {
			a := strings.Index(legacy, "CREATE TABLE IF NOT EXISTS conversation_share_claims")
			b := strings.Index(legacy[a:], ";") + a + 1
			legacy = legacy[:a] + legacy[b:]
		}
	} else {
		db, err = Open(context.Background(), mysqltest.NewConfig(t))
		data, readErr := os.ReadFile("testdata/schema_mysql_legacy.sql")
		if readErr != nil {
			t.Fatal(readErr)
		}
		legacy = string(data)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, statement := range strings.Split(legacy, ";") {
		if strings.TrimSpace(statement) != "" {
			if _, err := db.sql.Exec(statement); err != nil {
				t.Fatal(err)
			}
		}
	}
	if db.database == databaseSQLite {
		if _, err := db.sql.Exec(fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
			t.Fatal(err)
		}
		if version == 2 {
			if _, err := db.sql.Exec(`CREATE TABLE conversation_share_attachments (
				share_id TEXT NOT NULL,attachment_id TEXT NOT NULL,name TEXT NOT NULL,mime_type TEXT NOT NULL,
				size_bytes INTEGER NOT NULL,sha256 TEXT NOT NULL,body BLOB NOT NULL,
				PRIMARY KEY(share_id,attachment_id),FOREIGN KEY(share_id) REFERENCES conversation_shares(id) ON DELETE CASCADE)`); err != nil {
				t.Fatal(err)
			}
		}
	}
	return db
}

func TestShareLifecycleMigrationPreservesPermanentAndInvalidatesLegacyPolicies(t *testing.T) {
	versions := []int{4}
	if os.Getenv("TEST_DATABASE_TYPE") == "sqlite" {
		versions = []int{1, 2, 3, 4}
	}
	for _, version := range versions {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			db := openLegacyShareDB(t, version)
			ctx := context.Background()
			now := databaseTime(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
			oldRevocation := now.Add(-time.Hour)
			snapshot := []byte("{ \"version\":1,\"title\":\"permanent\" }\n")
			for _, row := range []struct {
				id               string
				expires, revoked *time.Time
				once             int
			}{
				{"permanent", nil, nil, 0}, {"revoked-permanent", nil, &oldRevocation, 0},
				{"future", timePointer(now.Add(100 * 365 * 24 * time.Hour)), nil, 0},
				{"past", timePointer(now.Add(-time.Hour)), nil, 0},
				{"once-unclaimed", nil, nil, 1}, {"once-claimed", nil, nil, 1},
				{"already-revoked", timePointer(now.Add(time.Hour)), &oldRevocation, 0},
			} {
				if _, err := db.sql.Exec(`INSERT INTO conversation_shares(id,owner_user_id,conversation_id,snapshot_version,snapshot_json,created_at,expires_at,revoked_at,single_use)
					VALUES(?,?,?,1,?,?,?,?,?)`, row.id, "owner", "chat", snapshot, now, row.expires, row.revoked, row.once); err != nil {
					t.Fatal(err)
				}
				if _, err := db.sql.Exec(`INSERT INTO conversation_share_access(share_id,last_accessed_at) VALUES(?,?)`, row.id, now.Add(time.Minute)); err != nil {
					t.Fatal(err)
				}
			}
			if version != 1 {
				if _, err := db.sql.Exec(`INSERT INTO conversation_share_claims(share_id,token_hash,expires_at) VALUES(?,?,?)`, "once-claimed", make([]byte, 32), now.Add(time.Minute)); err != nil {
					t.Fatal(err)
				}
			}
			resourceDir := t.TempDir()
			body := []byte("permanent resource")
			digest := sha256.Sum256(body)
			const resourceID = "0123456789abcdef01234567"
			if version >= 3 || db.database == databaseMySQL {
				if _, err := db.sql.Exec(`INSERT INTO conversation_share_resources(share_id,resource_id,name,mime_type,size_bytes,sha256) VALUES('permanent',?,'report.txt','text/plain',?,?)`, resourceID, len(body), hex.EncodeToString(digest[:])); err != nil {
					t.Fatal(err)
				}
				files, err := sharefiles.New(resourceDir)
				if err != nil {
					t.Fatal(err)
				}
				stage, err := files.Begin()
				if err != nil {
					t.Fatal(err)
				}
				if _, err := stage.Write(resourceID, bytes.NewReader(body), int64(len(body))); err != nil {
					t.Fatal(err)
				}
				if err := stage.Commit("permanent"); err != nil {
					t.Fatal(err)
				}
			} else if version == 2 {
				if _, err := db.sql.Exec(`INSERT INTO conversation_share_attachments(share_id,attachment_id,name,mime_type,size_bytes,sha256,body) VALUES('permanent',?,'report.txt','text/plain',?,?,?)`, resourceID, len(body), hex.EncodeToString(digest[:]), body); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				if err := db.Migrate(ctx, resourceDir); err != nil {
					t.Fatal(err)
				}
			}
			for _, id := range []string{"future", "past", "once-unclaimed", "once-claimed", "already-revoked", "revoked-permanent"} {
				if _, err := db.AccessPublicConversationShare(ctx, id); !errors.Is(err, ErrNotFound) {
					t.Fatalf("%s reopened: %v", id, err)
				}
			}
			share, err := db.AccessPublicConversationShare(ctx, "permanent")
			if err != nil || !bytes.Equal(share.SnapshotJSON, snapshot) {
				t.Fatal("permanent bytes changed", err)
			}
			listed, err := db.ListShares(ctx, "owner")
			if err != nil || len(listed) != 1 || !listed[0].CreatedAt.Equal(now) || listed[0].LastAccessedAt == nil || !listed[0].LastAccessedAt.Equal(now.Add(time.Minute)) {
				t.Fatal("permanent metadata changed", err)
			}
			var actual time.Time
			if err := db.sql.QueryRow(`SELECT revoked_at FROM conversation_shares WHERE id='already-revoked'`).Scan(&actual); err != nil || !actual.Equal(oldRevocation) {
				t.Fatal("existing revocation replaced", err)
			}
			if version != 1 {
				file := filepath.Join(resourceDir, "shares", "permanent", "resources", resourceID)
				got, err := os.ReadFile(file)
				if err != nil || !bytes.Equal(got, body) {
					t.Fatal("resource bytes changed", err)
				}
				resource, err := db.ReadPublicConversationShareResource(ctx, "permanent", resourceID)
				if err != nil || resource.SHA256 != hex.EncodeToString(digest[:]) {
					t.Fatal("resource metadata changed", err)
				}
			}
			if db.database == databaseSQLite {
				var version, enabled int
				if err := db.sql.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != sqliteSchemaVersion {
					t.Fatal("schema version", err)
				}
				if err := db.sql.QueryRow(`PRAGMA foreign_keys`).Scan(&enabled); err != nil || enabled != 1 {
					t.Fatal("FK enforcement not restored", err)
				}
			}
		})
	}
}

func TestSQLiteShareMigrationRollbackAndRetry(t *testing.T) {
	// SQLite-specific failure injection also runs in the MySQL suite.
	t.Setenv("TEST_DATABASE_TYPE", "sqlite")
	db := openLegacyShareDB(t, 4)
	if _, err := db.sql.Exec(`INSERT INTO conversation_shares(id,owner_user_id,conversation_id,snapshot_version,snapshot_json,created_at,single_use) VALUES('once','owner','chat',1,'{}',CURRENT_TIMESTAMP,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`CREATE TABLE conversation_shares_v5(id TEXT)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(context.Background()); err == nil {
		t.Fatal("injected collision did not fail migration")
	}
	var revoked *time.Time
	if err := db.sql.QueryRow(`SELECT revoked_at FROM conversation_shares WHERE id='once'`).Scan(&revoked); err != nil || revoked != nil {
		t.Fatal("failed transaction partially committed", err)
	}
	var enabled int
	if err := db.sql.QueryRow(`PRAGMA foreign_keys`).Scan(&enabled); err != nil || enabled != 1 {
		t.Fatal("FK setting leaked", err)
	}
	if _, err := db.sql.Exec(`DROP TABLE conversation_shares_v5`); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AccessPublicConversationShare(context.Background(), "once"); !errors.Is(err, ErrNotFound) {
		t.Fatal("retry reopened once share", err)
	}
}

func TestMySQLShareMigrationResumesAfterPartialDDL(t *testing.T) {
	requireMySQLIntegration(t)
	db := openLegacyShareDB(t, 4)
	ctx := context.Background()
	now := databaseTime(time.Now())
	for _, row := range []struct {
		id     string
		once   int
		expiry *time.Time
	}{
		{"once", 1, nil}, {"future", 0, timePointer(now.Add(time.Hour))}, {"permanent", 0, nil},
	} {
		if _, err := db.sql.Exec(`INSERT INTO conversation_shares(id,owner_user_id,conversation_id,snapshot_version,snapshot_json,created_at,single_use,expires_at) VALUES(?,?,?,1,'{}',?,?,?)`, row.id, "owner", "chat", now, row.once, row.expiry); err != nil {
			t.Fatal(err)
		}
	}
	// Model interruption after revocation committed and only one retired column
	// was dropped. A rerun must never infer that the old once record is permanent.
	for _, statement := range []string{
		`UPDATE conversation_shares SET revoked_at=created_at WHERE expires_at IS NOT NULL OR single_use<>0`,
		`DROP TABLE conversation_share_claims`,
		`ALTER TABLE conversation_shares DROP CHECK chk_conversation_share_single_use`,
		`ALTER TABLE conversation_shares DROP COLUMN single_use`,
	} {
		if _, err := db.sql.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err := db.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"once", "future"} {
		if _, err := db.AccessPublicConversationShare(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Fatal("partial migration reopened share", id, err)
		}
	}
	if _, err := db.AccessPublicConversationShare(ctx, "permanent"); err != nil {
		t.Fatal(err)
	}
	var legacyColumns int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='conversation_shares' AND column_name IN ('expires_at','single_use')`).Scan(&legacyColumns); err != nil || legacyColumns != 0 {
		t.Fatal("retired fields remain", err)
	}
}
