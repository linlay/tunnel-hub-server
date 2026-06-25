package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
	"github.com/linlay/zenmind-tunnel-server/internal/tunnel"
)

type fakeDownloadResult struct {
	OpenRequest     tunnel.StreamRequest
	DownloadRequest desktopDownloadBusinessRequest
	Pushed          []byte
	PushURL         string
}

func TestRelayDownloadRequestsDesktopAndReturnsPushedFile(t *testing.T) {
	db := openProxyTestDB(t)
	manager := NewManager()
	relay := NewRelay(db, manager, nil, 64<<20)
	relay.SetPublicBaseDomains("m.zenmind.cc", "wa.zenmind.cc")
	server := newDownloadRelayTestServer(t, relay)
	defer server.Close()

	registration := registerUploadDesktop(t, db, "desk.m.zenmind.cc")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resultCh := make(chan fakeDownloadResult, 1)
	fileBody := []byte("hello download")
	go runFakeDownloadDesktop(t, ctx, server.URL, registration.AgentToken, resultCh, fileBody, "note.txt", "text/plain", nil)
	waitForAgentToken(t, manager, registration.Token.ID)

	requestBody := fmt.Sprintf(`{"chatId":"chat_download","requestId":"req_download","publicHost":%q,"resourceUrl":"/api/resource?file=chat_download%%2Fnote.txt"}`, registration.Device.PublicHost)
	req, err := http.NewRequest(http.MethodPost, server.URL+"/api/download", strings.NewReader(requestBody))
	if err != nil {
		t.Fatalf("new download request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer desktop-app-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post download: %v", err)
	}
	defer resp.Body.Close()
	responseBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download status = %d body=%s", resp.StatusCode, string(responseBody))
	}
	if string(responseBody) != string(fileBody) {
		t.Fatalf("downloaded body = %q", string(responseBody))
	}
	if disposition := resp.Header.Get("Content-Disposition"); !strings.Contains(disposition, "note.txt") {
		t.Fatalf("content disposition = %q", disposition)
	}
	sum := sha256.Sum256(fileBody)
	if resp.Header.Get("X-Content-SHA256") != hex.EncodeToString(sum[:]) {
		t.Fatalf("sha header = %q", resp.Header.Get("X-Content-SHA256"))
	}

	select {
	case result := <-resultCh:
		if result.OpenRequest.Payload == nil || result.OpenRequest.Payload.AuthToken != "desktop-app-token" {
			t.Fatalf("desktop auth metadata = %#v", result.OpenRequest.Payload)
		}
		if result.DownloadRequest.NS != "ap" || result.DownloadRequest.Type != "/api/download" || result.DownloadRequest.ID != "req_download" {
			t.Fatalf("download request frame = %#v", result.DownloadRequest)
		}
		if result.DownloadRequest.Payload.ChatID != "chat_download" ||
			result.DownloadRequest.Payload.ResourceURL != "/api/resource?file=chat_download%2Fnote.txt" {
			t.Fatalf("download payload = %#v", result.DownloadRequest.Payload)
		}
		if result.DownloadRequest.Payload.Download.Method != http.MethodPost ||
			!strings.Contains(result.DownloadRequest.Payload.Download.URL, "/api/download/push/") {
			t.Fatalf("download ticket = %#v", result.DownloadRequest.Payload.Download)
		}
		if string(result.Pushed) != string(fileBody) {
			t.Fatalf("pushed body = %q", string(result.Pushed))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("fake desktop did not receive download")
	}
}

func TestRelayDownloadInfersDesktopFromPublicHost(t *testing.T) {
	db := openProxyTestDB(t)
	manager := NewManager()
	relay := NewRelay(db, manager, nil, 64<<20)
	relay.SetPublicBaseDomains("m.zenmind.cc", "wa.zenmind.cc")
	server := newDownloadRelayTestServer(t, relay)
	defer server.Close()

	registration := registerUploadDesktop(t, db, "desk.m.zenmind.cc")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resultCh := make(chan fakeDownloadResult, 1)
	go runFakeDownloadDesktop(t, ctx, server.URL, registration.AgentToken, resultCh, []byte("host download"), "host.txt", "text/plain", nil)
	waitForAgentToken(t, manager, registration.Token.ID)

	req, err := http.NewRequest(http.MethodPost, server.URL+"/api/download", strings.NewReader(`{"chatId":"chat_host","resourceId":"r01"}`))
	if err != nil {
		t.Fatalf("new host download request: %v", err)
	}
	req.Host = registration.Device.PublicHost
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer desktop-app-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post host download: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "host download" {
		t.Fatalf("host download status = %d body=%s", resp.StatusCode, string(body))
	}

	select {
	case result := <-resultCh:
		push, err := url.Parse(result.DownloadRequest.Payload.Download.URL)
		if err != nil {
			t.Fatalf("parse push url: %v", err)
		}
		if push.Host != registration.Device.PublicHost {
			t.Fatalf("push host = %q", push.Host)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("fake desktop did not receive host download")
	}
}

func TestRelayDownloadRequiresFieldsAndDesktopOnline(t *testing.T) {
	db := openProxyTestDB(t)
	manager := NewManager()
	relay := NewRelay(db, manager, nil, 64<<20)
	relay.SetPublicBaseDomains("m.zenmind.cc", "wa.zenmind.cc")
	server := newDownloadRelayTestServer(t, relay)
	defer server.Close()
	registration := registerUploadDesktop(t, db, "desk.m.zenmind.cc")

	tests := []struct {
		name   string
		auth   string
		body   string
		status int
	}{
		{
			name:   "missing auth",
			body:   fmt.Sprintf(`{"chatId":"chat_1","publicHost":%q,"resourceId":"r01"}`, registration.Device.PublicHost),
			status: http.StatusUnauthorized,
		},
		{
			name:   "missing chat",
			auth:   "Bearer desktop-app-token",
			body:   fmt.Sprintf(`{"publicHost":%q,"resourceId":"r01"}`, registration.Device.PublicHost),
			status: http.StatusBadRequest,
		},
		{
			name:   "missing resource",
			auth:   "Bearer desktop-app-token",
			body:   fmt.Sprintf(`{"chatId":"chat_1","publicHost":%q}`, registration.Device.PublicHost),
			status: http.StatusBadRequest,
		},
		{
			name:   "absolute resource url",
			auth:   "Bearer desktop-app-token",
			body:   fmt.Sprintf(`{"chatId":"chat_1","publicHost":%q,"resourceUrl":"https://example.com/file.txt"}`, registration.Device.PublicHost),
			status: http.StatusBadRequest,
		},
		{
			name:   "missing public host",
			auth:   "Bearer desktop-app-token",
			body:   `{"chatId":"chat_1","resourceId":"r01"}`,
			status: http.StatusBadRequest,
		},
		{
			name:   "desktop offline",
			auth:   "Bearer desktop-app-token",
			body:   fmt.Sprintf(`{"chatId":"chat_1","publicHost":%q,"resourceId":"r01"}`, registration.Device.PublicHost),
			status: http.StatusBadGateway,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, server.URL+"/api/download", strings.NewReader(tc.body))
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			req.Header.Set("Content-Type", "application/json")
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("post download: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.status {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d want %d body=%s", resp.StatusCode, tc.status, string(body))
			}
		})
	}
}

func TestRelayDownloadPushValidatesTicketExpiryAndSize(t *testing.T) {
	db := openProxyTestDB(t)
	relay := NewRelay(db, NewManager(), nil, 4)
	server := newDownloadRelayTestServer(t, relay)
	defer server.Close()

	relay.downloads.add(&pendingDownload{
		ID:        "download_ok",
		Ticket:    "ticket_ok",
		ExpiresAt: time.Now().Add(time.Minute),
		Ready:     make(chan struct{}),
	})
	t.Cleanup(func() { relay.downloads.remove("download_ok") })

	req, err := http.NewRequest(http.MethodPost, server.URL+"/api/download/push/download_ok?ticket=bad", strings.NewReader("body"))
	if err != nil {
		t.Fatalf("new bad ticket request: %v", err)
	}
	req.Header.Set("X-File-Name", "note.txt")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post bad ticket: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("bad ticket status = %d", resp.StatusCode)
	}

	req, err = http.NewRequest(http.MethodPost, server.URL+"/api/download/push/missing?ticket=ticket", strings.NewReader("body"))
	if err != nil {
		t.Fatalf("new missing request: %v", err)
	}
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post missing: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing status = %d", resp.StatusCode)
	}

	relay.downloads.mu.Lock()
	relay.downloads.items["download_expired"] = &pendingDownload{
		ID:        "download_expired",
		Ticket:    "ticket_expired",
		ExpiresAt: time.Now().Add(-time.Minute),
		Ready:     make(chan struct{}),
	}
	relay.downloads.mu.Unlock()
	req, err = http.NewRequest(http.MethodPost, server.URL+"/api/download/push/download_expired?ticket=ticket_expired", strings.NewReader("body"))
	if err != nil {
		t.Fatalf("new expired request: %v", err)
	}
	req.Header.Set("X-File-Name", "note.txt")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post expired: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expired status = %d", resp.StatusCode)
	}

	relay.downloads.add(&pendingDownload{
		ID:        "download_big",
		Ticket:    "ticket_big",
		ExpiresAt: time.Now().Add(time.Minute),
		Ready:     make(chan struct{}),
	})
	t.Cleanup(func() { relay.downloads.remove("download_big") })
	req, err = http.NewRequest(http.MethodPost, server.URL+"/api/download/push/download_big?ticket=ticket_big", strings.NewReader("too large"))
	if err != nil {
		t.Fatalf("new big request: %v", err)
	}
	req.Header.Set("X-File-Name", "note.txt")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post big: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("big status = %d body=%s", resp.StatusCode, string(body))
	}
}

func TestRelayDownloadCleansPendingFileAfterDesktopError(t *testing.T) {
	db := openProxyTestDB(t)
	manager := NewManager()
	relay := NewRelay(db, manager, nil, 64<<20)
	relay.SetPublicBaseDomains("m.zenmind.cc", "wa.zenmind.cc")
	server := newDownloadRelayTestServer(t, relay)
	defer server.Close()
	registration := registerUploadDesktop(t, db, "desk.m.zenmind.cc")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runFakeDownloadDesktop(t, ctx, server.URL, registration.AgentToken, nil, nil, "", "", func(stream *yamux.Stream, frame desktopDownloadBusinessRequest) {
		_ = tunnel.WriteWSFrame(stream, websocket.TextMessage, []byte(`{"ns":"ap","frame":"error","type":"download_failed","id":"`+frame.ID+`","code":502,"msg":"download rejected"}`))
	})
	waitForAgentToken(t, manager, registration.Token.ID)

	body := fmt.Sprintf(`{"chatId":"chat_download","publicHost":%q,"resourceId":"r01"}`, registration.Device.PublicHost)
	req, err := http.NewRequest(http.MethodPost, server.URL+"/api/download", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new download request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer desktop-app-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post download: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		responseBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("download status = %d body=%s", resp.StatusCode, string(responseBody))
	}
	relay.downloads.mu.Lock()
	count := len(relay.downloads.items)
	relay.downloads.mu.Unlock()
	if count != 0 {
		t.Fatalf("pending downloads not cleaned: %d", count)
	}
}

func newDownloadRelayTestServer(t *testing.T, relay *Relay) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/tunnel":
			relay.HandleTunnel(w, r)
		case r.URL.Path == "/api/download":
			relay.HandleDownload(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/download/push/"):
			relay.HandleDownloadPush(w, r)
		default:
			relay.HandlePublic(w, r)
		}
	}))
}

func runFakeDownloadDesktop(t *testing.T, ctx context.Context, relayURL, token string, resultCh chan<- fakeDownloadResult, data []byte, fileName, mimeType string, respond func(*yamux.Stream, desktopDownloadBusinessRequest)) {
	t.Helper()
	ws, _, err := websocket.DefaultDialer.DialContext(ctx, "ws"+strings.TrimPrefix(relayURL, "http")+"/tunnel", nil)
	if err != nil {
		t.Errorf("fake desktop dial: %v", err)
		return
	}
	defer ws.Close()
	open := tunnel.NewStreamRequest(tunnel.NamespaceDesktop, tunnel.FrameRequest, tunnel.TypeTunnelOpen, "tun_download", &tunnel.StreamPayload{
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
		t.Errorf("fake desktop read download frame: %v", err)
		return
	}
	if header.Type != websocket.TextMessage {
		t.Errorf("download frame type = %d", header.Type)
		return
	}
	var frame desktopDownloadBusinessRequest
	if err := json.Unmarshal(payload, &frame); err != nil {
		t.Errorf("decode download frame: %v payload=%s", err, string(payload))
		return
	}
	if respond != nil {
		respond(stream, frame)
		return
	}

	pushURL := rewriteDownloadPushURL(t, frame.Payload.Download.URL, relayURL)
	pushReq, err := http.NewRequest(http.MethodPost, pushURL, bytes.NewReader(data))
	if err != nil {
		t.Errorf("fake desktop create push: %v", err)
		return
	}
	pushReq.Header.Set("X-File-Name", fileName)
	pushReq.Header.Set("Content-Type", mimeType)
	sum := sha256.Sum256(data)
	pushReq.Header.Set("X-File-SHA256", hex.EncodeToString(sum[:]))
	httpResp, err := http.DefaultClient.Do(pushReq)
	if err != nil {
		t.Errorf("fake desktop push download: %v", err)
		return
	}
	defer httpResp.Body.Close()
	pushResponse, _ := io.ReadAll(httpResp.Body)
	if httpResp.StatusCode != http.StatusOK {
		t.Errorf("push status = %d body=%s", httpResp.StatusCode, string(pushResponse))
		return
	}
	if resultCh != nil {
		resultCh <- fakeDownloadResult{OpenRequest: request, DownloadRequest: frame, Pushed: data, PushURL: pushURL}
	}
	response := map[string]any{
		"ns":    "ap",
		"frame": "response",
		"type":  "/api/download",
		"id":    frame.ID,
		"code":  0,
		"msg":   "success",
		"data": map[string]any{
			"requestId": frame.Payload.RequestID,
			"chatId":    frame.Payload.ChatID,
			"download": map[string]any{
				"id":        frame.Payload.Download.ID,
				"name":      fileName,
				"mimeType":  mimeType,
				"sizeBytes": len(data),
				"sha256":    hex.EncodeToString(sum[:]),
			},
		},
	}
	responseData, _ := json.Marshal(response)
	if err := tunnel.WriteWSFrame(stream, websocket.TextMessage, responseData); err != nil {
		t.Errorf("fake desktop write download response: %v", err)
	}
}

func rewriteDownloadPushURL(t *testing.T, rawURL, relayURL string) string {
	t.Helper()
	target, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse push url: %v", err)
	}
	base, err := url.Parse(relayURL)
	if err != nil {
		t.Fatalf("parse relay url: %v", err)
	}
	target.Scheme = base.Scheme
	target.Host = base.Host
	return target.String()
}
