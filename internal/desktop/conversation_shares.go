package desktop

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/linlay/zenmind-tunnel-server/internal/store"
)

const maxConversationShareBytes int64 = 2 << 20
const maxConversationShareEvents = 2000
const maxConversationShareContentBytes = 200_000
const maxConversationShareTitleBytes = 300
const maxConversationShareLabelBytes = 300

var errConversationShareTooLarge = errors.New("share event stream is too large")

var conversationShareFramePrefix = []byte("event: message\ndata: ")

type conversationShareEvent struct {
	Seq            int64   `json:"seq"`
	Type           string  `json:"type"`
	ShareVersion   *int    `json:"shareVersion,omitempty"`
	ChatName       *string `json:"chatName,omitempty"`
	Message        *string `json:"message,omitempty"`
	Text           *string `json:"text,omitempty"`
	ReasoningLabel *string `json:"reasoningLabel,omitempty"`
	Timestamp      int64   `json:"timestamp"`
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
	title, eventStream, err := decodeConversationShareEventStream(w, r)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errConversationShareTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		writeError(w, status, err.Error())
		return
	}
	shareURL, err := s.conversationShareURL("")
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	share, err := s.DB.CreateConversationShare(r.Context(), principal.UserID, title, eventStream)
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
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(share.EventStream)))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(share.EventStream)
}

// decodeConversationShareEventStream implements only the finite Share SSE Profile.
// It deliberately does not accept the broader SSE grammar used by live streams.
func decodeConversationShareEventStream(w http.ResponseWriter, r *http.Request) (string, []byte, error) {
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "text/event-stream" {
		return "", nil, errors.New("Content-Type must be text/event-stream")
	}
	for name := range params {
		if name != "charset" {
			return "", nil, errors.New("share event stream has unsupported media type parameters")
		}
	}
	if charset, ok := params["charset"]; ok && !strings.EqualFold(charset, "utf-8") {
		return "", nil, errors.New("share event stream charset must be utf-8")
	}
	limited := http.MaxBytesReader(w, r.Body, maxConversationShareBytes)
	defer limited.Close()
	eventStream, err := io.ReadAll(limited)
	if err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			return "", nil, errConversationShareTooLarge
		}
		return "", nil, errors.New("invalid share event stream")
	}
	if len(eventStream) == 0 || !utf8.Valid(eventStream) || !bytes.HasSuffix(eventStream, []byte("\n\n")) {
		return "", nil, errors.New("invalid share event stream encoding")
	}

	remaining := eventStream
	expectedSeq := int64(1)
	eventCount := 0
	queryCount := 0
	turnOpen := false
	runStarted := false
	snapshotsStarted := false
	var lastTurnTimestamp int64
	var title string
	done := false

	for len(remaining) > 0 {
		boundary := bytes.Index(remaining, []byte("\n\n"))
		if boundary < 0 {
			return "", nil, errors.New("unterminated share event frame")
		}
		frame := remaining[:boundary]
		remaining = remaining[boundary+2:]
		if !bytes.HasPrefix(frame, conversationShareFramePrefix) {
			return "", nil, errors.New("invalid share event frame")
		}
		data := frame[len(conversationShareFramePrefix):]
		if len(data) == 0 || bytes.IndexByte(data, '\n') >= 0 || bytes.IndexByte(data, '\r') >= 0 {
			return "", nil, errors.New("invalid share event data")
		}
		if bytes.Equal(data, []byte("[DONE]")) {
			if len(remaining) != 0 || expectedSeq == 1 || queryCount == 0 {
				return "", nil, errors.New("invalid share event completion")
			}
			done = true
			break
		}

		var event conversationShareEvent
		if err := decodeStrictShareEvent(data, &event); err != nil {
			return "", nil, errors.New("invalid share event payload")
		}
		if event.Seq != expectedSeq || !isEpochMilliseconds(event.Timestamp) {
			return "", nil, errors.New("invalid share event sequence or time")
		}
		expectedSeq++

		if event.Seq == 1 {
			if err := validateConversationStart(event); err != nil {
				return "", nil, err
			}
			title = strings.TrimSpace(*event.ChatName)
			continue
		}
		eventCount++
		if eventCount > maxConversationShareEvents {
			return "", nil, fmt.Errorf("share event stream must contain at most %d conversation events", maxConversationShareEvents)
		}

		switch event.Type {
		case "request.query":
			if turnOpen || !onlyEventFields(event, false, false, true, false, false) || !validShareContent(event.Message) {
				return "", nil, errors.New("invalid request.query event")
			}
			turnOpen = true
			runStarted = false
			snapshotsStarted = false
			lastTurnTimestamp = event.Timestamp
			queryCount++
		case "run.start":
			if !turnOpen || runStarted || snapshotsStarted || event.Timestamp < lastTurnTimestamp || !onlyEventFields(event, false, false, false, false, false) {
				return "", nil, errors.New("invalid run.start event")
			}
			runStarted = true
			lastTurnTimestamp = event.Timestamp
		case "reasoning.snapshot":
			if !turnOpen || event.Timestamp < lastTurnTimestamp || !onlyReasoningEventFields(event) || !validShareContent(event.Text) {
				return "", nil, errors.New("invalid reasoning.snapshot event")
			}
			if event.ReasoningLabel != nil && len([]byte(*event.ReasoningLabel)) > maxConversationShareLabelBytes {
				return "", nil, errors.New("reasoning label is too large")
			}
			snapshotsStarted = true
			lastTurnTimestamp = event.Timestamp
		case "content.snapshot":
			if !turnOpen || event.Timestamp < lastTurnTimestamp || !onlyEventFields(event, false, false, false, true, false) || !validShareContent(event.Text) {
				return "", nil, errors.New("invalid content.snapshot event")
			}
			snapshotsStarted = true
			lastTurnTimestamp = event.Timestamp
		case "run.complete", "run.cancel", "run.error":
			if !turnOpen || event.Timestamp < lastTurnTimestamp || !onlyEventFields(event, false, false, false, false, false) {
				return "", nil, errors.New("invalid terminal event")
			}
			turnOpen = false
		default:
			return "", nil, errors.New("unsupported share event type")
		}
	}
	if !done {
		return "", nil, errors.New("share event stream must end with [DONE]")
	}
	return title, eventStream, nil
}

func decodeStrictShareEvent(data []byte, event *conversationShareEvent) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(event); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("multiple JSON values")
	}
	return nil
}

func validateConversationStart(event conversationShareEvent) error {
	if event.Type != "chat.start" || event.ShareVersion == nil || *event.ShareVersion != 1 || event.ChatName == nil || !onlyEventFields(event, true, true, false, false, false) {
		return errors.New("share event stream must start with chat.start version 1")
	}
	title := strings.TrimSpace(*event.ChatName)
	if title == "" || len([]byte(*event.ChatName)) > maxConversationShareTitleBytes {
		return errors.New("chat name must be between 1 and 300 bytes")
	}
	return nil
}

func onlyEventFields(event conversationShareEvent, shareVersion, chatName, message, text, reasoningLabel bool) bool {
	return (event.ShareVersion != nil) == shareVersion &&
		(event.ChatName != nil) == chatName &&
		(event.Message != nil) == message &&
		(event.Text != nil) == text &&
		(event.ReasoningLabel != nil) == reasoningLabel
}

func onlyReasoningEventFields(event conversationShareEvent) bool {
	return event.ShareVersion == nil && event.ChatName == nil && event.Message == nil && event.Text != nil
}

func validShareContent(value *string) bool {
	return value != nil && strings.TrimSpace(*value) != "" && len([]byte(*value)) <= maxConversationShareContentBytes
}

func isEpochMilliseconds(value int64) bool {
	return value >= 1_000_000_000_000
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
