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

const ConversationDocumentVersion = 1
const MaxConversationShareConversationIDBytes = 255

type ConversationShare struct {
	ID              string
	OwnerUserID     string
	ConversationID  string
	DocumentVersion int
	HTMLDocument    []byte
	CreatedAt       time.Time
	ExpiresAt       *time.Time
	LastAccessedAt  *time.Time
	SingleUse       bool
}

func (db *DB) CreateConversationShare(
	ctx context.Context,
	ownerUserID string,
	conversationID string,
	documentVersion int,
	htmlDocument []byte,
	createdAt time.Time,
	expiresAt *time.Time,
	singleUse bool,
) (ConversationShare, error) {
	ownerUserID = strings.TrimSpace(ownerUserID)
	conversationID = strings.TrimSpace(conversationID)
	createdAt = createdAt.UTC()
	if expiresAt != nil {
		normalized := expiresAt.UTC()
		expiresAt = &normalized
	}
	if ownerUserID == "" {
		return ConversationShare{}, errors.New("owner user id is required")
	}
	if !ValidConversationShareConversationID(conversationID) {
		return ConversationShare{}, errors.New("invalid conversation id")
	}
	if documentVersion != ConversationDocumentVersion {
		return ConversationShare{}, errors.New("unsupported conversation document version")
	}
	if len(htmlDocument) == 0 {
		return ConversationShare{}, errors.New("HTML document is required")
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
		DocumentVersion: documentVersion,
		HTMLDocument:    htmlDocument,
		CreatedAt:       createdAt,
		ExpiresAt:       expiresAt,
		SingleUse:       singleUse,
	}
	_, err = db.sql.ExecContext(ctx, `
		INSERT INTO conversation_shares (
			id, owner_user_id, conversation_id, document_version, html_document,
			created_at, expires_at, single_use
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, share.ID, share.OwnerUserID, share.ConversationID, share.DocumentVersion, share.HTMLDocument, share.CreatedAt, share.ExpiresAt, share.SingleUse)
	return share, err
}

func (db *DB) ListConversationShares(
	ctx context.Context,
	ownerUserID string,
	conversationID string,
	now time.Time,
) ([]ConversationShare, error) {
	ownerUserID = strings.TrimSpace(ownerUserID)
	conversationID = strings.TrimSpace(conversationID)
	if ownerUserID == "" {
		return nil, errors.New("owner user id is required")
	}
	if !ValidConversationShareConversationID(conversationID) {
		return nil, errors.New("invalid conversation id")
	}
	rows, err := db.sql.QueryContext(ctx, `
		SELECT shares.id, shares.owner_user_id, shares.conversation_id,
		       shares.document_version, shares.created_at, shares.expires_at,
		       access.last_accessed_at, shares.single_use
		FROM conversation_shares AS shares
		LEFT JOIN conversation_share_access AS access ON access.share_id = shares.id
		WHERE shares.owner_user_id = ?
		  AND shares.conversation_id = ?
		  AND shares.document_version = ?
		  AND shares.revoked_at IS NULL
		  AND (shares.expires_at IS NULL OR shares.expires_at > ?)
		ORDER BY shares.created_at DESC, shares.id DESC
	`, ownerUserID, conversationID, ConversationDocumentVersion, now.UTC())
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
			&share.DocumentVersion,
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
	now = now.UTC()
	row := db.sql.QueryRowContext(ctx, `
		SELECT id, document_version, html_document, single_use
		FROM conversation_shares
		WHERE id = ?
		  AND document_version = ?
		  AND revoked_at IS NULL
		  AND single_use = 0
		  AND (expires_at IS NULL OR expires_at > ?)
	`, id, ConversationDocumentVersion, now)
	var share ConversationShare
	if err := row.Scan(&share.ID, &share.DocumentVersion, &share.HTMLDocument, &share.SingleUse); err == nil {
		return share, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return ConversationShare{}, err
	}

	row = db.sql.QueryRowContext(ctx, `
		DELETE FROM conversation_shares
		WHERE id = ?
		  AND document_version = ?
		  AND revoked_at IS NULL
		  AND single_use = 1
		  AND (expires_at IS NULL OR expires_at > ?)
		RETURNING id, document_version, html_document, single_use
	`, id, ConversationDocumentVersion, now)
	if err := row.Scan(&share.ID, &share.DocumentVersion, &share.HTMLDocument, &share.SingleUse); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ConversationShare{}, ErrNotFound
		}
		return ConversationShare{}, err
	}
	return share, nil
}

func (db *DB) RevokeConversationShare(ctx context.Context, id, ownerUserID string, revokedAt time.Time) error {
	result, err := db.sql.ExecContext(ctx, `
		UPDATE conversation_shares
		SET revoked_at = ?
		WHERE id = ? AND owner_user_id = ? AND revoked_at IS NULL
	`, revokedAt.UTC(), strings.TrimSpace(id), strings.TrimSpace(ownerUserID))
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
	_, err := db.sql.ExecContext(ctx, `
		INSERT INTO conversation_share_access (share_id, last_accessed_at)
		VALUES (?, ?)
		ON CONFLICT(share_id) DO UPDATE SET last_accessed_at = excluded.last_accessed_at
	`, strings.TrimSpace(id), accessedAt.UTC())
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
