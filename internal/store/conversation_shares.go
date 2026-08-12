package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"strings"
	"time"
)

type ConversationShare struct {
	ID           string     `json:"id"`
	OwnerUserID  string     `json:"-"`
	Title        string     `json:"title"`
	SnapshotJSON []byte     `json:"-"`
	CreatedAt    time.Time  `json:"createdAt"`
	RevokedAt    *time.Time `json:"revokedAt,omitempty"`
}

func (db *DB) CreateConversationShare(ctx context.Context, ownerUserID, title string, snapshotJSON []byte) (ConversationShare, error) {
	ownerUserID = strings.TrimSpace(ownerUserID)
	title = strings.TrimSpace(title)
	if ownerUserID == "" {
		return ConversationShare{}, errors.New("owner user id is required")
	}
	if title == "" {
		return ConversationShare{}, errors.New("title is required")
	}
	if len(snapshotJSON) == 0 {
		return ConversationShare{}, errors.New("snapshot is required")
	}
	id, err := newConversationShareID()
	if err != nil {
		return ConversationShare{}, err
	}
	share := ConversationShare{
		ID:           id,
		OwnerUserID:  ownerUserID,
		Title:        title,
		SnapshotJSON: append([]byte(nil), snapshotJSON...),
		CreatedAt:    time.Now().UTC(),
	}
	_, err = db.sql.ExecContext(ctx, `
		INSERT INTO conversation_shares (id, owner_user_id, title, snapshot_json, created_at)
		VALUES (?, ?, ?, ?, ?)
	`, share.ID, share.OwnerUserID, share.Title, share.SnapshotJSON, share.CreatedAt)
	return share, err
}

func (db *DB) GetPublicConversationShare(ctx context.Context, id string) (ConversationShare, error) {
	row := db.sql.QueryRowContext(ctx, `
		SELECT id, owner_user_id, title, snapshot_json, created_at, revoked_at
		FROM conversation_shares
		WHERE id = ? AND revoked_at IS NULL
	`, strings.TrimSpace(id))
	return scanConversationShare(row)
}

func (db *DB) RevokeConversationShare(ctx context.Context, id, ownerUserID string) error {
	result, err := db.sql.ExecContext(ctx, `
		UPDATE conversation_shares
		SET revoked_at = ?
		WHERE id = ? AND owner_user_id = ? AND revoked_at IS NULL
	`, time.Now().UTC(), strings.TrimSpace(id), strings.TrimSpace(ownerUserID))
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

func scanConversationShare(row rowScanner) (ConversationShare, error) {
	var share ConversationShare
	var revokedAt sql.NullTime
	if err := row.Scan(&share.ID, &share.OwnerUserID, &share.Title, &share.SnapshotJSON, &share.CreatedAt, &revokedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ConversationShare{}, ErrNotFound
		}
		return ConversationShare{}, err
	}
	if revokedAt.Valid {
		share.RevokedAt = &revokedAt.Time
	}
	return share, nil
}

func newConversationShareID() (string, error) {
	var raw [18]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "share_" + base64.RawURLEncoding.EncodeToString(raw[:]), nil
}
