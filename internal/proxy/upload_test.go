package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
	"github.com/linlay/zenmind-tunnel-server/internal/store"
	"github.com/linlay/zenmind-tunnel-server/internal/tunnel"
)

type fakeUploadResult struct {
	OpenRequest   tunnel.StreamRequest
	UploadRequest desktopBusinessRequest
	Downloaded    []byte
}

func TestRelayUploadRejectsMainHost(t *testing.T) {
	relay := NewRelay(nil, NewManager(), nil, 64<<20)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/upload", nil)
	req.Host = "tunnel-hub.zenmind.cc"
	relay.HandleUpload(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRelayUploadForwardsToDesktopAndServesPull(t *testing.T) {
	db := openProxyTestDB(t)
	manager := NewManager()
	relay := NewRelay(db, manager, nil, 64<<20)
	relay.SetPublicBaseDomains("m.zenmind.cc", "wa.zenmind.cc")
	server := newUploadRelayTestServer(t, relay)
	defer server.Close()

	registration := registerUploadDesktop(t, db, "desk.m.zenmind.cc")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resultCh := make(chan fakeUploadResult, 1)
	go runFakeUploadDesktop(t, ctx, server.URL, registration.AgentToken, resultCh, nil)
	waitForAgentToken(t, manager, registration.Token.ID)

	body, contentType := uploadMultipartBody(t, map[string]string{
		"chatId":    "chat_upload",
		"requestId": "req_upload",
	}, "note.txt", "text/plain", []byte("hello upload"))
	req, err := http.NewRequest(http.MethodPost, server.URL+"/api/upload", body)
	if err != nil {
		t.Fatalf("new upload request: %v", err)
	}
	req.Host = registration.Device.PublicHost
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer desktop-app-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post upload: %v", err)
	}
	defer resp.Body.Close()
	responseBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upload status = %d body=%s", resp.StatusCode, string(responseBody))
	}
	var response struct {
		RequestID string `json:"requestId"`
		ChatID    string `json:"chatId"`
		Upload    struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			SandboxPath string `json:"sandboxPath"`
		} `json:"upload"`
	}
	if err := json.Unmarshal(responseBody, &response); err != nil {
		t.Fatalf("decode upload response: %v body=%s", err, string(responseBody))
	}
	if response.RequestID != "req_upload" || response.ChatID != "chat_upload" || response.Upload.ID != "r01" || response.Upload.SandboxPath != "/workspace/note.txt" {
		t.Fatalf("unexpected upload response: %+v", response)
	}

	select {
	case result := <-resultCh:
		if result.OpenRequest.Payload == nil || result.OpenRequest.Payload.AuthToken != "desktop-app-token" {
			t.Fatalf("desktop auth metadata = %#v", result.OpenRequest.Payload)
		}
		if result.UploadRequest.NS != "ap" || result.UploadRequest.Type != "/api/upload" || result.UploadRequest.ID != "req_upload" {
			t.Fatalf("upload request frame = %#v", result.UploadRequest)
		}
		sum := sha256.Sum256([]byte("hello upload"))
		if result.UploadRequest.Payload.Upload.Name != "note.txt" ||
			result.UploadRequest.Payload.Upload.SizeBytes != int64(len("hello upload")) ||
			result.UploadRequest.Payload.Upload.SHA256 != hex.EncodeToString(sum[:]) {
			t.Fatalf("upload metadata = %#v", result.UploadRequest.Payload.Upload)
		}
		if !strings.Contains(result.UploadRequest.Payload.Upload.URL, "/api/pull/") {
			t.Fatalf("pull url = %q", result.UploadRequest.Payload.Upload.URL)
		}
		if string(result.Downloaded) != "hello upload" {
			t.Fatalf("downloaded body = %q", string(result.Downloaded))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("fake desktop did not receive upload")
	}
}

func TestRelayUploadRequiresFieldsAndDesktopOnline(t *testing.T) {
	db := openProxyTestDB(t)
	manager := NewManager()
	relay := NewRelay(db, manager, nil, 64<<20)
	relay.SetPublicBaseDomains("m.zenmind.cc", "wa.zenmind.cc")
	server := newUploadRelayTestServer(t, relay)
	defer server.Close()
	registration := registerUploadDesktop(t, db, "desk.m.zenmind.cc")

	tests := []struct {
		name       string
		auth       string
		fields     map[string]string
		fileName   string
		wantStatus int
	}{
		{
			name:       "missing token",
			fields:     map[string]string{"chatId": "chat_1"},
			fileName:   "a.txt",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "missing chat id",
			auth:       "desktop-token",
			fields:     map[string]string{},
			fileName:   "a.txt",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "public host field is forbidden even when empty",
			auth:       "desktop-token",
			fields:     map[string]string{"chatId": "chat_1", "publicHost": ""},
			fileName:   "a.txt",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "missing file",
			auth:       "desktop-token",
			fields:     map[string]string{"chatId": "chat_1"},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "desktop offline",
			auth:       "desktop-token",
			fields:     map[string]string{"chatId": "chat_1"},
			fileName:   "a.txt",
			wantStatus: http.StatusBadGateway,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body, contentType := uploadMultipartBody(t, tc.fields, tc.fileName, "text/plain", []byte("body"))
			req, err := http.NewRequest(http.MethodPost, server.URL+"/api/upload", body)
			if err != nil {
				t.Fatalf("new upload request: %v", err)
			}
			req.Host = registration.Device.PublicHost
			req.Header.Set("Content-Type", contentType)
			if tc.auth != "" {
				req.Header.Set("Authorization", "Bearer "+tc.auth)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("post upload: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d want %d body=%s", resp.StatusCode, tc.wantStatus, string(body))
			}
		})
	}
}

func TestRelayPullRejectsInvalidAndExpiredTickets(t *testing.T) {
	relay := NewRelay(nil, NewManager(), nil, 64<<20)
	path := writePullFixture(t, "pull body")
	relay.uploads.add(&pendingUpload{
		ID:        "upload_ok",
		Ticket:    "ticket_ok",
		Path:      path,
		FileName:  "pull.txt",
		MimeType:  "text/plain",
		SizeBytes: int64(len("pull body")),
		SHA256:    "",
		ExpiresAt: time.Now().Add(time.Minute),
	})
	t.Cleanup(func() { relay.uploads.remove("upload_ok") })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/pull/upload_ok?ticket=bad", nil)
	relay.HandlePull(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("invalid ticket status = %d body=%s", rec.Code, rec.Body.String())
	}

	expiredPath := writePullFixture(t, "expired")
	relay.uploads.mu.Lock()
	relay.uploads.items["upload_expired"] = &pendingUpload{
		ID:        "upload_expired",
		Ticket:    "ticket_expired",
		Path:      expiredPath,
		FileName:  "expired.txt",
		MimeType:  "text/plain",
		SizeBytes: int64(len("expired")),
		ExpiresAt: time.Now().Add(-time.Second),
	}
	relay.uploads.mu.Unlock()
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/pull/upload_expired?ticket=ticket_expired", nil)
	relay.HandlePull(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expired ticket status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRelayUploadCleansPendingFileAfterDesktopError(t *testing.T) {
	db := openProxyTestDB(t)
	manager := NewManager()
	relay := NewRelay(db, manager, nil, 64<<20)
	relay.SetPublicBaseDomains("m.zenmind.cc", "wa.zenmind.cc")
	server := newUploadRelayTestServer(t, relay)
	defer server.Close()

	registration := registerUploadDesktop(t, db, "desk.m.zenmind.cc")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runFakeUploadDesktop(t, ctx, server.URL, registration.AgentToken, nil, func(stream *yamux.Stream, frame desktopBusinessRequest) {
		_ = tunnel.WriteWSFrame(stream, websocket.TextMessage, []byte(`{"ns":"ap","frame":"error","type":"upload_failed","id":"`+frame.ID+`","code":502,"msg":"upload rejected"}`))
	})
	waitForAgentToken(t, manager, registration.Token.ID)

	body, contentType := uploadMultipartBody(t, map[string]string{
		"chatId": "chat_upload",
	}, "note.txt", "text/plain", []byte("hello upload"))
	req, err := http.NewRequest(http.MethodPost, server.URL+"/api/upload", body)
	if err != nil {
		t.Fatalf("new upload request: %v", err)
	}
	req.Host = registration.Device.PublicHost
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer desktop-app-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post upload: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body=%s", resp.StatusCode, string(body))
	}
	relay.uploads.mu.Lock()
	count := len(relay.uploads.items)
	relay.uploads.mu.Unlock()
	if count != 0 {
		t.Fatalf("pending uploads not cleaned: %d", count)
	}
}

func newUploadRelayTestServer(t *testing.T, relay *Relay) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/tunnel":
			relay.HandleTunnel(w, r)
		case r.URL.Path == "/api/upload":
			relay.HandleUpload(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/pull/"):
			relay.HandlePull(w, r)
		default:
			relay.HandlePublic(w, r)
		}
	}))
}

func registerUploadDesktop(t *testing.T, db *store.DB, publicHost string) store.RegisterDesktopDeviceResult {
	t.Helper()
	result, err := db.RegisterDesktopDevice(context.Background(), store.RegisterDesktopDeviceInput{
		DeviceID:    "mac-mini",
		DeviceName:  "Mac Mini",
		OwnerUserID: "user_1",
		PublicHost:  publicHost,
	})
	if err != nil {
		t.Fatalf("register desktop: %v", err)
	}
	return result
}

func runFakeUploadDesktop(t *testing.T, ctx context.Context, relayURL, token string, resultCh chan<- fakeUploadResult, respond func(*yamux.Stream, desktopBusinessRequest)) {
	t.Helper()
	ws, _, err := websocket.DefaultDialer.DialContext(ctx, "ws"+strings.TrimPrefix(relayURL, "http")+"/tunnel", nil)
	if err != nil {
		t.Errorf("fake desktop dial: %v", err)
		return
	}
	defer ws.Close()
	open := tunnel.NewStreamRequest(tunnel.NamespaceDesktop, tunnel.FrameRequest, tunnel.TypeTunnelOpen, "tun_upload", &tunnel.StreamPayload{
		AgentToken: token,
		Client:     "zenmind-desktop",
		Capabilities: []string{
			"desktop.websocket",
		},
	})
	if err := ws.WriteJSON(open); err != nil {
		t.Errorf("fake desktop write tunnel.open: %v", err)
		return
	}
	var openResponse tunnel.StreamResponse
	if err := ws.ReadJSON(&openResponse); err != nil {
		t.Errorf("fake desktop read tunnel.open response: %v", err)
		return
	}
	session, err := yamux.Client(tunnel.NewWebSocketNetConn(ws), yamux.DefaultConfig())
	if err != nil {
		t.Errorf("fake desktop yamux: %v", err)
		return
	}
	defer session.Close()
	stream, err := session.AcceptStream()
	if err != nil {
		t.Errorf("fake desktop accept stream: %v", err)
		return
	}
	defer stream.Close()
	var request tunnel.StreamRequest
	if err := tunnel.ReadJSON(stream, &request); err != nil {
		t.Errorf("fake desktop read metadata: %v", err)
		return
	}
	if request.Type != tunnel.TypeDesktopWebSocketOpen || request.Payload == nil {
		t.Errorf("desktop metadata = %#v", request)
		return
	}
	if err := tunnel.WriteJSON(stream, tunnel.NewSuccessResponse(tunnel.NamespaceDesktop, tunnel.TypeDesktopWebSocketOpen, request.ID, &tunnel.StreamResponseData{
		StatusCode: http.StatusSwitchingProtocols,
	})); err != nil {
		t.Errorf("fake desktop write metadata response: %v", err)
		return
	}
	header, payload, err := tunnel.ReadWSFrame(stream)
	if err != nil {
		t.Errorf("fake desktop read upload frame: %v", err)
		return
	}
	if header.Type != websocket.TextMessage {
		t.Errorf("upload frame type = %d", header.Type)
		return
	}
	var frame desktopBusinessRequest
	if err := json.Unmarshal(payload, &frame); err != nil {
		t.Errorf("decode upload frame: %v payload=%s", err, string(payload))
		return
	}
	if respond != nil {
		respond(stream, frame)
		return
	}
	pullTarget, err := url.Parse(frame.Payload.Upload.URL)
	if err != nil {
		t.Errorf("parse upload pull URL: %v", err)
		return
	}
	base, _ := url.Parse(relayURL)
	originalHost := pullTarget.Host
	pullTarget.Scheme = base.Scheme
	pullTarget.Host = base.Host
	pullReq, err := http.NewRequest(http.MethodGet, pullTarget.String(), nil)
	if err != nil {
		t.Errorf("create upload pull request: %v", err)
		return
	}
	pullReq.Host = originalHost
	httpResp, err := http.DefaultClient.Do(pullReq)
	if err != nil {
		t.Errorf("fake desktop pull upload: %v", err)
		return
	}
	defer httpResp.Body.Close()
	downloaded, _ := io.ReadAll(httpResp.Body)
	if httpResp.StatusCode != http.StatusOK {
		t.Errorf("pull status = %d body=%s", httpResp.StatusCode, string(downloaded))
		return
	}
	if resultCh != nil {
		resultCh <- fakeUploadResult{OpenRequest: request, UploadRequest: frame, Downloaded: downloaded}
	}
	response := map[string]any{
		"ns":    "ap",
		"frame": "response",
		"type":  "/api/upload",
		"id":    frame.ID,
		"code":  0,
		"msg":   "success",
		"data": map[string]any{
			"requestId": frame.Payload.RequestID,
			"chatId":    frame.Payload.ChatID,
			"upload": map[string]any{
				"id":          "r01",
				"type":        "file",
				"name":        frame.Payload.Upload.Name,
				"mimeType":    frame.Payload.Upload.MimeType,
				"sizeBytes":   len(downloaded),
				"url":         "/api/resource?file=chat_upload%2Fnote.txt",
				"sha256":      frame.Payload.Upload.SHA256,
				"sandboxPath": "/workspace/" + frame.Payload.Upload.Name,
			},
		},
	}
	data, _ := json.Marshal(response)
	if err := tunnel.WriteWSFrame(stream, websocket.TextMessage, data); err != nil {
		t.Errorf("fake desktop write upload response: %v", err)
	}
}

func uploadMultipartBody(t *testing.T, fields map[string]string, fileName, mimeType string, data []byte) (*bytes.Buffer, string) {
	t.Helper()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	for key, value := range fields {
		if err := writer.WriteField(key, value); err != nil {
			t.Fatalf("write field %s: %v", key, err)
		}
	}
	if fileName != "" {
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", `form-data; name="file"; filename="`+fileName+`"`)
		if mimeType != "" {
			header.Set("Content-Type", mimeType)
		}
		part, err := writer.CreatePart(header)
		if err != nil {
			t.Fatalf("create file part: %v", err)
		}
		if _, err := part.Write(data); err != nil {
			t.Fatalf("write file part: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	return body, writer.FormDataContentType()
}

func writePullFixture(t *testing.T, value string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pull.txt")
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatalf("write pull fixture: %v", err)
	}
	return path
}
