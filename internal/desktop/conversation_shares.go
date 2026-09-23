package desktop

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"example.invalid/tunnel-hub-server/internal/store"
)

const maxConversationSnapshotBytes int64 = store.MaxConversationSnapshotBytes
const conversationSnapshotVersion = "1"
const conversationSnapshotVersionHeader = "X-Conversation-Snapshot-Version"
const conversationShareExpirationHeader = "X-Conversation-Share-Expiration"
const conversationShareConversationIDHeader = "X-Conversation-ID"

var errConversationShareTooLarge = errors.New("conversation snapshot is too large")

type conversationShareSizeError struct {
	actual int64
}

func (e *conversationShareSizeError) Error() string {
	return fmt.Sprintf(
		"conversation snapshot is %d bytes; limit is %d bytes (20 MiB)",
		e.actual,
		maxConversationSnapshotBytes,
	)
}

func (e *conversationShareSizeError) Unwrap() error {
	return errConversationShareTooLarge
}

func newConversationShareSizeError(actual int64) error {
	return &conversationShareSizeError{actual: actual}
}

type conversationShareRecordResponse struct {
	ID             string  `json:"id"`
	ConversationID string  `json:"conversationId"`
	URL            string  `json:"url"`
	CreatedAt      string  `json:"createdAt"`
	ExpiresAt      *string `json:"expiresAt"`
	LastAccessedAt *string `json:"lastAccessedAt"`
	SingleUse      bool    `json:"singleUse"`
}

type conversationShareListResponse struct {
	Items []conversationShareRecordResponse `json:"items"`
}

func (s *Server) handleCreateConversationShare(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.authorizeRegistration(w, r)
	if !ok {
		return
	}
	policy, err := parseConversationShareExpiration(r.Header.Get(conversationShareExpirationHeader))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	conversationID := strings.TrimSpace(r.Header.Get(conversationShareConversationIDHeader))
	if !store.ValidConversationShareConversationID(conversationID) {
		writeError(w, http.StatusBadRequest, "invalid conversation id")
		return
	}
	if r.Header.Get(conversationSnapshotVersionHeader) != conversationSnapshotVersion {
		writeError(w, http.StatusBadRequest, "unsupported conversation snapshot version")
		return
	}
	if s.conversationShareResources == nil {
		writeError(w, http.StatusServiceUnavailable, "conversation share resource storage is unavailable")
		return
	}
	shareURL, err := s.conversationShareBaseURL()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	shareID, err := store.NewConversationShareID()
	if err != nil {
		s.writeInternal(w, "create conversation share id", err)
		return
	}
	stage, err := s.conversationShareResources.Begin()
	if err != nil {
		s.writeInternal(w, "stage conversation share resources", err)
		return
	}
	defer stage.Abort()
	snapshot, resources, err := decodeConversationShare(w, r, stage)
	if err != nil {
		if errors.Is(err, errConversationShareResourceStorage) {
			s.writeInternal(w, "write conversation share resources", err)
			return
		}
		status := http.StatusBadRequest
		if errors.Is(err, errConversationShareTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		writeError(w, status, err.Error())
		return
	}
	if err := stage.Commit(shareID); err != nil {
		s.writeInternal(w, "commit conversation share resources", err)
		return
	}
	now := s.now().UTC()
	var expiresAt *time.Time
	if policy.duration > 0 {
		value := now.Add(policy.duration)
		expiresAt = &value
	}
	share, err := s.DB.CreateConversationShareWithResources(
		r.Context(),
		shareID,
		principal.UserID,
		conversationID,
		store.ConversationSnapshotVersion,
		snapshot,
		resources,
		now,
		expiresAt,
		policy.singleUse,
	)
	if err != nil {
		if cleanupErr := s.conversationShareResources.Delete(shareID); cleanupErr != nil {
			s.Logger.Error("remove failed conversation share resources", "shareId", shareID, "error", cleanupErr)
		}
		s.writeInternal(w, "create conversation share", err)
		return
	}
	writeJSON(w, http.StatusCreated, conversationShareRecordResponseFromStore(share, shareURL))
}

func (s *Server) handleListConversationShares(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.authorizeRegistration(w, r)
	if !ok {
		return
	}
	if r.URL.RawQuery != "" {
		writeError(w, http.StatusBadRequest, "conversation share list does not accept query parameters")
		return
	}
	shareURL, err := s.conversationShareBaseURL()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	shares, err := s.DB.ListConversationShares(r.Context(), principal.UserID, s.now().UTC())
	if err != nil {
		s.writeInternal(w, "list conversation shares", err)
		return
	}
	items := make([]conversationShareRecordResponse, 0, len(shares))
	for _, share := range shares {
		items = append(items, conversationShareRecordResponseFromStore(share, shareURL))
	}
	writeJSON(w, http.StatusOK, conversationShareListResponse{Items: items})
}

type conversationShareExpirationPolicy struct {
	duration  time.Duration
	singleUse bool
}

func parseConversationShareExpiration(value string) (conversationShareExpirationPolicy, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return conversationShareExpirationPolicy{}, errors.New("conversation share expiration is required")
	}
	switch value {
	case "once":
		return conversationShareExpirationPolicy{singleUse: true}, nil
	case "3h":
		return conversationShareExpirationPolicy{duration: 3 * time.Hour}, nil
	case "1d":
		return conversationShareExpirationPolicy{duration: 24 * time.Hour}, nil
	case "7d":
		return conversationShareExpirationPolicy{duration: 7 * 24 * time.Hour}, nil
	case "30d":
		return conversationShareExpirationPolicy{duration: 30 * 24 * time.Hour}, nil
	case "permanent":
		return conversationShareExpirationPolicy{}, nil
	default:
		return conversationShareExpirationPolicy{}, errors.New("unsupported conversation share expiration")
	}
}

func formatConversationShareExpiration(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := value.Format("2006-01-02T15:04:05.000Z07:00")
	return &formatted
}

func conversationShareRecordResponseFromStore(
	share store.ConversationShare,
	shareURL string,
) conversationShareRecordResponse {
	return conversationShareRecordResponse{
		ID:             share.ID,
		ConversationID: share.ConversationID,
		URL:            strings.TrimSuffix(shareURL, "/") + "/" + url.PathEscape(share.ID),
		CreatedAt:      share.CreatedAt.Format("2006-01-02T15:04:05.000Z07:00"),
		ExpiresAt:      formatConversationShareExpiration(share.ExpiresAt),
		LastAccessedAt: formatConversationShareExpiration(share.LastAccessedAt),
		SingleUse:      share.SingleUse,
	}
}

func (s *Server) handleRevokeConversationShare(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.authorizeRegistration(w, r)
	if !ok {
		return
	}
	id, ok := conversationShareIDFromPath(r.URL.Path, conversationSharesPath+"/")
	if !ok {
		writeError(w, http.StatusNotFound, "share not found")
		return
	}
	if err := s.DB.RevokeConversationShare(r.Context(), id, principal.UserID, s.now().UTC()); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "share not found")
			return
		}
		s.writeInternal(w, "revoke conversation share", err)
		return
	}
	if s.conversationShareResources != nil {
		if err := s.conversationShareResources.Delete(id); err != nil {
			s.Logger.Error("delete revoked conversation share resources", "shareId", id, "error", err)
		} else if err := s.DB.DeleteConversationShareResourceMetadata(r.Context(), id); err != nil {
			s.Logger.Error("delete revoked conversation share resource metadata", "shareId", id, "error", err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetPublicConversationSharePage(w http.ResponseWriter, r *http.Request) {
	id, ok := conversationShareIDFromPath(r.URL.Path, publicConversationSharePagePath)
	if !ok {
		writePublicConversationShareError(w, http.StatusNotFound)
		return
	}
	now := s.now().UTC()
	peek, err := s.DB.PeekPublicConversationShare(r.Context(), id, now)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writePublicConversationShareError(w, http.StatusNotFound)
		} else {
			s.Logger.Error("peek conversation share", "error", err)
			writePublicConversationShareError(w, http.StatusInternalServerError)
		}
		return
	}
	if r.Method == http.MethodHead {
		setPublicConversationShareHeaders(w.Header())
		w.WriteHeader(http.StatusOK)
		return
	}
	if peek.SnapshotVersion != store.ConversationSnapshotVersion {
		writePublicConversationShareError(w, http.StatusNotFound)
		return
	}
	var raw [32]byte
	if _, err = rand.Read(raw[:]); err != nil {
		writePublicConversationShareError(w, http.StatusInternalServerError)
		return
	}
	newHash := sha256.Sum256(raw[:])
	presentedHash := conversationShareSessionHash(r, id)
	share, claimed, err := s.DB.AccessPublicConversationShare(r.Context(), id, now, presentedHash, newHash[:])
	if err == nil && claimed {
		http.SetCookie(w, &http.Cookie{
			Name: conversationShareSessionCookieName, Value: base64.RawURLEncoding.EncodeToString(raw[:]),
			Path: "/share/" + id, HttpOnly: true, SameSite: http.SameSiteLaxMode,
			Secure: r.TLS != nil, MaxAge: int(store.ConversationShareSessionDuration.Seconds()),
		})
	}
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writePublicConversationShareError(w, http.StatusNotFound)
			return
		}
		s.Logger.Error("acquire conversation share", "error", err)
		writePublicConversationShareError(w, http.StatusInternalServerError)
		return
	}
	if !share.SingleUse {
		recordAccess := s.recordConversationShareAccess
		if recordAccess == nil {
			recordAccess = s.DB.RecordConversationShareAccess
		}
		if err := recordAccess(r.Context(), share.ID, now); err != nil {
			s.Logger.Error("record conversation share access", "shareId", share.ID, "error", err)
		}
	}
	if s.conversationShareRenderer == nil {
		s.Logger.Error("render conversation share", "shareId", share.ID, "error", "renderer unavailable")
		writePublicConversationShareError(w, http.StatusInternalServerError)
		return
	}
	html, err := s.conversationShareRenderer.Render(
		share.SnapshotJSON,
		s.Config.SharePublicBaseURL,
		s.Config.BrandID,
		s.Config.ProductName,
		s.Config.ProductDownloadPageURL,
	)
	if err != nil {
		s.Logger.Error("render conversation share", "shareId", share.ID, "error", err)
		writePublicConversationShareError(w, http.StatusInternalServerError)
		return
	}
	setPublicConversationShareHeaders(w.Header())
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(html)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(html)
}

const conversationShareSessionCookieName = "conversation_share_session"

func conversationShareSessionHash(r *http.Request, id string) []byte {
	cookie, err := r.Cookie(conversationShareSessionCookieName)
	if err != nil {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cookie.Value)
	if err != nil || len(raw) != 32 {
		return nil
	}
	hash := sha256.Sum256(raw)
	return hash[:]
}

func writePublicConversationShareError(w http.ResponseWriter, status int) {
	body := publicConversationShareErrorDocument(status)
	setPublicConversationShareHeaders(w.Header())
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Language", "zh-CN")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func setPublicConversationShareHeaders(header http.Header) {
	header.Set("Cache-Control", "no-store")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("X-Robots-Tag", "noindex, nofollow, noarchive")
	header.Set("Referrer-Policy", "no-referrer")
}

func conversationShareIDFromPath(path, prefix string) (string, bool) {
	id := strings.TrimPrefix(path, prefix)
	if id == path || len(id) == 0 || len(id) > 80 {
		return "", false
	}
	for _, char := range id {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '_' || char == '-' {
			continue
		}
		return "", false
	}
	return id, true
}

func (s *Server) conversationShareBaseURL() (string, error) {
	if s.Config.SharePublicBaseURL == "" {
		return "", errors.New("conversation sharing is not configured")
	}
	return s.Config.SharePublicBaseURL + "/share", nil
}
