package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode"

	"example.invalid/tunnel-hub-server/internal/sharecontract"
)

const ConversationSnapshotVersion = 1
const MaxConversationShareConversationIDBytes = 255

type ConversationShare struct {
	ID              string
	OwnerUserID     string
	ConversationID  string
	SnapshotVersion int
	SnapshotJSON    []byte
	Kind            string
	Title           string
	DescriptionJSON []byte
	CreatedAt       time.Time
	LastAccessedAt  *time.Time
}

type ConversationShareResource struct {
	ShareID  string
	ID       string
	Name     string
	MIMEType string
	Size     int64
	SHA256   string
	Path     string
}

var conversationShareResourceID = regexp.MustCompile(`^[a-f0-9]{24}$`)
var conversationShareResourceMIME = regexp.MustCompile(`^[A-Za-z0-9!#$&^_.+-]+/[A-Za-z0-9!#$&^_.+-]+$`)
var conversationShareResourceHash = regexp.MustCompile(`^[a-f0-9]{64}$`)
var conversationShareID = regexp.MustCompile(`^share_[A-Za-z0-9_-]{1,74}$`)

// The conversation entry remains a narrow compatibility adapter. The generic
// write below does not require a conversation ID or Snapshot V1.
func (db *DB) CreateConversationShare(ctx context.Context, ownerUserID, conversationID string,
	snapshotVersion int, snapshotJSON []byte, createdAt time.Time) (ConversationShare, error) {
	id, err := NewConversationShareID()
	if err != nil {
		return ConversationShare{}, err
	}
	return db.CreateConversationShareWithResources(ctx, id, ownerUserID, conversationID, snapshotVersion, snapshotJSON, nil, createdAt)
}

func (db *DB) CreateConversationShareWithResources(ctx context.Context, id, ownerUserID, conversationID string,
	snapshotVersion int, snapshotJSON []byte, resources []ConversationShareResource, createdAt time.Time) (ConversationShare, error) {
	conversationID = strings.TrimSpace(conversationID)
	if !ValidConversationShareConversationID(conversationID) || snapshotVersion != ConversationSnapshotVersion {
		return ConversationShare{}, errors.New("invalid conversation share snapshot identity")
	}
	var metadata struct {
		Title string `json:"title"`
	}
	_ = json.Unmarshal(snapshotJSON, &metadata)
	title := strings.TrimSpace(metadata.Title)
	if title == "" || len(title) > 255 || strings.ContainsFunc(title, unicode.IsControl) {
		title = "历史对话分享"
	}
	return db.CreateShareWithResources(ctx, ConversationShare{ID: id, OwnerUserID: ownerUserID, ConversationID: conversationID,
		Kind: "conversation", Title: title, SnapshotVersion: snapshotVersion, SnapshotJSON: snapshotJSON, CreatedAt: createdAt}, resources)
}

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
	seen := make(map[string]bool)
	paths := make(map[string]bool)
	for _, resource := range resources {
		if !conversationShareResourceID.MatchString(resource.ID) || seen[resource.ID] || resource.Name == "" ||
			len(resource.Name) > 255 || strings.ContainsAny(resource.Name, "/\\") || strings.ContainsFunc(resource.Name, unicode.IsControl) ||
			!conversationShareResourceMIME.MatchString(resource.MIMEType) || resource.MIMEType != strings.ToLower(resource.MIMEType) || len(resource.MIMEType) > 100 ||
			resource.Size < 0 || resource.Size > (1<<53)-1 || !conversationShareResourceHash.MatchString(resource.SHA256) {
			return ConversationShare{}, errors.New("invalid share resource")
		}
		if resource.Path != "" && (!sharecontract.ValidResourcePath(resource.Path) || paths[resource.Path]) {
			return ConversationShare{}, errors.New("invalid share resource path")
		}
		seen[resource.ID] = true
		if resource.Path != "" {
			paths[resource.Path] = true
		}
	}
	tx, err := db.beginWriteTx(ctx)
	if err != nil {
		return ConversationShare{}, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO conversation_shares
  (id,owner_user_id,conversation_id,kind,title,description_json,snapshot_version,snapshot_json,created_at)
  VALUES (?,?,?,?,?,?,?,?,?)`, share.ID, share.OwnerUserID, nullableString(share.ConversationID), share.Kind, share.Title,
		share.DescriptionJSON, share.SnapshotVersion, share.SnapshotJSON, share.CreatedAt)
	if err != nil {
		return ConversationShare{}, err
	}
	for _, resource := range resources {
		_, err = tx.ExecContext(ctx, `INSERT INTO conversation_share_resources
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

func (db *DB) ListShares(ctx context.Context, ownerUserID string) ([]ConversationShare, error) {
	ownerUserID = strings.TrimSpace(ownerUserID)
	if ownerUserID == "" {
		return nil, errors.New("owner user id is required")
	}
	// No snapshot or description BLOB is read for management lists.
	rows, err := db.sql.QueryContext(ctx, `SELECT s.id,s.owner_user_id,COALESCE(s.conversation_id,''),s.kind,s.title,
  s.snapshot_version,s.created_at,a.last_accessed_at FROM conversation_shares AS s
  LEFT JOIN conversation_share_access AS a ON a.share_id=s.id
  WHERE s.owner_user_id=? AND s.revoked_at IS NULL ORDER BY s.created_at DESC,s.id DESC`, ownerUserID)
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

func (db *DB) ListConversationShares(ctx context.Context, ownerUserID string) ([]ConversationShare, error) {
	shares, err := db.ListShares(ctx, ownerUserID)
	if err != nil {
		return nil, err
	}
	conversations := make([]ConversationShare, 0, len(shares))
	for _, share := range shares {
		if share.Kind == "conversation" && share.ConversationID != "" {
			conversations = append(conversations, share)
		}
	}
	return conversations, nil
}

func (db *DB) AccessPublicConversationShare(ctx context.Context, id string) (ConversationShare, error) {
	var share ConversationShare
	err := db.sql.QueryRowContext(ctx, `SELECT id,COALESCE(conversation_id,''),kind,title,description_json,snapshot_version,snapshot_json
  FROM conversation_shares WHERE id=? AND revoked_at IS NULL`, id).
		Scan(&share.ID, &share.ConversationID, &share.Kind, &share.Title, &share.DescriptionJSON, &share.SnapshotVersion, &share.SnapshotJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return ConversationShare{}, ErrNotFound
	}
	return share, err
}

func (db *DB) PeekPublicConversationShare(ctx context.Context, id string) (ConversationShare, error) {
	var share ConversationShare
	err := db.sql.QueryRowContext(ctx, `SELECT id,kind,title,snapshot_version FROM conversation_shares
  WHERE id=? AND revoked_at IS NULL`, id).Scan(&share.ID, &share.Kind, &share.Title, &share.SnapshotVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return ConversationShare{}, ErrNotFound
	}
	return share, err
}

// Description and resource metadata reads never load the frozen content BLOB.
func (db *DB) DescribePublicShare(ctx context.Context, id string) (ConversationShare, error) {
	var share ConversationShare
	err := db.sql.QueryRowContext(ctx, `SELECT id,owner_user_id,COALESCE(conversation_id,''),kind,title,description_json,snapshot_version
		FROM conversation_shares WHERE id=? AND revoked_at IS NULL`, id).
		Scan(&share.ID, &share.OwnerUserID, &share.ConversationID, &share.Kind, &share.Title, &share.DescriptionJSON, &share.SnapshotVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return ConversationShare{}, ErrNotFound
	}
	return share, err
}

func (db *DB) ListPublicShareResources(ctx context.Context, id string) ([]ConversationShareResource, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT r.share_id,r.resource_id,r.name,r.mime_type,r.size_bytes,r.sha256,COALESCE(r.logical_path,'')
		FROM conversation_share_resources AS r JOIN conversation_shares AS s ON s.id=r.share_id
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

func (db *DB) ReadPublicConversationShareResource(ctx context.Context, shareID, resourceID string) (ConversationShareResource, error) {
	var resource ConversationShareResource
	err := db.sql.QueryRowContext(ctx, `SELECT r.share_id,r.resource_id,r.name,r.mime_type,r.size_bytes,r.sha256,COALESCE(r.logical_path,'')
  FROM conversation_share_resources AS r JOIN conversation_shares AS s ON s.id=r.share_id
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
		FROM conversation_share_resources AS r JOIN conversation_shares AS s ON s.id=r.share_id
		WHERE r.share_id=? AND r.logical_path=? AND s.revoked_at IS NULL`, shareID, path).
		Scan(&resource.ShareID, &resource.ID, &resource.Name, &resource.MIMEType, &resource.Size, &resource.SHA256, &resource.Path)
	if errors.Is(err, sql.ErrNoRows) {
		return ConversationShareResource{}, ErrNotFound
	}
	return resource, err
}

func (db *DB) RevokeConversationShare(ctx context.Context, id, ownerUserID string, revokedAt time.Time) error {
	result, err := db.sql.ExecContext(ctx, `
		UPDATE conversation_shares
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

func (db *DB) RecordConversationShareAccess(ctx context.Context, id string, accessedAt time.Time) error {
	if db.database == databaseSQLite {
		_, err := db.sql.ExecContext(ctx, `
			INSERT INTO conversation_share_access (share_id, last_accessed_at)
			VALUES (?, ?)
			ON CONFLICT(share_id) DO UPDATE SET last_accessed_at = MAX(last_accessed_at, excluded.last_accessed_at)
		`, strings.TrimSpace(id), databaseTime(accessedAt))
		return err
	}
	_, err := db.sql.ExecContext(ctx, `
		INSERT INTO conversation_share_access (share_id, last_accessed_at)
		VALUES (?, ?)
		ON DUPLICATE KEY UPDATE last_accessed_at = GREATEST(last_accessed_at, ?)
	`, strings.TrimSpace(id), databaseTime(accessedAt), databaseTime(accessedAt))
	return err
}

func (db *DB) ConversationShareResourceCleanupIDs(ctx context.Context) ([]string, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT DISTINCT s.id FROM conversation_shares AS s
  JOIN conversation_share_resources AS r ON r.share_id=s.id WHERE s.revoked_at IS NOT NULL`)
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

func (db *DB) DeleteConversationShareResourceMetadata(ctx context.Context, shareID string) error {
	_, err := db.sql.ExecContext(ctx, `DELETE FROM conversation_share_resources WHERE share_id = ?`, strings.TrimSpace(shareID))
	return err
}

func (db *DB) ConversationShareResourceIDs(ctx context.Context) (map[string]struct{}, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT DISTINCT share_id FROM conversation_share_resources`)
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

func ValidConversationShareConversationID(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" &&
		len(value) <= MaxConversationShareConversationIDBytes &&
		!strings.ContainsFunc(value, unicode.IsControl)
}

func NewConversationShareID() (string, error) {
	var raw [18]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "share_" + base64.RawURLEncoding.EncodeToString(raw[:]), nil
}
