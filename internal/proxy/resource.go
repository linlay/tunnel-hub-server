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
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/linlay/zenmind-tunnel-server/internal/store"
	"github.com/linlay/zenmind-tunnel-server/internal/tunnel"
)

const resourcePushPathPrefix = "/api/push/"

type resourceStore struct {
	mu    sync.Mutex
	items map[string]*pendingResource
}

type pendingResource struct {
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

type desktopResourcePayload struct {
	File    string `json:"file"`
	PushURL string `json:"pushURL"`
}

type desktopResourceBusinessRequest struct {
	NS      string                 `json:"ns"`
	Frame   string                 `json:"frame"`
	Type    string                 `json:"type"`
	ID      string                 `json:"id"`
	Payload desktopResourcePayload `json:"payload"`
}

func newResourceStore() *resourceStore {
	return &resourceStore{items: make(map[string]*pendingResource)}
}

func (s *resourceStore) add(item *pendingResource) {
	s.mu.Lock()
	if s.items == nil {
		s.items = make(map[string]*pendingResource)
	}
	item.timer = time.AfterFunc(time.Until(item.ExpiresAt), func() {
		s.remove(item.ID)
	})
	s.items[item.ID] = item
	s.mu.Unlock()
}

func (s *resourceStore) remove(id string) {
	var item *pendingResource
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
	filePath := item.Path
	item.Path = ""
	if item.timer != nil {
		item.timer.Stop()
	}
	item.mu.Unlock()
	if filePath != "" {
		_ = os.Remove(filePath)
	}
}

func (s *resourceStore) getForPush(id, ticket string) (*pendingResource, int, string) {
	var expired *pendingResource
	s.mu.Lock()
	item := s.items[id]
	if item == nil {
		s.mu.Unlock()
		return nil, http.StatusNotFound, "resource not found"
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
		filePath := expired.Path
		expired.Path = ""
		expired.mu.Unlock()
		if filePath != "" {
			_ = os.Remove(filePath)
		}
		return nil, http.StatusNotFound, "resource expired"
	}
	if subtle.ConstantTimeCompare([]byte(ticket), []byte(item.Ticket)) != 1 {
		s.mu.Unlock()
		return nil, http.StatusForbidden, "invalid ticket"
	}
	s.mu.Unlock()
	return item, http.StatusOK, ""
}

func (r *Relay) HandleResource(w http.ResponseWriter, req *http.Request) {
	if !isHostUnderBaseDomain(req.Host, r.DesktopBaseDomain) {
		http.NotFound(w, req)
		return
	}
	if req.Method != http.MethodGet {
		writeUploadError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	authToken := bearerToken(req.Header.Get("Authorization"))
	if authToken == "" {
		writeUploadError(w, http.StatusUnauthorized, "desktop token required")
		return
	}
	filePath, status, message := resourceFile(req)
	if status != http.StatusOK {
		writeUploadError(w, status, message)
		return
	}

	device, err := r.DB.GetDesktopDeviceByPublicHost(req.Context(), tunnel.NormalizeHost(req.Host))
	if errors.Is(err, store.ErrNotFound) {
		writeUploadError(w, http.StatusNotFound, "desktop not found")
		return
	}
	if err != nil {
		r.writeGatewayError(w, "desktop lookup failed", err)
		return
	}

	resourceID, err := randomUploadToken("hub_resource_")
	if err != nil {
		r.writeGatewayError(w, "create resource id failed", err)
		return
	}
	ticket, err := randomUploadToken("")
	if err != nil {
		r.writeGatewayError(w, "create resource ticket failed", err)
		return
	}
	item := &pendingResource{
		ID:        resourceID,
		Ticket:    ticket,
		FileName:  sanitizeUploadFileName(path.Base(filePath)),
		ExpiresAt: time.Now().Add(uploadPullTTL),
		Ready:     make(chan struct{}),
	}
	r.resources.add(item)
	defer r.resources.remove(resourceID)

	ctx, cancel := context.WithTimeout(req.Context(), uploadDesktopBridgeTimeout)
	defer cancel()
	status, message, err = r.forwardResourceToDesktop(ctx, req, device, authToken, resourceID, filePath, r.resourcePushURL(req, resourceID, ticket))
	if err != nil {
		if ctx.Err() != nil {
			writeUploadError(w, http.StatusGatewayTimeout, "desktop resource timed out")
			return
		}
		r.Logger.Error("forward resource to desktop", "error", err)
		writeUploadError(w, statusOr(status, http.StatusBadGateway), messageOr(message, "resource failed"))
		return
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		writeUploadError(w, statusOr(status, http.StatusBadGateway), messageOr(message, "resource failed"))
		return
	}
	if err := waitResourceReady(ctx, item); err != nil {
		writeUploadError(w, http.StatusGatewayTimeout, "desktop resource timed out")
		return
	}
	if err := servePendingResource(w, item); err != nil {
		r.Logger.Error("serve resource", "error", err)
		writeUploadError(w, http.StatusBadGateway, "resource failed")
	}
}

func (r *Relay) HandlePush(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		writeUploadError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id, err := resourcePushID(req.URL.Path)
	if err != nil {
		writeUploadError(w, http.StatusNotFound, "resource not found")
		return
	}
	item, status, message := r.resources.getForPush(id, req.URL.Query().Get("ticket"))
	if status != http.StatusOK {
		writeUploadError(w, status, message)
		return
	}
	if status, message := r.saveResourcePush(w, req, item); status != http.StatusOK {
		writeUploadError(w, status, message)
		return
	}
	writeUploadJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

func resourceFile(req *http.Request) (string, int, string) {
	values, ok := req.URL.Query()["file"]
	if !ok || len(values) != 1 {
		return "", http.StatusBadRequest, "file is required"
	}
	value := strings.TrimSpace(values[0])
	if value == "" {
		return "", http.StatusBadRequest, "file is required"
	}
	if strings.ContainsAny(value, "\x00\\") || path.IsAbs(value) {
		return "", http.StatusBadRequest, "file must be a safe relative path"
	}
	cleaned := path.Clean(value)
	if cleaned != value || cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || !strings.Contains(cleaned, "/") {
		return "", http.StatusBadRequest, "file must be a chat-relative path"
	}
	return cleaned, http.StatusOK, ""
}

func (r *Relay) forwardResourceToDesktop(ctx context.Context, req *http.Request, device store.DesktopDevice, authToken, resourceID, filePath, pushURL string) (int, string, error) {
	stream, err := r.Manager.OpenStream(ctx, device.TokenID)
	if errors.Is(err, ErrNoAgent) {
		return http.StatusBadGateway, "desktop is offline", err
	}
	if err != nil {
		return http.StatusBadGateway, "open desktop stream failed", err
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
		return http.StatusBadGateway, "write desktop request metadata failed", err
	}
	var openResponse tunnel.StreamResponse
	if err := tunnel.ReadJSON(stream, &openResponse); err != nil {
		return http.StatusBadGateway, "read desktop response metadata failed", err
	}
	if !standardResponseOK(openResponse, tunnel.NamespaceDesktop, tunnel.TypeDesktopWebSocketOpen) {
		return standardStreamStatus(openResponse, http.StatusBadGateway), messageOr(openResponse.Msg, "desktop websocket open failed"), nil
	}

	frame := desktopResourceBusinessRequest{
		NS:    "ap",
		Frame: "request",
		Type:  "/api/resource",
		ID:    resourceID,
		Payload: desktopResourcePayload{
			File:    filePath,
			PushURL: pushURL,
		},
	}
	data, err := json.Marshal(frame)
	if err != nil {
		return http.StatusInternalServerError, "marshal resource frame failed", err
	}
	if err := tunnel.WriteWSFrame(stream, websocket.TextMessage, data); err != nil {
		return http.StatusBadGateway, "write resource frame failed", err
	}
	return readDesktopResourceResponse(stream, resourceID)
}

func readDesktopResourceResponse(r io.Reader, requestID string) (int, string, error) {
	for {
		header, payload, err := tunnel.ReadWSFrame(r)
		if err != nil {
			return http.StatusBadGateway, "read resource response failed", err
		}
		if header.Type != websocket.TextMessage {
			continue
		}
		var frame desktopBusinessResponse
		if err := json.Unmarshal(payload, &frame); err != nil {
			return http.StatusBadGateway, "invalid resource response frame", err
		}
		if frame.ID != requestID {
			continue
		}
		if frame.Frame == "error" || frame.Code != 0 {
			return uploadFrameStatus(frame.Code), messageOr(frame.Msg, "resource failed"), nil
		}
		if frame.Frame == "response" {
			return http.StatusOK, "", nil
		}
	}
}

func (r *Relay) saveResourcePush(w http.ResponseWriter, req *http.Request, item *pendingResource) (int, string) {
	item.mu.Lock()
	if item.removed {
		item.mu.Unlock()
		return http.StatusNotFound, "resource not found"
	}
	if item.completed || item.uploading {
		item.mu.Unlock()
		return http.StatusConflict, "resource already pushed"
	}
	item.uploading = true
	item.mu.Unlock()

	file, status, message := saveResourceBody(w, req, r.MaxRequestBodyBytes, item.FileName)
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
		return http.StatusNotFound, "resource not found"
	}
	item.Path = file.Path
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

func saveResourceBody(w http.ResponseWriter, req *http.Request, maxBodyBytes int64, fileName string) (savedUploadFile, int, string) {
	limit := maxBodyBytes
	if limit <= 0 || limit > maxAgentPlatformUploadBody {
		limit = maxAgentPlatformUploadBody
	}
	if fileName == "" {
		return savedUploadFile{}, http.StatusBadRequest, "file name is required"
	}
	tmp, err := os.CreateTemp("", "tunnel-hub-resource-*")
	if err != nil {
		return savedUploadFile{}, http.StatusInternalServerError, "create temp file failed"
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	hasher := sha256.New()
	req.Body = http.MaxBytesReader(w, req.Body, limit)
	size, err := copyResourceFile(tmp, req.Body, hasher)
	closeErr := tmp.Close()
	_ = req.Body.Close()
	if err != nil {
		return savedUploadFile{}, uploadReadErrorStatus(err), "read resource file failed"
	}
	if closeErr != nil {
		return savedUploadFile{}, http.StatusInternalServerError, "write resource file failed"
	}
	sha := hex.EncodeToString(hasher.Sum(nil))
	if expected := strings.TrimSpace(req.Header.Get("X-File-SHA256")); expected != "" && !equalSHA256(expected, sha) {
		return savedUploadFile{}, http.StatusBadRequest, "sha256 mismatch"
	}
	mimeType := strings.TrimSpace(req.Header.Get("Content-Type"))
	if mimeType == "" {
		mimeType = detectUploadMimeType(tmpPath)
	}
	cleanup = false
	return savedUploadFile{
		Path:      tmpPath,
		FileName:  fileName,
		MimeType:  mimeType,
		SizeBytes: size,
		SHA256:    sha,
	}, http.StatusOK, ""
}

func copyResourceFile(dst io.Writer, src io.Reader, hasher hash.Hash) (int64, error) {
	return io.Copy(io.MultiWriter(dst, hasher), src)
}

func waitResourceReady(ctx context.Context, item *pendingResource) error {
	select {
	case <-item.Ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func servePendingResource(w http.ResponseWriter, item *pendingResource) error {
	item.mu.Lock()
	filePath := item.Path
	fileName := item.FileName
	mimeType := item.MimeType
	sizeBytes := item.SizeBytes
	sha := item.SHA256
	item.mu.Unlock()
	if filePath == "" {
		return errors.New("resource file is empty")
	}
	file, err := os.Open(filePath)
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

func (r *Relay) resourcePushURL(req *http.Request, resourceID, ticket string) string {
	push := url.URL{
		Scheme: requestScheme(req),
		Host:   req.Host,
		Path:   resourcePushPathPrefix + resourceID,
	}
	query := push.Query()
	query.Set("ticket", ticket)
	push.RawQuery = query.Encode()
	return push.String()
}

func resourcePushID(value string) (string, error) {
	raw := strings.TrimPrefix(value, resourcePushPathPrefix)
	if raw == value || raw == "" || strings.Contains(raw, "/") {
		return "", errors.New("invalid resource id")
	}
	id, err := url.PathUnescape(raw)
	if err != nil || id == "" || strings.Contains(id, "/") {
		return "", errors.New("invalid resource id")
	}
	return id, nil
}

func equalSHA256(expected, actual string) bool {
	expected = strings.ToLower(strings.TrimSpace(expected))
	actual = strings.ToLower(strings.TrimSpace(actual))
	if len(expected) != len(actual) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(expected), []byte(actual)) == 1
}
