package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSQLiteV3CopyMigrationPreservesDevicesSharesAndResources(t *testing.T) {
	ctx := context.Background()
	old, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "original.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { old.Close() })
	legacy := strings.Replace(sqliteSchema, "\trevoked_at TIMESTAMP,\n\tUNIQUE(owner_user_id, display_device_id)", "\tUNIQUE(owner_user_id, display_device_id)", 1)
	if legacy == sqliteSchema {
		t.Fatal("legacy Desktop schema was not constructed")
	}
	for _, statement := range strings.Split(legacy, ";") {
		if strings.TrimSpace(statement) != "" {
			if _, err := old.sql.ExecContext(ctx, statement); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := old.sql.Exec(`PRAGMA user_version = 3`); err != nil {
		t.Fatal(err)
	}
	if _, err := old.sql.Exec(`INSERT INTO desktop_devices(device_id, display_device_id, device_name, owner_user_id, public_host, created_at, updated_at) VALUES('key', 'desktop-a', 'My Desktop', '42', 'unchanged.m.example.test', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	snapshot := []byte(`{"schemaVersion":1,"title":"existing share"}`)
	if _, err := old.sql.Exec(`INSERT INTO conversation_shares(id, owner_user_id, conversation_id, snapshot_version, snapshot_json, created_at) VALUES('existing-share','42','conversation',1,?,CURRENT_TIMESTAMP)`, snapshot); err != nil {
		t.Fatal(err)
	}
	body := []byte("existing attachment")
	digest := sha256.Sum256(body)
	file := filepath.Join(t.TempDir(), "attachment")
	if err := os.WriteFile(file, body, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := old.sql.Exec(`INSERT INTO conversation_share_resources(share_id,resource_id,name,mime_type,size_bytes,sha256) VALUES('existing-share','resource','file','text/plain',?,?)`, len(body), hex.EncodeToString(digest[:])); err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(t.TempDir(), "copy.sqlite")
	if _, err := old.sql.Exec(`VACUUM INTO ?`, copyPath); err != nil {
		t.Fatal(err)
	}
	copyDB, err := OpenSQLite(ctx, copyPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { copyDB.Close() })
	for range 2 {
		if err := copyDB.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
	}
	device, err := copyDB.GetDesktopDeviceByOwnerAndID(ctx, "42", "desktop-a")
	if err != nil || device.DeviceKey != "key" || device.PublicHost != "unchanged.m.example.test" || device.RevokedAt != nil {
		t.Fatalf("device changed: %+v %v", device, err)
	}
	var actual []byte
	if err := copyDB.sql.QueryRow(`SELECT snapshot_json FROM conversation_shares WHERE id='existing-share'`).Scan(&actual); err != nil || !bytes.Equal(actual, snapshot) {
		t.Fatalf("snapshot changed: %s %v", actual, err)
	}
	var resourceHash string
	var size int
	if err := copyDB.sql.QueryRow(`SELECT sha256,size_bytes FROM conversation_share_resources WHERE share_id='existing-share'`).Scan(&resourceHash, &size); err != nil || resourceHash != hex.EncodeToString(digest[:]) || size != len(body) {
		t.Fatal("resource metadata changed", err)
	}
	if actual, err := os.ReadFile(file); err != nil || !bytes.Equal(actual, body) {
		t.Fatal("attachment changed", err)
	}
	var version int
	if err := copyDB.sql.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 4 {
		t.Fatal("copy migration version", version, err)
	}
	if err := old.sql.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 3 {
		t.Fatal("original database was modified", version, err)
	}
	if err := copyDB.RevokeAccountDesktop(ctx, "other-account", "desktop-a"); err != ErrNotFound {
		t.Fatal("cross-account revoke accepted", err)
	}
	if err := copyDB.RevokeAccountDesktop(ctx, "42", "desktop-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := copyDB.GetDesktopDeviceByOwnerAndID(ctx, "42", "desktop-a"); err != ErrNotFound {
		t.Fatal("revoked device remains accessible", err)
	}
	if _, err := copyDB.RegisterDesktopDevice(ctx, RegisterDesktopDeviceInput{DeviceID: "desktop-a", OwnerUserID: "42", PublicHost: "unchanged.m.example.test"}); err != ErrDesktopDeviceRevoked {
		t.Fatal("registration restored revoked device", err)
	}
	if err := copyDB.sql.QueryRow(`SELECT snapshot_json FROM conversation_shares WHERE id='existing-share'`).Scan(&actual); err != nil || !bytes.Equal(actual, snapshot) {
		t.Fatal("revocation changed existing share", err)
	}
}
