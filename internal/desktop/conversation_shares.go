package desktop

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/linlay/zenmind-tunnel-server/internal/store"
)

const maxConversationShareBytes int64 = 2 << 20
const maxConversationShareEntries = 2000
const maxConversationShareEntryBytes = 200_000

type conversationShareSnapshot struct {
	SchemaVersion int                      `json:"schemaVersion"`
	Title         string                   `json:"title"`
	CreatedAt     int64                    `json:"createdAt"`
	UpdatedAt     int64                    `json:"updatedAt"`
	Entries       []conversationShareEntry `json:"entries"`
}

type conversationShareEntry struct {
	Type       string `json:"type"`
	Role       string `json:"role,omitempty"`
	Content    string `json:"content"`
	Label      string `json:"label,omitempty"`
	DurationMs *int64 `json:"durationMs,omitempty"`
	CreatedAt  int64  `json:"createdAt,omitempty"`
}

type conversationShareCreateResponse struct {
	ID        string `json:"id"`
	URL       string `json:"url"`
	CreatedAt string `json:"createdAt"`
}

func (s *Server) handleCreateConversationShare(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.authorizeRegistration(w, r)
	if !ok {
		return
	}
	snapshot, canonical, err := decodeConversationShareSnapshot(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	shareURL, err := s.conversationShareURL("")
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	share, err := s.DB.CreateConversationShare(r.Context(), principal.UserID, snapshot.Title, canonical)
	if err != nil {
		s.writeInternal(w, "create conversation share", err)
		return
	}
	writeJSON(w, http.StatusCreated, conversationShareCreateResponse{
		ID:        share.ID,
		URL:       strings.TrimSuffix(shareURL, "/") + "/" + url.PathEscape(share.ID),
		CreatedAt: share.CreatedAt.Format("2006-01-02T15:04:05.000Z07:00"),
	})
}

func (s *Server) handleRevokeConversationShare(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.authorizeRegistration(w, r)
	if !ok {
		return
	}
	id, ok := conversationShareIDFromPath(r.URL.Path, conversationSharesPath+"/")
	if !ok {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if err := s.DB.RevokeConversationShare(r.Context(), id, principal.UserID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "share not found")
			return
		}
		s.writeInternal(w, "revoke conversation share", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetPublicConversationShare(w http.ResponseWriter, r *http.Request) {
	id, ok := conversationShareIDFromPath(r.URL.Path, publicConversationSharesPath)
	if !ok {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	share, err := s.DB.GetPublicConversationShare(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "share not found")
			return
		}
		s.writeInternal(w, "get conversation share", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(share.SnapshotJSON)
}

func decodeConversationShareSnapshot(w http.ResponseWriter, r *http.Request) (conversationShareSnapshot, []byte, error) {
	limited := http.MaxBytesReader(w, r.Body, maxConversationShareBytes)
	defer limited.Close()
	decoder := json.NewDecoder(limited)
	decoder.DisallowUnknownFields()
	var snapshot conversationShareSnapshot
	if err := decoder.Decode(&snapshot); err != nil {
		return conversationShareSnapshot{}, nil, errors.New("invalid share snapshot")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return conversationShareSnapshot{}, nil, errors.New("invalid share snapshot")
	}
	if err := validateConversationShareSnapshot(snapshot); err != nil {
		return conversationShareSnapshot{}, nil, err
	}
	canonical, err := json.Marshal(snapshot)
	return snapshot, canonical, err
}

func validateConversationShareSnapshot(snapshot conversationShareSnapshot) error {
	if snapshot.SchemaVersion != 1 {
		return errors.New("unsupported share schemaVersion")
	}
	if title := strings.TrimSpace(snapshot.Title); title == "" || len([]byte(title)) > 300 {
		return errors.New("title must be between 1 and 300 bytes")
	}
	if snapshot.CreatedAt < 1_000_000_000_000 || snapshot.UpdatedAt < snapshot.CreatedAt {
		return errors.New("invalid snapshot time")
	}
	if len(snapshot.Entries) == 0 || len(snapshot.Entries) > maxConversationShareEntries {
		return fmt.Errorf("entries must contain between 1 and %d items", maxConversationShareEntries)
	}
	for _, entry := range snapshot.Entries {
		switch entry.Type {
		case "message":
			if entry.Role != "user" && entry.Role != "assistant" {
				return errors.New("message role must be user or assistant")
			}
			if entry.Label != "" {
				return errors.New("message label is not allowed")
			}
			if entry.DurationMs != nil {
				return errors.New("message durationMs is not allowed")
			}
		case "reasoning":
			if entry.Role != "" {
				return errors.New("reasoning role is not allowed")
			}
			if len([]byte(entry.Label)) > 300 {
				return errors.New("reasoning label is invalid")
			}
			if entry.DurationMs != nil && *entry.DurationMs < 0 {
				return errors.New("reasoning durationMs is invalid")
			}
		default:
			return errors.New("entry type must be message or reasoning")
		}
		if strings.TrimSpace(entry.Content) == "" || len([]byte(entry.Content)) > maxConversationShareEntryBytes {
			return errors.New("entry content is invalid")
		}
		if entry.CreatedAt != 0 && entry.CreatedAt < 1_000_000_000_000 {
			return errors.New("entry createdAt is invalid")
		}
	}
	return nil
}

func conversationShareIDFromPath(path, prefix string) (string, bool) {
	id := strings.TrimPrefix(path, prefix)
	if id == path || !strings.HasPrefix(id, "share_") || strings.Contains(id, "/") || len(id) > 80 {
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

func (s *Server) conversationShareURL(id string) (string, error) {
	base := strings.TrimRight(strings.TrimSpace(s.Config.SharePublicBaseURL), "/")
	if base == "" {
		return "", errors.New("conversation sharing is not configured")
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("share public base URL is invalid")
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && (parsed.Hostname() == "localhost" || parsed.Hostname() == "127.0.0.1")) {
		return "", errors.New("share public base URL must use https")
	}
	shareBase := base + "/share"
	if id == "" {
		return shareBase, nil
	}
	return shareBase + "/" + url.PathEscape(id), nil
}
