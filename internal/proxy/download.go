package proxy

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/linlay/zenmind-tunnel-server/internal/store"
	"github.com/linlay/zenmind-tunnel-server/internal/tunnel"
)

const downloadPushPathPrefix = "/api/download/push/"

type downloadStore struct {
	mu    sync.Mutex
	items map[string]*pendingDownload
}

type pendingDownload struct {
	ID        string
	Ticket    string
	Path      string
	FileName  string
	MimeType  string
	SizeBytes int64
	SHA256    string
	ExpiresAt time.Time
	Ready     chan struct{}
	timer     *time.Timer

	mu         sync.Mutex
	uploading  bool
	completed  bool
	removed    bool
	readyFired bool
}

type parsedDownloadRequest struct {
	ChatID      string `json:"chatId"`
	RequestID   string `json:"requestId"`
	PublicHost  string `json:"publicHost"`
	ResourceID  string `json:"resourceId"`
	ResourceURL string `json:"resourceUrl"`
}

type desktopDownloadPayload struct {
	ChatID      string                `json:"chatId"`
	RequestID   string                `json:"requestId"`
	ResourceID  string                `json:"resourceId,omitempty"`
	ResourceURL string                `json:"resourceUrl,omitempty"`
	Download    desktopDownloadTicket `json:"download"`
}

type desktopDownloadTicket struct {
	ID     string `json:"id"`
	Method string `json:"method"`
	URL    string `json:"url"`
}

type desktopDownloadBusinessRequest struct {
	NS      string                 `json:"ns"`
	Frame   string                 `json:"frame"`
	Type    string                 `json:"type"`
	ID      string                 `json:"id"`
	Payload desktopDownloadPayload `json:"payload"`
}

func newDownloadStore() *downloadStore {
	return &downloadStore{items: make(map[string]*pendingDownload)}
}

func (s *downloadStore) add(item *pendingDownload) {
	s.mu.Lock()
	if s.items == nil {
		s.items = make(map[string]*pendingDownload)
	}
	item.timer = time.AfterFunc(time.Until(item.ExpiresAt), func() {
		s.remove(item.ID)
	})
	s.items[item.ID] = item
	s.mu.Unlock()
}

func (s *downloadStore) remove(id string) {
	var item *pendingDownload
	s.mu.Lock()
	if s.items != nil {
		item = s.items[id]
		delete(s.items, id)
	}
	s.mu.Unlock()
	if item == nil {
		return
	}
	item.mu.Lock()
	item.removed = true
	path := item.Path
	item.Path = ""
	if item.timer != nil {
		item.timer.Stop()
	}
	item.mu.Unlock()
	if path != "" {
		_ = os.Remove(path)
	}
}

func (s *downloadStore) getForPush(id, ticket string) (*pendingDownload, int, string) {
	var expired *pendingDownload
	s.mu.Lock()
	item := s.items[id]
	if item == nil {
		s.mu.Unlock()
		return nil, http.StatusNotFound, "download not found"
	}
	if time.Now().After(item.ExpiresAt) {
		expired = item
		delete(s.items, id)
		s.mu.Unlock()
		if expired.timer != nil {
			expired.timer.Stop()
		}
		expired.mu.Lock()
		expired.removed = true
		path := expired.Path
		expired.Path = ""
		expired.mu.Unlock()
		if path != "" {
			_ = os.Remove(path)
		}
		return nil, http.StatusNotFound, "download expired"
	}
	if subtle.ConstantTimeCompare([]byte(ticket), []byte(item.Ticket)) != 1 {
		s.mu.Unlock()
		return nil, http.StatusForbidden, "invalid ticket"
	}
	s.mu.Unlock()
	return item, http.StatusOK, ""
}

func (r *Relay) HandleDownload(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		writeUploadError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	authToken := bearerToken(req.Header.Get("Authorization"))
	if authToken == "" {
		writeUploadError(w, http.StatusUnauthorized, "desktop token required")
		return
	}
	parsed, status, message := r.parseDownloadRequest(w, req)
	if status != http.StatusOK {
		writeUploadError(w, status, message)
		return
	}

	publicHost := r.uploadPublicHost(req, parsed.PublicHost)
	if publicHost == "" {
		writeUploadError(w, http.StatusBadRequest, "publicHost is required")
		return
	}
	device, err := r.DB.GetDesktopDeviceByPublicHost(req.Context(), publicHost)
	if errors.Is(err, store.ErrNotFound) {
		writeUploadError(w, http.StatusNotFound, "desktop not found")
		return
	}
	if err != nil {
		r.writeGatewayError(w, "desktop lookup failed", err)
		return
	}

	downloadID, err := randomUploadToken("hub_download_")
	if err != nil {
		r.writeGatewayError(w, "create download id failed", err)
		return
	}
	ticket, err := randomUploadToken("")
	if err != nil {
		r.writeGatewayError(w, "create download ticket failed", err)
		return
	}
	if parsed.RequestID == "" {
		parsed.RequestID = downloadID
	}
	item := &pendingDownload{
		ID:        downloadID,
		Ticket:    ticket,
		ExpiresAt: time.Now().Add(uploadPullTTL),
		Ready:     make(chan struct{}),
	}
	r.downloads.add(item)
	defer r.downloads.remove(downloadID)

	ctx, cancel := context.WithTimeout(req.Context(), uploadDesktopBridgeTimeout)
	defer cancel()
	_, status, message, err = r.forwardDownloadToDesktop(ctx, req, device, authToken, parsed, downloadID, r.downloadPushURL(req, downloadID, ticket))
	if err != nil {
		if ctx.Err() != nil {
			writeUploadError(w, http.StatusGatewayTimeout, "desktop download timed out")
			return
		}
		r.Logger.Error("forward download to desktop", "error", err)
		writeUploadError(w, statusOr(status, http.StatusBadGateway), messageOr(message, "download failed"))
		return
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		writeUploadError(w, statusOr(status, http.StatusBadGateway), messageOr(message, "download failed"))
		return
	}
	if err := waitDownloadReady(ctx, item); err != nil {
		writeUploadError(w, http.StatusGatewayTimeout, "desktop download timed out")
		return
	}
	if err := servePendingDownload(w, item); err != nil {
		r.Logger.Error("serve download", "error", err)
		writeUploadError(w, http.StatusBadGateway, "download failed")
		return
	}
}

func (r *Relay) HandleDownloadPush(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		writeUploadError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id, err := downloadPushID(req.URL.Path)
	if err != nil {
		writeUploadError(w, http.StatusNotFound, "download not found")
		return
	}
	item, status, message := r.downloads.getForPush(id, req.URL.Query().Get("ticket"))
	if status != http.StatusOK {
		writeUploadError(w, status, message)
		return
	}
	if status, message := r.saveDownloadPush(w, req, item); status != http.StatusOK {
		writeUploadError(w, status, message)
		return
	}
	writeUploadJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

func (r *Relay) parseDownloadRequest(w http.ResponseWriter, req *http.Request) (parsedDownloadRequest, int, string) {
	req.Body = http.MaxBytesReader(w, req.Body, maxUploadFieldBytes)
	defer req.Body.Close()
	var out parsedDownloadRequest
	if err := json.NewDecoder(req.Body).Decode(&out); err != nil {
		return out, http.StatusBadRequest, "invalid JSON body"
	}
	out.ChatID = strings.TrimSpace(out.ChatID)
	out.RequestID = strings.TrimSpace(out.RequestID)
	out.PublicHost = strings.TrimSpace(out.PublicHost)
	out.ResourceID = strings.TrimSpace(out.ResourceID)
	out.ResourceURL = strings.TrimSpace(out.ResourceURL)
	if out.ChatID == "" {
		return out, http.StatusBadRequest, "chatId is required"
	}
	if out.ResourceID == "" && out.ResourceURL == "" {
		return out, http.StatusBadRequest, "resourceId or resourceUrl is required"
	}
	if out.ResourceURL != "" && !isRelativeDesktopResourceURL(out.ResourceURL) {
		return out, http.StatusBadRequest, "resourceUrl must be a relative path"
	}
	return out, http.StatusOK, ""
}

func (r *Relay) forwardDownloadToDesktop(ctx context.Context, req *http.Request, device store.DesktopDevice, authToken string, parsed parsedDownloadRequest, downloadID, pushURL string) (json.RawMessage, int, string, error) {
	stream, err := r.Manager.OpenStream(ctx, device.TokenID)
	if errors.Is(err, ErrNoAgent) {
		return nil, http.StatusBadGateway, "desktop is offline", err
	}
	if err != nil {
		return nil, http.StatusBadGateway, "open desktop stream failed", err
	}
	defer func() {
		_ = stream.Close()
		r.Manager.StreamClosed()
	}()

	openID := requestID()
	openRequest := tunnel.NewStreamRequest(tunnel.NamespaceDesktop, tunnel.FrameRequest, tunnel.TypeDesktopWebSocketOpen, openID, &tunnel.StreamPayload{
		AuthToken: authToken,
		Source:    "tunnel-hub",
		Public:    publicRequest(req, http.Header{}),
	})
	if err := tunnel.WriteJSON(stream, openRequest); err != nil {
		return nil, http.StatusBadGateway, "write desktop request metadata failed", err
	}
	var openResponse tunnel.StreamResponse
	if err := tunnel.ReadJSON(stream, &openResponse); err != nil {
		return nil, http.StatusBadGateway, "read desktop response metadata failed", err
	}
	if !standardResponseOK(openResponse, tunnel.NamespaceDesktop, tunnel.TypeDesktopWebSocketOpen) {
		return nil, standardStreamStatus(openResponse, http.StatusBadGateway), messageOr(openResponse.Msg, "desktop websocket open failed"), nil
	}

	payload := desktopDownloadPayload{
		ChatID:      parsed.ChatID,
		RequestID:   parsed.RequestID,
		ResourceID:  parsed.ResourceID,
		ResourceURL: parsed.ResourceURL,
		Download: desktopDownloadTicket{
			ID:     downloadID,
			Method: http.MethodPost,
			URL:    pushURL,
		},
	}
	frame := desktopDownloadBusinessRequest{
		NS:      "ap",
		Frame:   "request",
		Type:    "/api/download",
		ID:      parsed.RequestID,
		Payload: payload,
	}
	data, err := json.Marshal(frame)
	if err != nil {
		return nil, http.StatusInternalServerError, "marshal download frame failed", err
	}
	if err := tunnel.WriteWSFrame(stream, websocket.TextMessage, data); err != nil {
		return nil, http.StatusBadGateway, "write download frame failed", err
	}
	return readDesktopDownloadResponse(stream, parsed.RequestID)
}

func readDesktopDownloadResponse(r io.Reader, requestID string) (json.RawMessage, int, string, error) {
	for {
		header, payload, err := tunnel.ReadWSFrame(r)
		if err != nil {
			return nil, http.StatusBadGateway, "read download response failed", err
		}
		if header.Type != websocket.TextMessage {
			continue
		}
		var frame desktopBusinessResponse
		if err := json.Unmarshal(payload, &frame); err != nil {
			return nil, http.StatusBadGateway, "invalid download response frame", err
		}
		if frame.ID != requestID {
			continue
		}
		if frame.Frame == "error" || frame.Code != 0 {
			return frame.Data, uploadFrameStatus(frame.Code), messageOr(frame.Msg, "download failed"), nil
		}
		if frame.Frame != "response" {
			continue
		}
		if len(frame.Data) == 0 {
			return json.RawMessage(`{}`), http.StatusOK, "", nil
		}
		return frame.Data, http.StatusOK, "", nil
	}
}

func (r *Relay) saveDownloadPush(w http.ResponseWriter, req *http.Request, item *pendingDownload) (int, string) {
	item.mu.Lock()
	if item.removed {
		item.mu.Unlock()
		return http.StatusNotFound, "download not found"
	}
	if item.completed || item.uploading {
		item.mu.Unlock()
		return http.StatusConflict, "download already pushed"
	}
	item.uploading = true
	item.mu.Unlock()

	file, status, message := saveDownloadBody(w, req, r.MaxRequestBodyBytes)
	item.mu.Lock()
	item.uploading = false
	if status != http.StatusOK {
		item.mu.Unlock()
		if file.Path != "" {
			_ = os.Remove(file.Path)
		}
		return status, message
	}
	if item.removed {
		item.mu.Unlock()
		_ = os.Remove(file.Path)
		return http.StatusNotFound, "download not found"
	}
	item.Path = file.Path
	item.FileName = file.FileName
	item.MimeType = file.MimeType
	item.SizeBytes = file.SizeBytes
	item.SHA256 = file.SHA256
	item.completed = true
	if !item.readyFired {
		close(item.Ready)
		item.readyFired = true
	}
	item.mu.Unlock()
	return http.StatusOK, ""
}

func saveDownloadBody(w http.ResponseWriter, req *http.Request, maxBodyBytes int64) (savedUploadFile, int, string) {
	limit := maxBodyBytes
	if limit <= 0 || limit > maxAgentPlatformUploadBody {
		limit = maxAgentPlatformUploadBody
	}
	fileName := sanitizeUploadFileName(req.Header.Get("X-File-Name"))
	if fileName == "" {
		return savedUploadFile{}, http.StatusBadRequest, "file name is required"
	}
	tmp, err := os.CreateTemp("", "tunnel-hub-download-*")
	if err != nil {
		return savedUploadFile{}, http.StatusInternalServerError, "create temp file failed"
	}
	path := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(path)
		}
	}()

	hasher := sha256.New()
	req.Body = http.MaxBytesReader(w, req.Body, limit)
	size, err := copyDownloadFile(tmp, req.Body, hasher)
	closeErr := tmp.Close()
	_ = req.Body.Close()
	if err != nil {
		return savedUploadFile{}, uploadReadErrorStatus(err), "read download file failed"
	}
	if closeErr != nil {
		return savedUploadFile{}, http.StatusInternalServerError, "write download file failed"
	}
	sha := hex.EncodeToString(hasher.Sum(nil))
	if expected := strings.TrimSpace(req.Header.Get("X-File-SHA256")); expected != "" && !equalSHA256(expected, sha) {
		return savedUploadFile{}, http.StatusBadRequest, "sha256 mismatch"
	}
	mimeType := strings.TrimSpace(req.Header.Get("Content-Type"))
	if mimeType == "" {
		mimeType = detectUploadMimeType(path)
	}
	cleanup = false
	return savedUploadFile{
		Path:      path,
		FileName:  fileName,
		MimeType:  mimeType,
		SizeBytes: size,
		SHA256:    sha,
	}, http.StatusOK, ""
}

func copyDownloadFile(dst io.Writer, src io.Reader, hasher hash.Hash) (int64, error) {
	return io.Copy(io.MultiWriter(dst, hasher), src)
}

func waitDownloadReady(ctx context.Context, item *pendingDownload) error {
	select {
	case <-item.Ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func servePendingDownload(w http.ResponseWriter, item *pendingDownload) error {
	item.mu.Lock()
	path := item.Path
	fileName := item.FileName
	mimeType := item.MimeType
	sizeBytes := item.SizeBytes
	sha := item.SHA256
	item.mu.Unlock()
	if path == "" {
		return errors.New("download file is empty")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", mimeType)
	w.Header().Set("Content-Length", strconv.FormatInt(sizeBytes, 10))
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": fileName}))
	if sha != "" {
		w.Header().Set("X-Content-SHA256", sha)
	}
	_, err = io.Copy(w, file)
	return err
}

func (r *Relay) downloadPushURL(req *http.Request, downloadID, ticket string) string {
	push := url.URL{
		Scheme: requestScheme(req),
		Host:   req.Host,
		Path:   downloadPushPathPrefix + downloadID,
	}
	query := push.Query()
	query.Set("ticket", ticket)
	push.RawQuery = query.Encode()
	return push.String()
}

func downloadPushID(path string) (string, error) {
	raw := strings.TrimPrefix(path, downloadPushPathPrefix)
	if raw == path || raw == "" || strings.Contains(raw, "/") {
		return "", errors.New("invalid download id")
	}
	id, err := url.PathUnescape(raw)
	if err != nil || id == "" || strings.Contains(id, "/") {
		return "", errors.New("invalid download id")
	}
	return id, nil
}

func isRelativeDesktopResourceURL(value string) bool {
	if strings.HasPrefix(value, "//") {
		return false
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.IsAbs() || parsed.Host != "" {
		return false
	}
	return strings.HasPrefix(parsed.Path, "/")
}

func equalSHA256(expected, actual string) bool {
	expected = strings.ToLower(strings.TrimSpace(expected))
	actual = strings.ToLower(strings.TrimSpace(actual))
	if len(expected) != len(actual) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(expected), []byte(actual)) == 1
}
