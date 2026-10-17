package store

import (
	"context"
	"errors"
	"strings"
	"time"
)

var ErrInvalidDeviceName = errors.New("deviceName must contain 1-64 characters")

func (db *DB) IsDesktopDeviceActive(ctx context.Context, deviceKey string) (bool, error) {
	var count int
	err := db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM desktop_devices WHERE device_id = ? AND revoked_at IS NULL`, deviceKey).Scan(&count)
	return count == 1, err
}

func (db *DB) RenameAccountDesktop(ctx context.Context, ownerID, deviceID, name string) error {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > 64 {
		return ErrInvalidDeviceName
	}
	result, err := db.sql.ExecContext(ctx, `UPDATE desktop_devices SET device_name = ?, updated_at = ? WHERE owner_user_id = ? AND display_device_id = ? AND revoked_at IS NULL`, name, databaseTime(time.Now()), ownerID, deviceID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

func (db *DB) RevokeAccountDesktop(ctx context.Context, ownerID, deviceID string) error {
	now := databaseTime(time.Now())
	result, err := db.sql.ExecContext(ctx, `UPDATE desktop_devices SET revoked_at = ?, updated_at = ? WHERE owner_user_id = ? AND display_device_id = ? AND revoked_at IS NULL`, now, now, ownerID, deviceID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return ErrNotFound
	}
	return nil
}
