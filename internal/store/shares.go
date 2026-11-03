package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"

	"example.invalid/tunnel-hub-server/internal/sharecontract"
)

func (db *DB) CreateShareWithResources(ctx context.Context, share ConversationShare, resources []ConversationShareResource) (ConversationShare, error) {
	share.OwnerUserID = strings.TrimSpace(share.OwnerUserID)
	share.CreatedAt = databaseTime(share.CreatedAt)
	if err := ValidateTextLength("ownerUserId", share.OwnerUserID, 255); err != nil {
		return ConversationShare{}, err
	}
	if share.OwnerUserID == "" || !conversationShareID.MatchString(share.ID) || share.Kind == "" || len(share.Kind) > 128 ||
		share.Title == "" || len(share.Title) > 255 || share.SnapshotVersion < 1 || !json.Valid(share.SnapshotJSON) ||
		(len(share.DescriptionJSON) > 0 && !json.Valid(share.DescriptionJSON)) {
		return ConversationShare{}, errors.New("invalid share metadata")
	}
	if err := validateShareResources(resources); err != nil {
		return ConversationShare{}, err
	}
	tx, err := db.beginWriteTx(ctx)
	if err != nil {
		return ConversationShare{}, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO shares
  (id,owner_user_id,conversation_id,kind,title,description_json,snapshot_version,snapshot_json,created_at)
  VALUES (?,?,?,?,?,?,?,?,?)`, share.ID, share.OwnerUserID, nullableString(share.ConversationID), share.Kind, share.Title,
		share.DescriptionJSON, share.SnapshotVersion, share.SnapshotJSON, share.CreatedAt)
	if err != nil {
		return ConversationShare{}, err
	}
	for _, resource := range resources {
		_, err = tx.ExecContext(ctx, `INSERT INTO share_resources
   (share_id,resource_id,name,mime_type,size_bytes,sha256,logical_path) VALUES (?,?,?,?,?,?,?)`,
			share.ID, resource.ID, resource.Name, resource.MIMEType, resource.Size, resource.SHA256, nullableString(resource.Path))
		if err != nil {
			return ConversationShare{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return ConversationShare{}, err
	}
	return share, nil
}

func (db *DB) ListShares(ctx context.Context, ownerUserID string, now time.Time) ([]ConversationShare, error) {
	ownerUserID = strings.TrimSpace(ownerUserID)
	if ownerUserID == "" {
		return nil, errors.New("owner user id is required")
	}
	// No snapshot or description BLOB is read for management lists.
	rows, err := db.sql.QueryContext(ctx, `SELECT s.id,s.owner_user_id,COALESCE(s.conversation_id,''),s.kind,s.title,
  s.snapshot_version,s.created_at,a.last_accessed_at FROM shares AS s
  LEFT JOIN share_access AS a ON a.share_id=s.id
  WHERE s.owner_user_id=? AND s.revoked_at IS NULL
  UNION ALL
  SELECT s.id,s.owner_user_id,s.conversation_id,'conversation','历史对话分享',
  s.snapshot_version,s.created_at,a.last_accessed_at FROM conversation_shares AS s
  LEFT JOIN conversation_share_access AS a ON a.share_id=s.id
  LEFT JOIN conversation_share_claims AS c ON c.share_id=s.id
  WHERE s.owner_user_id=? AND s.revoked_at IS NULL AND (s.expires_at IS NULL OR s.expires_at>?)
  AND (s.single_use=0 OR c.expires_at IS NULL OR c.expires_at>?)
  ORDER BY created_at DESC,id DESC`, ownerUserID, ownerUserID, databaseTime(now), databaseTime(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	shares := make([]ConversationShare, 0)
	for rows.Next() {
		var share ConversationShare
		if err := rows.Scan(&share.ID, &share.OwnerUserID, &share.ConversationID, &share.Kind, &share.Title,
			&share.SnapshotVersion, &share.CreatedAt, &share.LastAccessedAt); err != nil {
			return nil, err
		}
		shares = append(shares, share)
	}
	return shares, rows.Err()
}

func (db *DB) AccessPublicShare(ctx context.Context, id string) (ConversationShare, error) {
	var share ConversationShare
	err := db.sql.QueryRowContext(ctx, `SELECT id,COALESCE(conversation_id,''),kind,title,description_json,snapshot_version,snapshot_json
  FROM shares WHERE id=? AND revoked_at IS NULL`, id).
		Scan(&share.ID, &share.ConversationID, &share.Kind, &share.Title, &share.DescriptionJSON, &share.SnapshotVersion, &share.SnapshotJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return ConversationShare{}, ErrNotFound
	}
	return share, err
}

// Description and resource metadata reads never load the frozen content BLOB.
func (db *DB) DescribePublicShare(ctx context.Context, id string) (ConversationShare, error) {
	var share ConversationShare
	err := db.sql.QueryRowContext(ctx, `SELECT id,owner_user_id,COALESCE(conversation_id,''),kind,title,description_json,snapshot_version
		FROM shares WHERE id=? AND revoked_at IS NULL`, id).
		Scan(&share.ID, &share.OwnerUserID, &share.ConversationID, &share.Kind, &share.Title, &share.DescriptionJSON, &share.SnapshotVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return ConversationShare{}, ErrNotFound
	}
	return share, err
}

func (db *DB) ListPublicShareResources(ctx context.Context, id string) ([]ConversationShareResource, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT r.share_id,r.resource_id,r.name,r.mime_type,r.size_bytes,r.sha256,COALESCE(r.logical_path,'')
		FROM share_resources AS r JOIN shares AS s ON s.id=r.share_id
		WHERE r.share_id=? AND s.revoked_at IS NULL ORDER BY r.resource_id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	resources := make([]ConversationShareResource, 0)
	for rows.Next() {
		var resource ConversationShareResource
		if err := rows.Scan(&resource.ShareID, &resource.ID, &resource.Name, &resource.MIMEType, &resource.Size, &resource.SHA256, &resource.Path); err != nil {
			return nil, err
		}
		resources = append(resources, resource)
	}
	return resources, rows.Err()
}

func (db *DB) ReadPublicShareResource(ctx context.Context, shareID, resourceID string) (ConversationShareResource, error) {
	var resource ConversationShareResource
	err := db.sql.QueryRowContext(ctx, `SELECT r.share_id,r.resource_id,r.name,r.mime_type,r.size_bytes,r.sha256,COALESCE(r.logical_path,'')
  FROM share_resources AS r JOIN shares AS s ON s.id=r.share_id
  WHERE r.share_id=? AND r.resource_id=? AND s.revoked_at IS NULL`, shareID, resourceID).
		Scan(&resource.ShareID, &resource.ID, &resource.Name, &resource.MIMEType, &resource.Size, &resource.SHA256, &resource.Path)
	if errors.Is(err, sql.ErrNoRows) {
		return ConversationShareResource{}, ErrNotFound
	}
	return resource, err
}

func (db *DB) ReadPublicShareResourceByPath(ctx context.Context, shareID, path string) (ConversationShareResource, error) {
	var resource ConversationShareResource
	err := db.sql.QueryRowContext(ctx, `SELECT r.share_id,r.resource_id,r.name,r.mime_type,r.size_bytes,r.sha256,r.logical_path
		FROM share_resources AS r JOIN shares AS s ON s.id=r.share_id
		WHERE r.share_id=? AND r.logical_path=? AND s.revoked_at IS NULL`, shareID, path).
		Scan(&resource.ShareID, &resource.ID, &resource.Name, &resource.MIMEType, &resource.Size, &resource.SHA256, &resource.Path)
	if errors.Is(err, sql.ErrNoRows) {
		return ConversationShareResource{}, ErrNotFound
	}
	return resource, err
}

func (db *DB) RevokeShare(ctx context.Context, id, ownerUserID string, revokedAt time.Time) error {
	result, err := db.sql.ExecContext(ctx, `
		UPDATE shares
		SET revoked_at = ?
		WHERE id = ? AND owner_user_id = ? AND revoked_at IS NULL
	`, databaseTime(revokedAt), strings.TrimSpace(id), strings.TrimSpace(ownerUserID))
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

func (db *DB) RecordShareAccess(ctx context.Context, id string, accessedAt time.Time) error {
	if db.database == databaseSQLite {
		_, err := db.sql.ExecContext(ctx, `
			INSERT INTO share_access (share_id, last_accessed_at)
			VALUES (?, ?)
			ON CONFLICT(share_id) DO UPDATE SET last_accessed_at = MAX(last_accessed_at, excluded.last_accessed_at)
		`, strings.TrimSpace(id), databaseTime(accessedAt))
		return err
	}
	_, err := db.sql.ExecContext(ctx, `
		INSERT INTO share_access (share_id, last_accessed_at)
		VALUES (?, ?)
		ON DUPLICATE KEY UPDATE last_accessed_at = GREATEST(last_accessed_at, ?)
	`, strings.TrimSpace(id), databaseTime(accessedAt), databaseTime(accessedAt))
	return err
}

func (db *DB) ShareResourceCleanupIDs(ctx context.Context) ([]string, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT DISTINCT s.id FROM shares AS s
  JOIN share_resources AS r ON r.share_id=s.id WHERE s.revoked_at IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (db *DB) DeleteShareResourceMetadata(ctx context.Context, shareID string) error {
	_, err := db.sql.ExecContext(ctx, `DELETE FROM share_resources WHERE share_id = ?`, strings.TrimSpace(shareID))
	return err
}

func (db *DB) ShareResourceIDs(ctx context.Context) (map[string]struct{}, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT DISTINCT share_id FROM share_resources`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make(map[string]struct{})
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = struct{}{}
	}
	return ids, rows.Err()
}

func validateShareResources(resources []ConversationShareResource) error {
	seen := make(map[string]bool)
	paths := make(map[string]bool)
	for _, resource := range resources {
		if !conversationShareResourceID.MatchString(resource.ID) || seen[resource.ID] || resource.Name == "" ||
			len(resource.Name) > 255 || strings.ContainsAny(resource.Name, "/\\") || strings.ContainsFunc(resource.Name, unicode.IsControl) ||
			!conversationShareResourceMIME.MatchString(resource.MIMEType) || resource.MIMEType != strings.ToLower(resource.MIMEType) || len(resource.MIMEType) > 100 ||
			resource.Size < 0 || resource.Size > (1<<53)-1 || !conversationShareResourceHash.MatchString(resource.SHA256) {
			return errors.New("invalid share resource")
		}
		if resource.Path != "" && (!sharecontract.ValidResourcePath(resource.Path) || paths[resource.Path]) {
			return errors.New("invalid share resource path")
		}
		seen[resource.ID] = true
		if resource.Path != "" {
			paths[resource.Path] = true
		}
	}
	return nil
}
