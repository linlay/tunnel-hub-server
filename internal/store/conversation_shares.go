package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"strings"
	"time"
	"unicode"
)

const ConversationSnapshotVersion = 1
const MaxConversationSnapshotBytes = 20 << 20
const MaxConversationShareConversationIDBytes = 255

type ConversationShare struct {
	ID              string
	OwnerUserID     string
	ConversationID  string
	SnapshotVersion int
	SnapshotJSON    []byte
	CreatedAt       time.Time
	ExpiresAt       *time.Time
	LastAccessedAt  *time.Time
	SingleUse       bool
}

func (db *DB) CreateConversationShare(
	ctx context.Context,
	ownerUserID string,
	conversationID string,
	snapshotVersion int,
	snapshotJSON []byte,
	createdAt time.Time,
	expiresAt *time.Time,
	singleUse bool,
) (ConversationShare, error) {
	ownerUserID = strings.TrimSpace(ownerUserID)
	conversationID = strings.TrimSpace(conversationID)
	createdAt = databaseTime(createdAt)
	if expiresAt != nil {
		normalized := databaseTime(*expiresAt)
		expiresAt = &normalized
	}
	if err := ValidateTextLength("ownerUserId", ownerUserID, 255); err != nil {
		return ConversationShare{}, err
	}
	if ownerUserID == "" {
		return ConversationShare{}, errors.New("owner user id is required")
	}
	if !ValidConversationShareConversationID(conversationID) {
		return ConversationShare{}, errors.New("invalid conversation id")
	}
	if snapshotVersion != ConversationSnapshotVersion {
		return ConversationShare{}, errors.New("unsupported conversation snapshot version")
	}
	if len(snapshotJSON) > MaxConversationSnapshotBytes {
		return ConversationShare{}, errors.New("conversation snapshot is too large")
	}
	if len(snapshotJSON) == 0 {
		return ConversationShare{}, errors.New("conversation snapshot is required")
	}
	if expiresAt != nil && !expiresAt.After(createdAt) {
		return ConversationShare{}, errors.New("expiration must be after creation")
	}
	if singleUse && expiresAt != nil {
		return ConversationShare{}, errors.New("single-use share cannot have an expiration")
	}
	id, err := newConversationShareID()
	if err != nil {
		return ConversationShare{}, err
	}
	share := ConversationShare{
		ID:              id,
		OwnerUserID:     ownerUserID,
		ConversationID:  conversationID,
		SnapshotVersion: snapshotVersion,
		SnapshotJSON:    snapshotJSON,
		CreatedAt:       createdAt,
		ExpiresAt:       expiresAt,
		SingleUse:       singleUse,
	}
	_, err = db.sql.ExecContext(ctx, `
		INSERT INTO conversation_shares (
			id, owner_user_id, conversation_id, snapshot_version, snapshot_json,
			created_at, expires_at, single_use
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, share.ID, share.OwnerUserID, share.ConversationID, share.SnapshotVersion, share.SnapshotJSON, share.CreatedAt, share.ExpiresAt, share.SingleUse)
	return share, err
}

func (db *DB) ListConversationShares(
	ctx context.Context,
	ownerUserID string,
	now time.Time,
) ([]ConversationShare, error) {
	ownerUserID = strings.TrimSpace(ownerUserID)
	if ownerUserID == "" {
		return nil, errors.New("owner user id is required")
	}
	rows, err := db.sql.QueryContext(ctx, `
		SELECT shares.id, shares.owner_user_id, shares.conversation_id,
		       shares.snapshot_version, shares.created_at, shares.expires_at,
		       access.last_accessed_at, shares.single_use
		FROM conversation_shares AS shares
		LEFT JOIN conversation_share_access AS access ON access.share_id = shares.id
		WHERE shares.owner_user_id = ?
		  AND shares.snapshot_version = ?
		  AND shares.revoked_at IS NULL
		  AND (shares.expires_at IS NULL OR shares.expires_at > ?)
		ORDER BY shares.created_at DESC, shares.id DESC
	`, ownerUserID, ConversationSnapshotVersion, databaseTime(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	shares := make([]ConversationShare, 0)
	for rows.Next() {
		var share ConversationShare
		if err := rows.Scan(
			&share.ID,
			&share.OwnerUserID,
			&share.ConversationID,
			&share.SnapshotVersion,
			&share.CreatedAt,
			&share.ExpiresAt,
			&share.LastAccessedAt,
			&share.SingleUse,
		); err != nil {
			return nil, err
		}
		shares = append(shares, share)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return shares, nil
}

func (db *DB) AcquirePublicConversationShare(ctx context.Context, id string, now time.Time) (ConversationShare, error) {
	id = strings.TrimSpace(id)
	now = databaseTime(now)
	row := db.sql.QueryRowContext(ctx, `
		SELECT id, snapshot_version, snapshot_json, single_use
		FROM conversation_shares
		WHERE id = ?
		  AND snapshot_version = ?
		  AND revoked_at IS NULL
		  AND single_use = 0
		  AND (expires_at IS NULL OR expires_at > ?)
	`, id, ConversationSnapshotVersion, now)
	var share ConversationShare
	if err := row.Scan(&share.ID, &share.SnapshotVersion, &share.SnapshotJSON, &share.SingleUse); err == nil {
		return share, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return ConversationShare{}, err
	}

	tx, err := db.beginWriteTx(ctx)
	if err != nil {
		return ConversationShare{}, err
	}
	defer tx.Rollback()
	row = tx.QueryRowContext(ctx, `
        SELECT id, snapshot_version, snapshot_json, single_use
        FROM conversation_shares
        WHERE id = ? AND snapshot_version = ? AND revoked_at IS NULL
          AND single_use = 1 AND (expires_at IS NULL OR expires_at > ?)
	`+db.forUpdateClause(), id, ConversationSnapshotVersion, now)
	if err := row.Scan(&share.ID, &share.SnapshotVersion, &share.SnapshotJSON, &share.SingleUse); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ConversationShare{}, ErrNotFound
		}
		return ConversationShare{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM conversation_shares WHERE id = ?`, id); err != nil {
		return ConversationShare{}, err
	}
	if err := tx.Commit(); err != nil {
		return ConversationShare{}, err
	}

	return share, nil
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

func ValidConversationShareConversationID(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" &&
		len(value) <= MaxConversationShareConversationIDBytes &&
		!strings.ContainsFunc(value, unicode.IsControl)
}

func newConversationShareID() (string, error) {
	var raw [18]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "share_" + base64.RawURLEncoding.EncodeToString(raw[:]), nil
}
