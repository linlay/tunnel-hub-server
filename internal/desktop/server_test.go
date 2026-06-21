package desktop

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
	"github.com/linlay/zenmind-tunnel-server/internal/config"
	"github.com/linlay/zenmind-tunnel-server/internal/proxy"
	"github.com/linlay/zenmind-tunnel-server/internal/store"
	"github.com/linlay/zenmind-tunnel-server/internal/tunnel"
)

var (
	defaultDesktopJWT        string
	defaultDesktopPrivateKey *rsa.PrivateKey
)

func TestRegisterRequiresOfficialJWT(t *testing.T) {
	server, _ := newDesktopTestServer(t)
	rec := performRegister(t, server, desktopRegisterBody("mac-mini", "http://127.0.0.1:7082", false), "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing JWT status = %d, body = %s", rec.Code, rec.Body.String())
	}

	rec = performRegister(t, server, desktopRegisterBody("mac-mini", "http://127.0.0.1:7082", false), "register-secret")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("legacy secret status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestRegisterDesktopDeviceCreatesTokenAndBrokerHost(t *testing.T) {
	server, db := newDesktopTestServer(t)
	rec := performRegister(t, server, desktopRegisterBody("mac-mini", "", false), defaultDesktopJWT)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	response := decodeRegisterResponse(t, rec.Body)
	if !response.Created || response.Rotated {
		t.Fatalf("unexpected create flags: %+v", response)
	}
	assertDesktopPublicHost(t, response.PublicHost, "mac-mini")
	if response.WebSocketURL != "wss://"+response.PublicHost+"/ws" {
		t.Fatalf("webSocketUrl = %q", response.WebSocketURL)
	}
	if response.RelayURL != "wss://tunnel-hub.zenmind.cc/tunnel" {
		t.Fatalf("relayUrl = %q", response.RelayURL)
	}
	if !strings.HasPrefix(response.AgentToken, "zt_") || response.TokenID == "" {
		t.Fatalf("missing token fields: %+v", response)
	}

	if response.TargetURL != "" {
		t.Fatalf("desktop targetUrl should be empty: %+v", response)
	}
	if _, err := db.GetRouteByHost(context.Background(), response.PublicHost); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("desktop registration should not create route, got %v", err)
	}
	device, err := db.GetDesktopDeviceByPublicHost(context.Background(), response.PublicHost)
	if err != nil {
		t.Fatalf("get desktop device: %v", err)
	}
	if device.TokenID != response.TokenID || device.TargetURL != "" {
		t.Fatalf("unexpected desktop device: %+v", device)
	}
	token, err := db.FindActiveTokenBySecret(context.Background(), response.AgentToken)
	if err != nil {
		t.Fatalf("find active token: %v", err)
	}
	if token.ID != response.TokenID {
		t.Fatalf("token id = %q, want %q", token.ID, response.TokenID)
	}
}

func TestRegisterDesktopDeviceAcceptsSSOJWT(t *testing.T) {
	privateKey, publicKeyPEM := testSSOJWTKey(t)
	server, db := newDesktopTestServerWithConfig(t, config.RelayConfig{
		PublicBaseDomain:        "tunnel-hub.zenmind.cc",
		DesktopPublicBaseDomain: "m.zenmind.cc",
		SSOJWTIssuer:            "https://official.example.test",
		SSOJWTPublicKeyPEM:      publicKeyPEM,
		SSOJWTAudience:          "zenmind-tunnel-hub-server",
	})
	token := signTestSSOJWT(t, privateKey, testSSOJWTClaims{
		Issuer:   "https://official.example.test",
		Audience: "zenmind-tunnel-hub-server",
		UserID:   "42",
		Email:    "desktop@example.test",
		Role:     "user",
		Scope:    "profile tunnel",
		Expires:  time.Now().Add(time.Hour),
	})

	rec := performRegister(t, server, desktopRegisterBody("jwt-device", "", false), token)
	if rec.Code != http.StatusOK {
		t.Fatalf("JWT registration status = %d body = %s", rec.Code, rec.Body.String())
	}
	response := decodeRegisterResponse(t, rec.Body)
	assertDesktopPublicHost(t, response.PublicHost, "jwt-device")
	if response.AgentToken == "" {
		t.Fatalf("unexpected JWT registration response: %+v", response)
	}
	if _, err := db.GetDesktopDeviceByPublicHost(context.Background(), response.PublicHost); err != nil {
		t.Fatalf("get JWT registered desktop device: %v", err)
	}

	wrongAudienceToken := signTestSSOJWT(t, privateKey, testSSOJWTClaims{
		Issuer:   "https://official.example.test",
		Audience: "zenmind-market-server",
		UserID:   "42",
		Email:    "desktop@example.test",
		Role:     "user",
		Scope:    "profile tunnel",
		Expires:  time.Now().Add(time.Hour),
	})
	rec = performRegister(t, server, desktopRegisterBody("jwt-denied", "http://127.0.0.1:7082", false), wrongAudienceToken)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong audience status = %d body = %s", rec.Code, rec.Body.String())
	}

	noScopeToken := signTestSSOJWT(t, privateKey, testSSOJWTClaims{
		Issuer:   "https://official.example.test",
		Audience: "zenmind-tunnel-hub-server",
		UserID:   "42",
		Email:    "desktop@example.test",
		Role:     "user",
		Scope:    "profile market",
		Expires:  time.Now().Add(time.Hour),
	})
	rec = performRegister(t, server, desktopRegisterBody("jwt-no-scope", "http://127.0.0.1:7082", false), noScopeToken)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing scope status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func TestRegisterDesktopDeviceReusesExistingDevice(t *testing.T) {
	server, db := newDesktopTestServer(t)
	first := decodeRegisterResponse(t, performRegister(t, server, desktopRegisterBody("mac-mini", "", false), defaultDesktopJWT).Body)

	rec := performRegister(t, server, desktopRegisterBody("mac-mini", "", false), defaultDesktopJWT)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	second := decodeRegisterResponse(t, rec.Body)
	if second.Created || second.Rotated || second.AgentToken != "" {
		t.Fatalf("unexpected reuse response: %+v", second)
	}
	if second.TokenID != first.TokenID {
		t.Fatalf("token changed without rotate: %q -> %q", first.TokenID, second.TokenID)
	}
	if _, err := db.GetRouteByHost(context.Background(), second.PublicHost); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("desktop registration should not create route, got %v", err)
	}
}

func TestRegisterDesktopDeviceIgnoresLegacyDeviceSecret(t *testing.T) {
	server, db := newDesktopTestServer(t)
	firstBody := `{"deviceId":"mac-mini","deviceSecret":"old-secret","targetUrl":"http://127.0.0.1:7082","rotateToken":false}`
	first := decodeRegisterResponse(t, performRegister(t, server, firstBody, defaultDesktopJWT).Body)

	secondBody := `{"deviceId":"mac-mini","deviceSecret":"different-old-secret","targetUrl":"http://127.0.0.1:7083","rotateToken":false}`
	rec := performRegister(t, server, secondBody, defaultDesktopJWT)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	second := decodeRegisterResponse(t, rec.Body)
	if second.TokenID != first.TokenID || second.AgentToken != "" {
		t.Fatalf("legacy deviceSecret affected registration: %+v", second)
	}
	if _, err := db.GetRouteByHost(context.Background(), second.PublicHost); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("desktop registration should not create route, got %v", err)
	}
}

func TestRegisterDesktopDeviceAllowsSameDeviceIDForDifferentOwners(t *testing.T) {
	server, db := newDesktopTestServer(t)
	first := decodeRegisterResponse(t, performRegister(t, server, desktopRegisterBody("mac-mini", "", false), defaultDesktopJWT).Body)
	otherOwnerJWT := signTestSSOJWT(t, defaultDesktopPrivateKey, testSSOJWTClaims{
		Issuer:   "https://official.example.test",
		Audience: "zenmind-tunnel-hub-server",
		UserID:   "43",
		Email:    "other@example.test",
		Role:     "user",
		Scope:    "profile tunnel",
		Expires:  time.Now().Add(time.Hour),
	})

	rec := performRegister(t, server, desktopRegisterBody("mac-mini", "", false), otherOwnerJWT)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	second := decodeRegisterResponse(t, rec.Body)
	if !second.Created || second.PublicHost == first.PublicHost || second.TokenID == first.TokenID {
		t.Fatalf("different owner should get an independent registration: first=%+v second=%+v", first, second)
	}
	assertDesktopPublicHost(t, second.PublicHost, "mac-mini")
	if _, err := db.GetRouteByHost(context.Background(), first.PublicHost); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("first desktop registration should not create route, got %v", err)
	}
	if _, err := db.GetRouteByHost(context.Background(), second.PublicHost); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second desktop registration should not create route, got %v", err)
	}
}

func TestRegisterDesktopDeviceRotatesToken(t *testing.T) {
	server, db := newDesktopTestServer(t)
	first := decodeRegisterResponse(t, performRegister(t, server, desktopRegisterBody("mac-mini", "", false), defaultDesktopJWT).Body)

	rec := performRegister(t, server, desktopRegisterBody("mac-mini", "", true), defaultDesktopJWT)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	second := decodeRegisterResponse(t, rec.Body)
	if second.Created || !second.Rotated || second.AgentToken == "" {
		t.Fatalf("unexpected rotate response: %+v", second)
	}
	if second.TokenID == first.TokenID {
		t.Fatalf("token did not rotate: %q", second.TokenID)
	}
	if _, err := db.FindActiveTokenBySecret(context.Background(), first.AgentToken); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("old token should be inactive, got %v", err)
	}
	token, err := db.FindActiveTokenBySecret(context.Background(), second.AgentToken)
	if err != nil {
		t.Fatalf("new token should be active: %v", err)
	}
	if token.ID != second.TokenID {
		t.Fatalf("new token id = %q, want %q", token.ID, second.TokenID)
	}
	device, err := db.GetDesktopDeviceByPublicHost(context.Background(), second.PublicHost)
	if err != nil {
		t.Fatalf("get desktop device: %v", err)
	}
	if device.TokenID != second.TokenID {
		t.Fatalf("device token id = %q, want %q", device.TokenID, second.TokenID)
	}
}

func TestRegisterDesktopWebAppCreatesWARoute(t *testing.T) {
	server, db := newDesktopTestServer(t)
	desktop := decodeRegisterResponse(t, performRegister(t, server, desktopRegisterBody("mac-mini", "", false), defaultDesktopJWT).Body)

	rec := performRegisterWebApp(t, server, "mac-mini", "notes", `{"targetUrl":"http://127.0.0.1:5173"}`, defaultDesktopJWT)
	if rec.Code != http.StatusOK {
		t.Fatalf("webapp status = %d body = %s", rec.Code, rec.Body.String())
	}
	response := decodeWebAppResponse(t, rec.Body)
	if response.DeviceID != "mac-mini" || response.Name != "notes" {
		t.Fatalf("unexpected webapp response: %+v", response)
	}
	if !strings.HasSuffix(response.PublicHost, ".wa.zenmind.cc") {
		t.Fatalf("webapp public host = %q", response.PublicHost)
	}
	if response.PublicURL != "https://"+response.PublicHost || response.TargetURL != "http://127.0.0.1:5173" || !response.Active {
		t.Fatalf("unexpected webapp response fields: %+v", response)
	}
	route, err := db.GetActiveRouteByHost(context.Background(), response.PublicHost)
	if err != nil {
		t.Fatalf("get webapp route: %v", err)
	}
	if route.TargetURL != "http://127.0.0.1:5173" || route.TokenID != desktop.TokenID {
		t.Fatalf("unexpected webapp route: %+v", route)
	}
}

func TestRegisterDesktopWebAppRequiresTargetURL(t *testing.T) {
	server, _ := newDesktopTestServer(t)
	_ = decodeRegisterResponse(t, performRegister(t, server, desktopRegisterBody("mac-mini", "", false), defaultDesktopJWT).Body)

	rec := performRegisterWebApp(t, server, "mac-mini", "notes", `{}`, defaultDesktopJWT)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing target status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func TestDesktopPublicHostIgnoresLegacyMRoute(t *testing.T) {
	server, db := newDesktopTestServer(t)
	desktop := decodeRegisterResponse(t, performRegister(t, server, desktopRegisterBody("mac-mini", "", false), defaultDesktopJWT).Body)
	if _, err := db.CreateRoute(context.Background(), "legacy.m.zenmind.cc", "http://127.0.0.1:7083", true, desktop.TokenID); err != nil {
		t.Fatalf("create legacy desktop route: %v", err)
	}

	relay := proxy.NewRelay(db, proxy.NewManager(), nil, 64<<20)
	relay.SetPublicBaseDomains("m.zenmind.cc", "wa.zenmind.cc")
	publicServer := httptest.NewServer(http.HandlerFunc(relay.HandlePublic))
	defer publicServer.Close()

	req, err := http.NewRequest(http.MethodGet, publicServer.URL+"/ws", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Host = "legacy.m.zenmind.cc"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do public request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("legacy *.m route should be ignored, status = %d", resp.StatusCode)
	}
}

func TestDesktopPublicWebSocketOfflineReturnsGatewayError(t *testing.T) {
	server, db := newDesktopTestServer(t)
	registration := decodeRegisterResponse(t, performRegister(t, server, desktopRegisterBody("mac-mini", "", false), defaultDesktopJWT).Body)

	relay := proxy.NewRelay(db, proxy.NewManager(), nil, 64<<20)
	relay.SetPublicBaseDomains("m.zenmind.cc", "wa.zenmind.cc")
	publicServer := httptest.NewServer(http.HandlerFunc(relay.HandlePublic))
	defer publicServer.Close()

	serverURL, err := url.Parse(publicServer.URL)
	if err != nil {
		t.Fatalf("parse public server url: %v", err)
	}
	publicWSURL := "ws://" + registration.PublicHost + ":" + serverURL.Port() + "/ws"
	dialer := websocket.Dialer{NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, serverURL.Host)
	}}
	client, resp, err := dialer.Dial(publicWSURL, nil)
	if err == nil {
		_ = client.Close()
		t.Fatal("expected offline desktop dial to fail")
	}
	if resp == nil || resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("offline status = %#v, err = %v", resp, err)
	}
}

func TestDesktopRegistrationTunnelWebSocketIntegration(t *testing.T) {
	db := openDesktopTestDB(t)
	manager := proxy.NewManager()
	cfg := desktopTestConfig(t)
	relay := proxy.NewRelay(db, manager, nil, 64<<20)
	relay.SetPublicBaseDomains(cfg.DesktopPublicBaseDomain, cfg.WebAppPublicBaseDomain)
	desktopServer, err := NewServer(db, cfg, nil)
	if err != nil {
		t.Fatalf("new desktop server: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/tunnel":
			relay.HandleTunnel(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/desktop"):
			desktopServer.ServeHTTP(w, r)
		default:
			relay.HandlePublic(w, r)
		}
	}))
	defer server.Close()

	registration := postRegisterHTTP(t, server.URL, desktopRegisterBody("mac-mini", "", false))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runFakeDesktopBroker(t, ctx, server.URL, registration.AgentToken)
	waitForDesktopAgentToken(t, manager, registration.TokenID)

	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse server url: %v", err)
	}
	publicWSURL := "ws://" + registration.PublicHost + ":" + serverURL.Port() + "/ws"
	dialer := websocket.Dialer{NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, serverURL.Host)
	}}
	client, resp, err := dialer.DialContext(ctx, publicWSURL+"?token=desktop-token", http.Header{
		"Sec-WebSocket-Protocol": []string{"bearer.desktop-token"},
	})
	if err != nil {
		t.Fatalf("dial public desktop ws: %v", err)
	}
	defer client.Close()
	if resp == nil || resp.Header.Get("Sec-WebSocket-Protocol") != "bearer.desktop-token" {
		t.Fatalf("subprotocol was not negotiated through desktop broker: %#v", resp)
	}
	if err := client.WriteMessage(websocket.TextMessage, []byte("ping")); err != nil {
		t.Fatalf("write public desktop ws: %v", err)
	}
	_, payload, err := client.ReadMessage()
	if err != nil {
		t.Fatalf("read public desktop ws: %v", err)
	}
	if string(payload) != "desktop:ping" {
		t.Fatalf("payload = %q", payload)
	}
}

func TestDesktopRegistrationWebAppHTTPIntegration(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RequestURI() != "/hello?source=wa" {
			http.Error(w, "unexpected upstream path: "+r.URL.RequestURI(), http.StatusBadRequest)
			return
		}
		if r.Header.Get("X-Forwarded-Host") == "" {
			http.Error(w, "missing forwarded host", http.StatusBadRequest)
			return
		}
		w.Header().Set("X-WebApp", "ok")
		_, _ = w.Write([]byte("hello webapp"))
	}))
	defer upstream.Close()

	db := openDesktopTestDB(t)
	manager := proxy.NewManager()
	cfg := desktopTestConfig(t)
	relay := proxy.NewRelay(db, manager, nil, 64<<20)
	relay.SetPublicBaseDomains(cfg.DesktopPublicBaseDomain, cfg.WebAppPublicBaseDomain)
	desktopServer, err := NewServer(db, cfg, nil)
	if err != nil {
		t.Fatalf("new desktop server: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/tunnel":
			relay.HandleTunnel(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/desktop"):
			desktopServer.ServeHTTP(w, r)
		default:
			relay.HandlePublic(w, r)
		}
	}))
	defer server.Close()

	registration := postRegisterHTTP(t, server.URL, desktopRegisterBody("mac-mini", "", false))
	webApp := postRegisterWebAppHTTP(t, server.URL, "mac-mini", "notes", `{"targetUrl":"`+upstream.URL+`"}`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runFakeWebAppTunnelClient(t, ctx, server.URL, registration.AgentToken, handleFakeWebAppHTTPStream)
	waitForDesktopAgentToken(t, manager, registration.TokenID)

	req, err := http.NewRequest(http.MethodGet, server.URL+"/hello?source=wa", nil)
	if err != nil {
		t.Fatalf("new webapp request: %v", err)
	}
	req.Host = webApp.PublicHost
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do webapp request: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read webapp body: %v", err)
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-WebApp") != "ok" || string(body) != "hello webapp" {
		t.Fatalf("unexpected webapp response: status=%d header=%q body=%q", resp.StatusCode, resp.Header.Get("X-WebApp"), string(body))
	}
}

func TestDesktopRegistrationWebAppWebSocketIntegration(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RequestURI() != "/socket?room=1" {
			http.Error(w, "unexpected upstream path: "+r.URL.RequestURI(), http.StatusBadRequest)
			return
		}
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade upstream: %v", err)
		}
		defer ws.Close()
		messageType, payload, err := ws.ReadMessage()
		if err != nil {
			t.Fatalf("read upstream ws: %v", err)
		}
		if err := ws.WriteMessage(messageType, []byte("webapp:"+string(payload))); err != nil {
			t.Fatalf("write upstream ws: %v", err)
		}
	}))
	defer upstream.Close()

	db := openDesktopTestDB(t)
	manager := proxy.NewManager()
	cfg := desktopTestConfig(t)
	relay := proxy.NewRelay(db, manager, nil, 64<<20)
	relay.SetPublicBaseDomains(cfg.DesktopPublicBaseDomain, cfg.WebAppPublicBaseDomain)
	desktopServer, err := NewServer(db, cfg, nil)
	if err != nil {
		t.Fatalf("new desktop server: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/tunnel":
			relay.HandleTunnel(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/desktop"):
			desktopServer.ServeHTTP(w, r)
		default:
			relay.HandlePublic(w, r)
		}
	}))
	defer server.Close()

	registration := postRegisterHTTP(t, server.URL, desktopRegisterBody("mac-mini", "", false))
	webApp := postRegisterWebAppHTTP(t, server.URL, "mac-mini", "chat", `{"targetUrl":"`+upstream.URL+`"}`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runFakeWebAppTunnelClient(t, ctx, server.URL, registration.AgentToken, handleFakeWebAppWebSocketStream)
	waitForDesktopAgentToken(t, manager, registration.TokenID)

	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse server url: %v", err)
	}
	publicWSURL := "ws://" + webApp.PublicHost + ":" + serverURL.Port() + "/socket?room=1"
	dialer := websocket.Dialer{NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, serverURL.Host)
	}}
	client, _, err := dialer.DialContext(ctx, publicWSURL, nil)
	if err != nil {
		t.Fatalf("dial webapp ws: %v", err)
	}
	defer client.Close()
	if err := client.WriteMessage(websocket.TextMessage, []byte("ping")); err != nil {
		t.Fatalf("write webapp ws: %v", err)
	}
	_, payload, err := client.ReadMessage()
	if err != nil {
		t.Fatalf("read webapp ws: %v", err)
	}
	if string(payload) != "webapp:ping" {
		t.Fatalf("payload = %q", payload)
	}
}

func runFakeDesktopBroker(t *testing.T, ctx context.Context, relayURL, token string) {
	t.Helper()
	ws, _, err := websocket.DefaultDialer.DialContext(ctx, "ws"+strings.TrimPrefix(relayURL, "http")+"/tunnel", http.Header{
		"Authorization": []string{"Bearer " + token},
	})
	if err != nil {
		t.Errorf("fake desktop dial: %v", err)
		return
	}
	defer ws.Close()
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
		t.Errorf("fake desktop read request: %v", err)
		return
	}
	if request.V != tunnel.ProtocolVersion || request.NS != tunnel.NamespaceDesktop || request.Type != tunnel.TypeDesktopWebSocket {
		t.Errorf("desktop request metadata = %#v", request)
		return
	}
	if request.Public == nil || request.Public.Path != "/ws?token=desktop-token" {
		t.Errorf("request public path = %#v", request.Public)
		return
	}
	if request.Public.Headers.Get("Sec-WebSocket-Protocol") != "bearer.desktop-token" {
		t.Errorf("subprotocol metadata = %q", request.Public.Headers.Get("Sec-WebSocket-Protocol"))
		return
	}
	if err := tunnel.WriteJSON(stream, tunnel.StreamResponse{
		V:          tunnel.ProtocolVersion,
		NS:         tunnel.NamespaceDesktop,
		Type:       tunnel.TypeWebSocketAccept,
		ID:         request.ID,
		OK:         true,
		StatusCode: http.StatusSwitchingProtocols,
		Headers:    http.Header{"Sec-WebSocket-Protocol": []string{"bearer.desktop-token"}},
	}); err != nil {
		t.Errorf("fake desktop write response: %v", err)
		return
	}
	header, payload, err := tunnel.ReadWSFrame(stream)
	if err != nil {
		t.Errorf("fake desktop read frame: %v", err)
		return
	}
	if err := tunnel.WriteWSFrame(stream, header.Type, []byte("desktop:"+string(payload))); err != nil {
		t.Errorf("fake desktop write frame: %v", err)
	}
}

func runFakeWebAppTunnelClient(t *testing.T, ctx context.Context, relayURL, token string, handler func(*testing.T, *yamux.Stream)) {
	t.Helper()
	ws, _, err := websocket.DefaultDialer.DialContext(ctx, "ws"+strings.TrimPrefix(relayURL, "http")+"/tunnel", http.Header{
		"Authorization": []string{"Bearer " + token},
	})
	if err != nil {
		t.Errorf("fake webapp desktop dial: %v", err)
		return
	}
	defer ws.Close()
	session, err := yamux.Client(tunnel.NewWebSocketNetConn(ws), yamux.DefaultConfig())
	if err != nil {
		t.Errorf("fake webapp desktop yamux: %v", err)
		return
	}
	defer session.Close()
	stream, err := session.AcceptStream()
	if err != nil {
		t.Errorf("fake webapp desktop accept stream: %v", err)
		return
	}
	defer stream.Close()
	handler(t, stream)
}

func handleFakeWebAppHTTPStream(t *testing.T, stream *yamux.Stream) {
	t.Helper()
	var request tunnel.StreamRequest
	if err := tunnel.ReadJSON(stream, &request); err != nil {
		t.Errorf("fake webapp read request: %v", err)
		return
	}
	if request.V != tunnel.ProtocolVersion || request.NS != tunnel.NamespaceWebApp || request.Type != tunnel.TypeWebAppHTTPRequest {
		t.Errorf("webapp http request metadata = %#v", request)
		return
	}
	if request.Route == nil || request.Route.PublicHost == "" || request.Route.ID == "" {
		t.Errorf("missing route metadata = %#v", request.Route)
		return
	}
	if request.Upstream == nil || request.Upstream.Scheme != "http" || request.Upstream.Port == 0 {
		t.Errorf("unexpected upstream metadata = %#v", request.Upstream)
		return
	}
	var body io.ReadCloser = http.NoBody
	if request.BodyLength > 0 {
		body = io.NopCloser(io.LimitReader(stream, request.BodyLength))
	}
	outReq, err := http.NewRequest(request.Public.Method, fakeUpstreamURL(request.Upstream, request.Public.Path), body)
	if err != nil {
		_ = tunnel.WriteJSON(stream, tunnel.StreamResponse{V: tunnel.ProtocolVersion, NS: tunnel.NamespaceWebApp, Type: tunnel.TypeError, ID: request.ID, OK: false, StatusCode: http.StatusBadGateway, Error: err.Error()})
		return
	}
	outReq.Header = tunnel.StripHopHeaders(request.Public.Headers)
	outReq.Header.Set("X-Forwarded-Host", request.Public.Host)
	resp, err := http.DefaultClient.Do(outReq)
	if err != nil {
		_ = tunnel.WriteJSON(stream, tunnel.StreamResponse{V: tunnel.ProtocolVersion, NS: tunnel.NamespaceWebApp, Type: tunnel.TypeError, ID: request.ID, OK: false, StatusCode: http.StatusBadGateway, Error: err.Error()})
		return
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		_ = tunnel.WriteJSON(stream, tunnel.StreamResponse{V: tunnel.ProtocolVersion, NS: tunnel.NamespaceWebApp, Type: tunnel.TypeError, ID: request.ID, OK: false, StatusCode: http.StatusBadGateway, Error: err.Error()})
		return
	}
	if err := tunnel.WriteJSON(stream, tunnel.StreamResponse{
		V:          tunnel.ProtocolVersion,
		NS:         tunnel.NamespaceWebApp,
		Type:       tunnel.TypeWebAppHTTPResponse,
		ID:         request.ID,
		OK:         true,
		StatusCode: resp.StatusCode,
		Headers:    tunnel.StripHopHeaders(resp.Header),
		BodyLength: int64(len(responseBody)),
	}); err != nil {
		t.Errorf("fake webapp write response: %v", err)
		return
	}
	if len(responseBody) > 0 {
		if _, err := stream.Write(responseBody); err != nil {
			t.Errorf("fake webapp write body: %v", err)
		}
	}
}

func handleFakeWebAppWebSocketStream(t *testing.T, stream *yamux.Stream) {
	t.Helper()
	var request tunnel.StreamRequest
	if err := tunnel.ReadJSON(stream, &request); err != nil {
		t.Errorf("fake webapp ws read request: %v", err)
		return
	}
	if request.V != tunnel.ProtocolVersion || request.NS != tunnel.NamespaceWebApp || request.Type != tunnel.TypeWebSocketConnect {
		t.Errorf("webapp websocket request metadata = %#v", request)
		return
	}
	if request.Upstream == nil || request.Upstream.Scheme != "ws" || request.Upstream.Port == 0 {
		t.Errorf("unexpected websocket upstream metadata = %#v", request.Upstream)
		return
	}
	localWS, _, err := websocket.DefaultDialer.Dial(fakeUpstreamURL(request.Upstream, request.Public.Path), tunnel.StripWebSocketDialHeaders(request.Public.Headers))
	if err != nil {
		_ = tunnel.WriteJSON(stream, tunnel.StreamResponse{V: tunnel.ProtocolVersion, NS: tunnel.NamespaceWebApp, Type: tunnel.TypeError, ID: request.ID, OK: false, StatusCode: http.StatusBadGateway, Error: err.Error()})
		return
	}
	defer localWS.Close()
	if err := tunnel.WriteJSON(stream, tunnel.StreamResponse{
		V:          tunnel.ProtocolVersion,
		NS:         tunnel.NamespaceWebApp,
		Type:       tunnel.TypeWebSocketAccept,
		ID:         request.ID,
		OK:         true,
		StatusCode: http.StatusSwitchingProtocols,
	}); err != nil {
		t.Errorf("fake webapp ws write response: %v", err)
		return
	}
	errs := make(chan error, 2)
	go func() { errs <- tunnel.CopyWebSocketToFrames(localWS, stream) }()
	go func() { errs <- tunnel.CopyFramesToWebSocket(stream, localWS) }()
	<-errs
}

func fakeUpstreamURL(upstream *tunnel.UpstreamTarget, publicPath string) string {
	requestURL, err := url.Parse(publicPath)
	if err != nil {
		requestURL = &url.URL{Path: "/"}
	}
	basePath := strings.TrimRight(upstream.BasePath, "/")
	requestPath := "/" + strings.TrimLeft(requestURL.Path, "/")
	if requestPath == "/" {
		requestPath = ""
	}
	target := url.URL{
		Scheme:   upstream.Scheme,
		Host:     net.JoinHostPort(upstream.Host, fmt.Sprintf("%d", upstream.Port)),
		Path:     basePath + requestPath,
		RawQuery: requestURL.RawQuery,
	}
	if target.Path == "" {
		target.Path = "/"
	}
	return target.String()
}

func newDesktopTestServer(t *testing.T) (*Server, *store.DB) {
	t.Helper()
	return newDesktopTestServerWithConfig(t, desktopTestConfig(t))
}

func newDesktopTestServerWithConfig(t *testing.T, cfg config.RelayConfig) (*Server, *store.DB) {
	t.Helper()
	db := openDesktopTestDB(t)
	server, err := NewServer(db, cfg, nil)
	if err != nil {
		t.Fatalf("new desktop server: %v", err)
	}
	return server, db
}

func openDesktopTestDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func desktopTestConfig(t *testing.T) config.RelayConfig {
	t.Helper()
	privateKey, publicKeyPEM := testSSOJWTKey(t)
	defaultDesktopPrivateKey = privateKey
	defaultDesktopJWT = signTestSSOJWT(t, privateKey, testSSOJWTClaims{
		Issuer:   "https://official.example.test",
		Audience: "zenmind-tunnel-hub-server",
		UserID:   "42",
		Email:    "desktop@example.test",
		Role:     "user",
		Scope:    "profile tunnel",
		Expires:  time.Now().Add(time.Hour),
	})
	return config.RelayConfig{
		PublicBaseDomain:        "tunnel-hub.zenmind.cc",
		DesktopPublicBaseDomain: "m.zenmind.cc",
		SSOJWTIssuer:            "https://official.example.test",
		SSOJWTPublicKeyPEM:      publicKeyPEM,
		SSOJWTAudience:          "zenmind-tunnel-hub-server",
	}
}

func assertDesktopPublicHost(t *testing.T, publicHost, deviceID string) {
	t.Helper()
	if !strings.HasSuffix(publicHost, ".m.zenmind.cc") {
		t.Fatalf("publicHost = %q, want *.m.zenmind.cc", publicHost)
	}
	if strings.Contains(publicHost, deviceID) {
		t.Fatalf("publicHost %q should not contain device id %q", publicHost, deviceID)
	}
	label := strings.TrimSuffix(publicHost, ".m.zenmind.cc")
	if !strings.HasPrefix(label, "zm") || len(label) < 12 {
		t.Fatalf("publicHost label = %q, want generated zm label", label)
	}
}

func performRegister(t *testing.T, server *Server, body string, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, registerPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	return rec
}

func performRegisterWebApp(t *testing.T, server *Server, deviceID, name, body string, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/api/desktop/devices/"+deviceID+"/webapps/"+name, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	return rec
}

type testSSOJWTClaims struct {
	Issuer   string
	Audience string
	UserID   string
	Email    string
	Role     string
	Scope    string
	Expires  time.Time
}

func testSSOJWTKey(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	publicKeyDER, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	publicKeyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "PUBLIC KEY",
		Bytes: publicKeyDER,
	})
	return privateKey, string(publicKeyPEM)
}

func signTestSSOJWT(t *testing.T, privateKey *rsa.PrivateKey, claims testSSOJWTClaims) string {
	t.Helper()
	headerJSON, _ := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "test-key"})
	claimsJSON, _ := json.Marshal(map[string]any{
		"iss":     claims.Issuer,
		"sub":     "user:" + claims.UserID,
		"aud":     claims.Audience,
		"iat":     time.Now().Unix(),
		"exp":     claims.Expires.Unix(),
		"jti":     "test-jti",
		"user_id": claims.UserID,
		"email":   claims.Email,
		"name":    "Desktop User",
		"role":    claims.Role,
		"scope":   claims.Scope,
	})
	headerPart := base64.RawURLEncoding.EncodeToString(headerJSON)
	payloadPart := base64.RawURLEncoding.EncodeToString(claimsJSON)
	signedValue := headerPart + "." + payloadPart
	digest := sha256.Sum256([]byte(signedValue))
	signature, err := rsa.SignPKCS1v15(rand.Reader, privateKey, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign JWT: %v", err)
	}
	return signedValue + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func postRegisterHTTP(t *testing.T, baseURL, body string) registerResponse {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL+registerPath, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+defaultDesktopJWT)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post register: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("register status = %d, body = %s", resp.StatusCode, string(payload))
	}
	return decodeRegisterResponse(t, resp.Body)
}

func postRegisterWebAppHTTP(t *testing.T, baseURL, deviceID, name, body string) webAppResponse {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, baseURL+"/api/desktop/devices/"+deviceID+"/webapps/"+name, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new webapp request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+defaultDesktopJWT)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("put webapp: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("webapp status = %d, body = %s", resp.StatusCode, string(payload))
	}
	return decodeWebAppResponse(t, resp.Body)
}

func decodeRegisterResponse(t *testing.T, body io.Reader) registerResponse {
	t.Helper()
	var response registerResponse
	if err := json.NewDecoder(body).Decode(&response); err != nil {
		t.Fatalf("decode register response: %v", err)
	}
	return response
}

func decodeWebAppResponse(t *testing.T, body io.Reader) webAppResponse {
	t.Helper()
	var response webAppResponse
	if err := json.NewDecoder(body).Decode(&response); err != nil {
		t.Fatalf("decode webapp response: %v", err)
	}
	return response
}

func desktopRegisterBody(deviceID, targetURL string, rotateToken bool) string {
	rotate := "false"
	if rotateToken {
		rotate = "true"
	}
	return `{"deviceId":"` + deviceID + `","targetUrl":"` + targetURL + `","rotateToken":` + rotate + `}`
}

func waitForDesktopAgentToken(t *testing.T, manager *proxy.Manager, tokenID string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, agent := range manager.ActiveAgents() {
			if agent.TokenID == tokenID {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("agent token %s did not connect", tokenID)
}
