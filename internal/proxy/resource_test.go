package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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

type fakeResourceResult struct {
	OpenRequest     tunnel.StreamRequest
	ResourceRequest desktopResourceBusinessRequest
	Pushed          []byte
}

func TestRelayResourceRequestsDesktopAndReturnsPushedFile(t *testing.T) {
	db := openProxyTestDB(t)
	manager := NewManager()
	relay := NewRelay(db, manager, nil, 64<<20)
	relay.SetPublicBaseDomains("m.zenmind.cc", "wa.zenmind.cc")
	server := newResourceRelayTestServer(t, relay)
	defer server.Close()

	registration := registerUploadDesktop(t, db, "desk.m.zenmind.cc")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resultCh := make(chan fakeResourceResult, 1)
	fileBody := []byte("hello resource")
	go runFakeResourceDesktop(t, ctx, server.URL, registration.AgentToken, resultCh, fileBody, "text/plain", false, nil)
	waitForAgentToken(t, manager, registration.Token.ID)

	req, err := http.NewRequest(http.MethodGet, server.URL+"/api/resource?file=chat_resource%2Fnote.txt", nil)
	if err != nil {
		t.Fatalf("new resource request: %v", err)
	}
	req.Host = registration.Device.PublicHost
	req.Header.Set("Authorization", "Bearer desktop-app-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get resource: %v", err)
	}
	defer resp.Body.Close()
	responseBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("resource status = %d body=%s", resp.StatusCode, string(responseBody))
	}
	if string(responseBody) != string(fileBody) {
		t.Fatalf("resource body = %q", string(responseBody))
	}
	if got := resp.Header.Get("Content-Type"); got != "text/plain" {
		t.Fatalf("content type = %q", got)
	}
	if got := resp.Header.Get("Content-Length"); got != "14" {
		t.Fatalf("content length = %q", got)
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
		if result.ResourceRequest.NS != "ap" || result.ResourceRequest.Type != "/api/resource" {
			t.Fatalf("resource request frame = %#v", result.ResourceRequest)
		}
		if result.ResourceRequest.Payload.File != "chat_resource/note.txt" {
			t.Fatalf("resource file = %q", result.ResourceRequest.Payload.File)
		}
		pushURL, err := url.Parse(result.ResourceRequest.Payload.PushURL)
		if err != nil {
			t.Fatalf("parse push URL: %v", err)
		}
		if pushURL.Host != registration.Device.PublicHost || !strings.HasPrefix(pushURL.Path, "/api/push/") {
			t.Fatalf("push URL = %q", result.ResourceRequest.Payload.PushURL)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("fake desktop did not receive resource request")
	}
}

func TestRelayResourceEnforcesDesktopHostAuthAndSafeFile(t *testing.T) {
	db := openProxyTestDB(t)
	manager := NewManager()
	relay := NewRelay(db, manager, nil, 64<<20)
	relay.SetPublicBaseDomains("m.zenmind.cc", "wa.zenmind.cc")
	registration := registerUploadDesktop(t, db, "desk.m.zenmind.cc")

	tests := []struct {
		name   string
		host   string
		auth   string
		target string
		status int
	}{
		{name: "main host", host: "tunnel-hub.zenmind.cc", auth: "desktop-token", target: "/api/resource?file=chat%2Fa.txt", status: http.StatusNotFound},
		{name: "missing token", host: registration.Device.PublicHost, target: "/api/resource?file=chat%2Fa.txt", status: http.StatusUnauthorized},
		{name: "missing file", host: registration.Device.PublicHost, auth: "desktop-token", target: "/api/resource", status: http.StatusBadRequest},
		{name: "absolute file", host: registration.Device.PublicHost, auth: "desktop-token", target: "/api/resource?file=%2Ftmp%2Fa.txt", status: http.StatusBadRequest},
		{name: "parent traversal", host: registration.Device.PublicHost, auth: "desktop-token", target: "/api/resource?file=chat%2F..%2Fa.txt", status: http.StatusBadRequest},
		{name: "not chat relative", host: registration.Device.PublicHost, auth: "desktop-token", target: "/api/resource?file=a.txt", status: http.StatusBadRequest},
		{name: "desktop offline", host: registration.Device.PublicHost, auth: "desktop-token", target: "/api/resource?file=chat%2Fa.txt", status: http.StatusBadGateway},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tc.target, nil)
			req.Host = tc.host
			if tc.auth != "" {
				req.Header.Set("Authorization", "Bearer "+tc.auth)
			}
			relay.HandleResource(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status = %d want %d body=%s", rec.Code, tc.status, rec.Body.String())
			}
		})
	}
}

func TestRelayResourcePropagatesDesktopTokenRejection(t *testing.T) {
	db := openProxyTestDB(t)
	manager := NewManager()
	relay := NewRelay(db, manager, nil, 64<<20)
	server := newResourceRelayTestServer(t, relay)
	defer server.Close()
	registration := registerUploadDesktop(t, db, "desk.m.zenmind.cc")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runFakeResourceDesktop(t, ctx, server.URL, registration.AgentToken, nil, nil, "", true, nil)
	waitForAgentToken(t, manager, registration.Token.ID)

	req, _ := http.NewRequest(http.MethodGet, server.URL+"/api/resource?file=chat%2Fa.txt", nil)
	req.Host = registration.Device.PublicHost
	req.Header.Set("Authorization", "Bearer token-for-a-different-desktop")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get resource: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body=%s", resp.StatusCode, string(body))
	}
}

func TestRelayPushValidatesTicketExpiryAndSizeWithoutFileNameHeader(t *testing.T) {
	relay := NewRelay(nil, NewManager(), nil, 4)
	relay.resources.add(&pendingResource{
		ID: "resource_ok", Ticket: "ticket_ok", FileName: "note.txt",
		ExpiresAt: time.Now().Add(time.Minute), Ready: make(chan struct{}),
	})
	t.Cleanup(func() { relay.resources.remove("resource_ok") })

	request := func(target, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
		req.Header.Set("Content-Type", "text/plain")
		relay.HandlePush(rec, req)
		return rec
	}
	if rec := request("/api/push/resource_ok?ticket=bad", "body"); rec.Code != http.StatusForbidden {
		t.Fatalf("bad ticket status = %d", rec.Code)
	}
	if rec := request("/api/push/missing?ticket=ticket", "body"); rec.Code != http.StatusNotFound {
		t.Fatalf("missing status = %d", rec.Code)
	}

	relay.resources.mu.Lock()
	relay.resources.items["resource_expired"] = &pendingResource{
		ID: "resource_expired", Ticket: "ticket_expired", FileName: "note.txt",
		ExpiresAt: time.Now().Add(-time.Minute), Ready: make(chan struct{}),
	}
	relay.resources.mu.Unlock()
	if rec := request("/api/push/resource_expired?ticket=ticket_expired", "body"); rec.Code != http.StatusNotFound {
		t.Fatalf("expired status = %d", rec.Code)
	}
	if rec := request("/api/push/resource_ok?ticket=ticket_ok", "too large"); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize status = %d body=%s", rec.Code, rec.Body.String())
	}
	if rec := request("/api/push/resource_ok?ticket=ticket_ok", "data"); rec.Code != http.StatusOK {
		t.Fatalf("push without file-name status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestWaitResourceReadyReturnsContextError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := waitResourceReady(ctx, &pendingResource{Ready: make(chan struct{})})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("wait error = %v", err)
	}
}

func TestRelayResourceCleansPendingStateAfterDesktopError(t *testing.T) {
	db := openProxyTestDB(t)
	manager := NewManager()
	relay := NewRelay(db, manager, nil, 64<<20)
	server := newResourceRelayTestServer(t, relay)
	defer server.Close()
	registration := registerUploadDesktop(t, db, "desk.m.zenmind.cc")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runFakeResourceDesktop(t, ctx, server.URL, registration.AgentToken, nil, nil, "", false, func(stream *yamux.Stream, frame desktopResourceBusinessRequest) {
		_ = tunnel.WriteWSFrame(stream, websocket.TextMessage, []byte(`{"ns":"ap","frame":"error","type":"/api/resource","id":"`+frame.ID+`","code":404,"msg":"resource not found"}`))
	})
	waitForAgentToken(t, manager, registration.Token.ID)

	req, _ := http.NewRequest(http.MethodGet, server.URL+"/api/resource?file=chat%2Fmissing.txt", nil)
	req.Host = registration.Device.PublicHost
	req.Header.Set("Authorization", "Bearer desktop-app-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get resource: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body=%s", resp.StatusCode, string(body))
	}
	relay.resources.mu.Lock()
	count := len(relay.resources.items)
	relay.resources.mu.Unlock()
	if count != 0 {
		t.Fatalf("pending resources not cleaned: %d", count)
	}
}

func newResourceRelayTestServer(t *testing.T, relay *Relay) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/tunnel":
			relay.HandleTunnel(w, r)
		case r.URL.Path == "/api/resource":
			relay.HandleResource(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/push/"):
			relay.HandlePush(w, r)
		default:
			relay.HandlePublic(w, r)
		}
	}))
}

func runFakeResourceDesktop(t *testing.T, ctx context.Context, relayURL, token string, resultCh chan<- fakeResourceResult, data []byte, mimeType string, rejectOpen bool, respond func(*yamux.Stream, desktopResourceBusinessRequest)) {
	t.Helper()
	ws, _, err := websocket.DefaultDialer.DialContext(ctx, "ws"+strings.TrimPrefix(relayURL, "http")+"/tunnel", nil)
	if err != nil {
		t.Errorf("fake desktop dial: %v", err)
		return
	}
	defer ws.Close()
	open := tunnel.NewStreamRequest(tunnel.NamespaceDesktop, tunnel.FrameRequest, tunnel.TypeTunnelOpen, "tun_resource", &tunnel.StreamPayload{
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
	if rejectOpen {
		_ = tunnel.WriteJSON(stream, tunnel.NewErrorResponse(tunnel.NamespaceDesktop, tunnel.TypeDesktopWebSocketOpen, request.ID, http.StatusUnauthorized, "token does not belong to desktop"))
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
		t.Errorf("fake desktop read resource frame: %v", err)
		return
	}
	if header.Type != websocket.TextMessage {
		t.Errorf("resource frame type = %d", header.Type)
		return
	}
	var frame desktopResourceBusinessRequest
	if err := json.Unmarshal(payload, &frame); err != nil {
		t.Errorf("decode resource frame: %v payload=%s", err, string(payload))
		return
	}
	if respond != nil {
		respond(stream, frame)
		return
	}

	pushTarget, err := url.Parse(frame.Payload.PushURL)
	if err != nil {
		t.Errorf("parse push URL: %v", err)
		return
	}
	base, _ := url.Parse(relayURL)
	originalHost := pushTarget.Host
	pushTarget.Scheme = base.Scheme
	pushTarget.Host = base.Host
	pushReq, err := http.NewRequest(http.MethodPost, pushTarget.String(), bytes.NewReader(data))
	if err != nil {
		t.Errorf("create resource push: %v", err)
		return
	}
	pushReq.Host = originalHost
	pushReq.Header.Set("Content-Type", mimeType)
	httpResp, err := http.DefaultClient.Do(pushReq)
	if err != nil {
		t.Errorf("push resource: %v", err)
		return
	}
	pushResponse, _ := io.ReadAll(httpResp.Body)
	httpResp.Body.Close()
	if httpResp.StatusCode != http.StatusOK {
		t.Errorf("push status = %d body=%s", httpResp.StatusCode, string(pushResponse))
		return
	}
	if resultCh != nil {
		resultCh <- fakeResourceResult{OpenRequest: request, ResourceRequest: frame, Pushed: data}
	}
	response := map[string]any{
		"ns": "ap", "frame": "response", "type": "/api/resource", "id": frame.ID,
		"code": 0, "msg": "success", "data": map[string]any{},
	}
	responseData, _ := json.Marshal(response)
	if err := tunnel.WriteWSFrame(stream, websocket.TextMessage, responseData); err != nil {
		t.Errorf("fake desktop write resource response: %v", err)
	}
}
