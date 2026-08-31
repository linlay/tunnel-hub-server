package desktop

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"example.invalid/tunnel-hub-server/internal/store"
)

const maxConversationShareBytes int64 = 20 * 1024 * 1024
const conversationDocumentVersion = "1"
const conversationDocumentVersionHeader = "X-Conversation-Document-Version"
const conversationShareExpirationHeader = "X-Conversation-Share-Expiration"
const conversationShareConversationIDHeader = "X-Conversation-ID"

var errConversationShareTooLarge = errors.New("conversation HTML document is too large")

type conversationShareSizeError struct {
	actual int64
}

func (e *conversationShareSizeError) Error() string {
	return fmt.Sprintf(
		"conversation HTML document is %d bytes; limit is %d bytes (20 MiB)",
		e.actual,
		maxConversationShareBytes,
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
	html, err := decodeConversationHTML(r)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errConversationShareTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		writeError(w, status, err.Error())
		return
	}
	shareURL, err := s.conversationShareBaseURL()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	now := s.now().UTC()
	var expiresAt *time.Time
	if policy.duration > 0 {
		value := now.Add(policy.duration)
		expiresAt = &value
	}
	share, err := s.DB.CreateConversationShare(
		r.Context(),
		principal.UserID,
		conversationID,
		store.ConversationDocumentVersion,
		html,
		now,
		expiresAt,
		policy.singleUse,
	)
	if err != nil {
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
	conversationID := strings.TrimSpace(r.URL.Query().Get("conversationId"))
	if !store.ValidConversationShareConversationID(conversationID) {
		writeError(w, http.StatusBadRequest, "invalid conversation id")
		return
	}
	shareURL, err := s.conversationShareBaseURL()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	shares, err := s.DB.ListConversationShares(r.Context(), principal.UserID, conversationID, s.now().UTC())
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
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetPublicConversationSharePage(w http.ResponseWriter, r *http.Request) {
	id, ok := conversationShareIDFromPath(r.URL.Path, publicConversationSharePagePath)
	if !ok {
		writePublicConversationShareError(w, http.StatusNotFound)
		return
	}
	now := s.now().UTC()
	share, err := s.DB.AcquirePublicConversationShare(r.Context(), id, now)
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
	setPublicConversationShareHeaders(w.Header())
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(share.HTMLDocument)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(share.HTMLDocument)
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

func decodeConversationHTML(r *http.Request) ([]byte, error) {
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "text/html" {
		return nil, errors.New("Content-Type must be text/html")
	}
	for name := range params {
		if name != "charset" {
			return nil, errors.New("HTML document has unsupported media type parameters")
		}
	}
	if charset, ok := params["charset"]; ok && !strings.EqualFold(charset, "utf-8") {
		return nil, errors.New("HTML document charset must be utf-8")
	}
	if r.Header.Get(conversationDocumentVersionHeader) != conversationDocumentVersion {
		return nil, errors.New("unsupported conversation document version")
	}
	if r.ContentLength > maxConversationShareBytes {
		return nil, newConversationShareSizeError(r.ContentLength)
	}
	limited := io.LimitReader(r.Body, maxConversationShareBytes+1)
	html, err := io.ReadAll(limited)
	if err != nil {
		return nil, errors.New("invalid HTML document")
	}
	if int64(len(html)) > maxConversationShareBytes {
		return nil, newConversationShareSizeError(int64(len(html)))
	}
	if len(html) == 0 || !utf8.Valid(html) {
		return nil, errors.New("HTML document must be non-empty UTF-8")
	}
	return html, nil
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
