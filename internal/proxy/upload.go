package proxy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/linlay/zenmind-tunnel-server/internal/store"
	"github.com/linlay/zenmind-tunnel-server/internal/tunnel"
)

const (
	uploadPullTTL              = 5 * time.Minute
	uploadDesktopBridgeTimeout = 2 * time.Minute
	maxAgentPlatformUploadBody = 100 << 20
	maxUploadFieldBytes        = 64 << 10
)

type uploadStore struct {
	mu    sync.Mutex
	items map[string]*pendingUpload
}

type pendingUpload struct {
	ID        string
	Ticket    string
	Path      string
	FileName  string
	MimeType  string
	SizeBytes int64
	SHA256    string
	ExpiresAt time.Time
	timer     *time.Timer
}

type parsedUploadRequest struct {
	ChatID     string
	RequestID  string
	PublicHost string
	FileName   string
	MimeType   string
	SizeBytes  int64
	SHA256     string
	Path       string
}

type desktopUploadPayload struct {
	ChatID    string              `json:"chatId"`
	RequestID string              `json:"requestId"`
	Upload    desktopUploadTicket `json:"upload"`
}

type desktopUploadTicket struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Name      string `json:"name"`
	MimeType  string `json:"mimeType"`
	SizeBytes int64  `json:"sizeBytes"`
	SHA256    string `json:"sha256"`
	URL       string `json:"url"`
}

type desktopBusinessRequest struct {
	NS      string               `json:"ns"`
	Frame   string               `json:"frame"`
	Type    string               `json:"type"`
	ID      string               `json:"id"`
	Payload desktopUploadPayload `json:"payload"`
}

type desktopBusinessResponse struct {
	NS    string          `json:"ns,omitempty"`
	Frame string          `json:"frame"`
	Type  string          `json:"type"`
	ID    string          `json:"id"`
	Code  int             `json:"code"`
	Msg   string          `json:"msg"`
	Data  json.RawMessage `json:"data,omitempty"`
}

func newUploadStore() *uploadStore {
	return &uploadStore{items: make(map[string]*pendingUpload)}
}

func (s *uploadStore) add(item *pendingUpload) {
	s.mu.Lock()
	if s.items == nil {
		s.items = make(map[string]*pendingUpload)
	}
	item.timer = time.AfterFunc(time.Until(item.ExpiresAt), func() {
		s.remove(item.ID)
	})
	s.items[item.ID] = item
	s.mu.Unlock()
}

func (s *uploadStore) remove(id string) {
	var item *pendingUpload
	s.mu.Lock()
	if s.items != nil {
		item = s.items[id]
		delete(s.items, id)
	}
	s.mu.Unlock()
	if item == nil {
		return
	}
	if item.timer != nil {
		item.timer.Stop()
	}
	if item.Path != "" {
		_ = os.Remove(item.Path)
	}
}

func (s *uploadStore) get(id, ticket string) (*pendingUpload, int, string) {
	var expired *pendingUpload
	s.mu.Lock()
	item := s.items[id]
	if item == nil {
		s.mu.Unlock()
		return nil, http.StatusNotFound, "upload not found"
	}
	if time.Now().After(item.ExpiresAt) {
		expired = item
		delete(s.items, id)
		s.mu.Unlock()
		if expired.timer != nil {
			expired.timer.Stop()
		}
		if expired.Path != "" {
			_ = os.Remove(expired.Path)
		}
		return nil, http.StatusNotFound, "upload expired"
	}
	if subtle.ConstantTimeCompare([]byte(ticket), []byte(item.Ticket)) != 1 {
		s.mu.Unlock()
		return nil, http.StatusForbidden, "invalid ticket"
	}
	copy := *item
	s.mu.Unlock()
	return &copy, http.StatusOK, ""
}

func (r *Relay) HandleUpload(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		writeUploadError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	authToken := bearerToken(req.Header.Get("Authorization"))
	if authToken == "" {
		writeUploadError(w, http.StatusUnauthorized, "desktop token required")
		return
	}
	parsed, status, message := r.parseUploadMultipart(w, req)
	if status != http.StatusOK {
		writeUploadError(w, status, message)
		return
	}
	registered := false
	defer func() {
		if !registered && parsed.Path != "" {
			_ = os.Remove(parsed.Path)
		}
	}()

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

	uploadID, err := randomUploadToken("hub_upload_")
	if err != nil {
		r.writeGatewayError(w, "create upload id failed", err)
		return
	}
	ticket, err := randomUploadToken("")
	if err != nil {
		r.writeGatewayError(w, "create upload ticket failed", err)
		return
	}
	if parsed.RequestID == "" {
		parsed.RequestID = uploadID
	}
	item := &pendingUpload{
		ID:        uploadID,
		Ticket:    ticket,
		Path:      parsed.Path,
		FileName:  parsed.FileName,
		MimeType:  parsed.MimeType,
		SizeBytes: parsed.SizeBytes,
		SHA256:    parsed.SHA256,
		ExpiresAt: time.Now().Add(uploadPullTTL),
	}
	r.uploads.add(item)
	registered = true
	defer r.uploads.remove(uploadID)

	ctx, cancel := context.WithTimeout(req.Context(), uploadDesktopBridgeTimeout)
	defer cancel()
	data, status, message, err := r.forwardUploadToDesktop(ctx, req, device, authToken, parsed, uploadID, r.uploadPullURL(req, uploadID, ticket))
	if err != nil {
		if ctx.Err() != nil {
			writeUploadError(w, http.StatusGatewayTimeout, "desktop upload timed out")
			return
		}
		r.Logger.Error("forward upload to desktop", "error", err)
		writeUploadError(w, statusOr(status, http.StatusBadGateway), messageOr(message, "upload failed"))
		return
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		writeUploadError(w, statusOr(status, http.StatusBadGateway), messageOr(message, "upload failed"))
		return
	}
	writeUploadRawJSON(w, http.StatusOK, data)
}

func (r *Relay) HandlePull(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		writeUploadError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id, err := pullUploadID(req.URL.Path)
	if err != nil {
		writeUploadError(w, http.StatusNotFound, "upload not found")
		return
	}
	item, status, message := r.uploads.get(id, req.URL.Query().Get("ticket"))
	if status != http.StatusOK {
		writeUploadError(w, status, message)
		return
	}
	file, err := os.Open(item.Path)
	if err != nil {
		writeUploadError(w, http.StatusNotFound, "upload not found")
		return
	}
	defer file.Close()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", item.MimeType)
	w.Header().Set("Content-Length", strconv.FormatInt(item.SizeBytes, 10))
	if _, err := io.Copy(w, file); err != nil {
		r.Logger.Error("serve upload pull", "error", err)
	}
}

func (r *Relay) parseUploadMultipart(w http.ResponseWriter, req *http.Request) (parsedUploadRequest, int, string) {
	limit := r.MaxRequestBodyBytes
	if limit <= 0 || limit > maxAgentPlatformUploadBody {
		limit = maxAgentPlatformUploadBody
	}
	req.Body = http.MaxBytesReader(w, req.Body, limit)
	reader, err := req.MultipartReader()
	if err != nil {
		return parsedUploadRequest{}, http.StatusBadRequest, "invalid multipart form"
	}

	var out parsedUploadRequest
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return out, uploadReadErrorStatus(err), "invalid multipart form"
		}
		name := part.FormName()
		switch name {
		case "chatId":
			value, status, message := readUploadField(part)
			if status != http.StatusOK {
				_ = part.Close()
				return out, status, message
			}
			out.ChatID = value
		case "requestId":
			value, status, message := readUploadField(part)
			if status != http.StatusOK {
				_ = part.Close()
				return out, status, message
			}
			out.RequestID = value
		case "publicHost":
			value, status, message := readUploadField(part)
			if status != http.StatusOK {
				_ = part.Close()
				return out, status, message
			}
			out.PublicHost = value
		case "file":
			if out.Path != "" {
				_ = part.Close()
				return out, http.StatusBadRequest, "only one file is supported"
			}
			file, status, message := saveUploadPart(part)
			_ = part.Close()
			if status != http.StatusOK {
				return out, status, message
			}
			out.FileName = file.FileName
			out.MimeType = file.MimeType
			out.SizeBytes = file.SizeBytes
			out.SHA256 = file.SHA256
			out.Path = file.Path
		default:
			_, _ = io.Copy(io.Discard, part)
			_ = part.Close()
		}
	}
	out.ChatID = strings.TrimSpace(out.ChatID)
	out.RequestID = strings.TrimSpace(out.RequestID)
	out.PublicHost = strings.TrimSpace(out.PublicHost)
	if out.ChatID == "" {
		return out, http.StatusBadRequest, "chatId is required"
	}
	if out.Path == "" {
		return out, http.StatusBadRequest, "file is required"
	}
	return out, http.StatusOK, ""
}

func (r *Relay) forwardUploadToDesktop(ctx context.Context, req *http.Request, device store.DesktopDevice, authToken string, parsed parsedUploadRequest, uploadID, pullURL string) (json.RawMessage, int, string, error) {
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

	payload := desktopUploadPayload{
		ChatID:    parsed.ChatID,
		RequestID: parsed.RequestID,
		Upload: desktopUploadTicket{
			ID:        uploadID,
			Type:      "file",
			Name:      parsed.FileName,
			MimeType:  parsed.MimeType,
			SizeBytes: parsed.SizeBytes,
			SHA256:    parsed.SHA256,
			URL:       pullURL,
		},
	}
	frame := desktopBusinessRequest{
		NS:      "ap",
		Frame:   "request",
		Type:    "/api/upload",
		ID:      parsed.RequestID,
		Payload: payload,
	}
	data, err := json.Marshal(frame)
	if err != nil {
		return nil, http.StatusInternalServerError, "marshal upload frame failed", err
	}
	if err := tunnel.WriteWSFrame(stream, websocket.TextMessage, data); err != nil {
		return nil, http.StatusBadGateway, "write upload frame failed", err
	}
	return readDesktopUploadResponse(stream, parsed.RequestID)
}

func readDesktopUploadResponse(r io.Reader, requestID string) (json.RawMessage, int, string, error) {
	for {
		header, payload, err := tunnel.ReadWSFrame(r)
		if err != nil {
			return nil, http.StatusBadGateway, "read upload response failed", err
		}
		if header.Type != websocket.TextMessage {
			continue
		}
		var frame desktopBusinessResponse
		if err := json.Unmarshal(payload, &frame); err != nil {
			return nil, http.StatusBadGateway, "invalid upload response frame", err
		}
		if frame.ID != requestID {
			continue
		}
		if frame.Frame == "error" || frame.Code != 0 {
			return frame.Data, uploadFrameStatus(frame.Code), messageOr(frame.Msg, "upload failed"), nil
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

type savedUploadFile struct {
	Path      string
	FileName  string
	MimeType  string
	SizeBytes int64
	SHA256    string
}

func saveUploadPart(part *multipart.Part) (savedUploadFile, int, string) {
	fileName := sanitizeUploadFileName(part.FileName())
	if fileName == "" {
		return savedUploadFile{}, http.StatusBadRequest, "file name is required"
	}
	tmp, err := os.CreateTemp("", "tunnel-hub-upload-*")
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
	size, err := copyUploadFile(tmp, part, hasher)
	closeErr := tmp.Close()
	if err != nil {
		return savedUploadFile{}, uploadReadErrorStatus(err), "read upload file failed"
	}
	if closeErr != nil {
		return savedUploadFile{}, http.StatusInternalServerError, "write upload file failed"
	}
	mimeType := strings.TrimSpace(part.Header.Get("Content-Type"))
	if mimeType == "" {
		mimeType = detectUploadMimeType(path)
	}
	cleanup = false
	return savedUploadFile{
		Path:      path,
		FileName:  fileName,
		MimeType:  mimeType,
		SizeBytes: size,
		SHA256:    hex.EncodeToString(hasher.Sum(nil)),
	}, http.StatusOK, ""
}

func copyUploadFile(dst io.Writer, src io.Reader, hasher hash.Hash) (int64, error) {
	return io.Copy(io.MultiWriter(dst, hasher), src)
}

func readUploadField(part *multipart.Part) (string, int, string) {
	data, err := io.ReadAll(io.LimitReader(part, maxUploadFieldBytes+1))
	if err != nil {
		return "", uploadReadErrorStatus(err), "read form field failed"
	}
	if len(data) > maxUploadFieldBytes {
		return "", http.StatusBadRequest, "form field too large"
	}
	return strings.TrimSpace(string(data)), http.StatusOK, ""
}

func detectUploadMimeType(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return "application/octet-stream"
	}
	defer file.Close()
	var head [512]byte
	n, _ := file.Read(head[:])
	if n > 0 {
		return http.DetectContentType(head[:n])
	}
	return "application/octet-stream"
}

func (r *Relay) uploadPublicHost(req *http.Request, formValue string) string {
	if isHostUnderBaseDomain(req.Host, r.DesktopBaseDomain) {
		return tunnel.NormalizeHost(req.Host)
	}
	return normalizeUploadPublicHost(formValue)
}

func normalizeUploadPublicHost(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if parsed, err := url.Parse(value); err == nil && parsed.Host != "" {
		return tunnel.NormalizeHost(parsed.Host)
	}
	return tunnel.NormalizeHost(value)
}

func (r *Relay) uploadPullURL(req *http.Request, uploadID, ticket string) string {
	pull := url.URL{
		Scheme: requestScheme(req),
		Host:   req.Host,
		Path:   "/api/pull/" + uploadID,
	}
	query := pull.Query()
	query.Set("ticket", ticket)
	pull.RawQuery = query.Encode()
	return pull.String()
}

func requestScheme(req *http.Request) string {
	if proto := strings.TrimSpace(req.Header.Get("X-Forwarded-Proto")); proto != "" {
		if comma := strings.Index(proto, ","); comma >= 0 {
			proto = strings.TrimSpace(proto[:comma])
		}
		if proto == "http" || proto == "https" {
			return proto
		}
	}
	if req.TLS != nil {
		return "https"
	}
	return "http"
}

func pullUploadID(path string) (string, error) {
	const prefix = "/api/pull/"
	raw := strings.TrimPrefix(path, prefix)
	if raw == path || raw == "" || strings.Contains(raw, "/") {
		return "", errors.New("invalid upload id")
	}
	id, err := url.PathUnescape(raw)
	if err != nil || id == "" || strings.Contains(id, "/") {
		return "", errors.New("invalid upload id")
	}
	return id, nil
}

func sanitizeUploadFileName(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	if value == "" {
		return ""
	}
	value = filepath.Base(value)
	value = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, value)
	value = strings.TrimSpace(value)
	if value == "." || value == ".." {
		return ""
	}
	return value
}

func randomUploadToken(prefix string) (string, error) {
	raw := make([]byte, 18)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

func uploadFrameStatus(code int) int {
	if code >= http.StatusBadRequest && code <= 599 {
		return code
	}
	return http.StatusBadGateway
}

func uploadReadErrorStatus(err error) int {
	if err != nil && strings.Contains(err.Error(), "http: request body too large") {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadRequest
}

func writeUploadRawJSON(w http.ResponseWriter, status int, data json.RawMessage) {
	if len(data) == 0 {
		data = json.RawMessage(`{}`)
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func writeUploadError(w http.ResponseWriter, status int, message string) {
	writeUploadJSON(w, status, map[string]string{"error": messageOr(message, http.StatusText(status))})
}

func writeUploadJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func messageOr(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}
