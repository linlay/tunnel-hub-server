package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"example.invalid/tunnel-hub-server/internal/sharefiles"
)

type legacyConversationShareResource struct {
	ShareID, ID, Name, MIMEType, SHA256 string
	Size                                int64
	Body                                []byte
}

func (db *DB) migrateLegacyConversationShareAttachments(ctx context.Context, resourceDir string) error {
	exists, err := db.legacyConversationShareAttachmentsExist(ctx)
	if err != nil || !exists {
		return err
	}
	rows, err := db.sql.QueryContext(ctx, `SELECT share_id, attachment_id, name, mime_type, size_bytes, sha256, body
		FROM conversation_share_attachments ORDER BY share_id, attachment_id`)
	if err != nil {
		return fmt.Errorf("read legacy conversation share attachments: %w", err)
	}
	var resources []legacyConversationShareResource
	for rows.Next() {
		var resource legacyConversationShareResource
		if err := rows.Scan(&resource.ShareID, &resource.ID, &resource.Name, &resource.MIMEType, &resource.Size, &resource.SHA256, &resource.Body); err != nil {
			rows.Close()
			return err
		}
		resources = append(resources, resource)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(resources) > 0 && strings.TrimSpace(resourceDir) == "" {
		return errors.New("CONVERSATION_SHARE_RESOURCE_DIR is required to migrate legacy conversation share attachments")
	}
	var files *sharefiles.Store
	if len(resources) > 0 {
		files, err = sharefiles.New(resourceDir)
		if err != nil {
			return err
		}
		byShare := make(map[string][]legacyConversationShareResource)
		shareBytes := make(map[string]int64)
		for _, resource := range resources {
			digest := sha256.Sum256(resource.Body)
			actualHash := hex.EncodeToString(digest[:])
			if resource.Size != int64(len(resource.Body)) || !strings.EqualFold(resource.SHA256, actualHash) {
				return fmt.Errorf("legacy conversation share resource %s/%s failed integrity validation", resource.ShareID, resource.ID)
			}
			shareBytes[resource.ShareID] += resource.Size
			if shareBytes[resource.ShareID] > MaxConversationResourceBytes {
				return fmt.Errorf("legacy conversation share %s resources exceed size limit", resource.ShareID)
			}
			byShare[resource.ShareID] = append(byShare[resource.ShareID], resource)
		}
		shareIDs := make([]string, 0, len(byShare))
		for shareID := range byShare {
			shareIDs = append(shareIDs, shareID)
		}
		sort.Strings(shareIDs)
		for _, shareID := range shareIDs {
			complete := true
			for _, resource := range byShare[shareID] {
				if files.Verify(shareID, resource.ID, resource.Size, resource.SHA256) != nil {
					complete = false
					break
				}
			}
			if complete {
				continue
			}
			if err := files.Delete(shareID); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			stage, err := files.Begin()
			if err != nil {
				return err
			}
			committed := false
			defer func() {
				if !committed {
					_ = stage.Abort()
				}
			}()
			for _, resource := range byShare[shareID] {
				written, err := stage.Write(resource.ID, bytes.NewReader(resource.Body), MaxConversationResourceBytes)
				if err != nil || written.Size != resource.Size || !strings.EqualFold(written.SHA256, resource.SHA256) {
					return fmt.Errorf("write legacy conversation share resource %s/%s", shareID, resource.ID)
				}
			}
			if err := stage.Commit(shareID); err != nil {
				return err
			}
			committed = true
		}
	}

	tx, err := db.beginWriteTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	insert := `INSERT IGNORE INTO conversation_share_resources
		(share_id, resource_id, name, mime_type, size_bytes, sha256) VALUES (?, ?, ?, ?, ?, ?)`
	if db.database == databaseSQLite {
		insert = `INSERT OR IGNORE INTO conversation_share_resources
			(share_id, resource_id, name, mime_type, size_bytes, sha256) VALUES (?, ?, ?, ?, ?, ?)`
	}
	for _, resource := range resources {
		if _, err := tx.ExecContext(ctx, insert, resource.ShareID, resource.ID, resource.Name,
			resource.MIMEType, resource.Size, strings.ToLower(resource.SHA256)); err != nil {
			return err
		}
	}
	if db.database == databaseSQLite {
		if _, err := tx.ExecContext(ctx, `DROP TABLE conversation_share_attachments`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `PRAGMA user_version = 3`); err != nil {
			return err
		}
		return tx.Commit()
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	_, err = db.sql.ExecContext(ctx, `DROP TABLE conversation_share_attachments`)
	return err
}

func (db *DB) legacyConversationShareAttachmentsExist(ctx context.Context) (bool, error) {
	var count int
	query := `SELECT COUNT(*) FROM information_schema.tables
		WHERE table_schema = DATABASE() AND table_name = 'conversation_share_attachments'`
	if db.database == databaseSQLite {
		query = `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'conversation_share_attachments'`
	}
	if err := db.sql.QueryRowContext(ctx, query).Scan(&count); err != nil {
		return false, err
	}
	return count == 1, nil
}
