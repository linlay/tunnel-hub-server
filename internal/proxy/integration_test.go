package proxy

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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"example.invalid/tunnel-hub-server/internal/auth"
	"example.invalid/tunnel-hub-server/internal/config"
	"example.invalid/tunnel-hub-server/internal/store"
	"example.invalid/tunnel-hub-server/internal/tunnel"
	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
)

func TestRelayAgentHTTPIntegration(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/hello" {
			t.Fatalf("unexpected upstream path: %s", r.URL.Path)
		}
		w.Header().Set("X-Upstream", "ok")
		_, _ = w.Write([]byte("hello through tunnel"))
	}))
	defer upstream.Close()

	db, manager, relayURL, stop := startTunnelPair(t, upstream.URL)
	defer stop()
	_ = db

	waitForAgent(t, manager)

	req, err := http.NewRequest(http.MethodGet, relayURL+"/hello", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Upstream") != "ok" {
		t.Fatalf("missing upstream header")
	}
}

func TestRelayAgentWebSocketIntegration(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade upstream: %v", err)
		}
		defer ws.Close()
		messageType, payload, err := ws.ReadMessage()
		if err != nil {
			t.Fatalf("read upstream ws: %v", err)
		}
		if err := ws.WriteMessage(messageType, []byte("echo:"+string(payload))); err != nil {
			t.Fatalf("write upstream ws: %v", err)
		}
	}))
	defer upstream.Close()

	_, manager, relayURL, stop := startTunnelPair(t, upstream.URL)
	defer stop()
	waitForAgent(t, manager)

	wsURL := "ws" + strings.TrimPrefix(relayURL, "http") + "/socket"
	client, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial public ws: %v", err)
	}
	defer client.Close()
	if err := client.WriteMessage(websocket.TextMessage, []byte("ping")); err != nil {
		t.Fatalf("write public ws: %v", err)
	}
	_, payload, err := client.ReadMessage()
	if err != nil {
		t.Fatalf("read public ws: %v", err)
	}
	if string(payload) != "echo:ping" {
		t.Fatalf("payload = %q", payload)
	}
}

func TestRelayRejectsInvalidTunnelToken(t *testing.T) {
	db := openProxyTestDB(t)
	manager := NewManager()
	relay := NewRelay(db, manager, nil, "example", "m.example.test", "wa.example.test", 64<<20)
	server := httptest.NewServer(http.HandlerFunc(relay.HandleTunnel))
	defer server.Close()

	for _, header := range []string{"Bearer wrong", "Basic wrong", "Bearer", ""} {
		_, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), http.Header{
			"Authorization": []string{header},
		})
		if err == nil {
			t.Fatalf("expected %q dial to fail", header)
		}
		if response == nil || response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%q response = %#v", header, response)
		}
	}
}

func TestRelayTunnelRejectsLegacyAgentTokenFrame(t *testing.T) {
	db := openProxyTestDB(t)
	manager := NewManager()
	relay := NewRelay(db, manager, nil, "example", "m.example.test", "wa.example.test", 64<<20)
	server := httptest.NewServer(http.HandlerFunc(relay.HandleTunnel))
	defer server.Close()

	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial tunnel: %v", err)
	}
	defer ws.Close()
	if err := ws.WriteJSON(map[string]any{
		"v": 1, "ns": "d", "frame": "request", "type": "tunnel.open", "id": "tun_1",
		"payload": map[string]any{"agentToken": "legacy", "deviceId": "desktop-1"},
	}); err != nil {
		t.Fatalf("write tunnel.open: %v", err)
	}
	var response tunnel.StreamResponse
	if err := ws.ReadJSON(&response); err != nil {
		t.Fatalf("read tunnel.open response: %v", err)
	}
	if response.Frame != tunnel.FrameError || response.Code != http.StatusBadRequest {
		t.Fatalf("tunnel.open response = %#v", response)
	}
}

func TestRelayDesktopIdentityAuthorization(t *testing.T) {
	tests := []struct {
		name       string
		registered bool
		userID     string
		scope      string
		invalidJWT bool
		allowScope bool
		wantCode   int
	}{
		{name: "valid", registered: true, userID: "user-1", scope: "profile tunnel", wantCode: 0},
		{name: "invalid identity", registered: true, userID: "user-1", scope: "profile tunnel", invalidJWT: true, wantCode: http.StatusUnauthorized},
		{name: "missing scope", registered: true, userID: "user-1", scope: "profile", wantCode: http.StatusForbidden},
		{name: "missing scope allowed by explicit compatibility switch", registered: true, userID: "user-1", scope: "profile", allowScope: true, wantCode: 0},
		{name: "wrong owner", registered: true, userID: "user-2", scope: "profile tunnel", wantCode: http.StatusForbidden},
		{name: "unregistered device", userID: "user-1", scope: "profile tunnel", wantCode: http.StatusForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := openProxyTestDB(t)
			manager := NewManager()
			relay := NewRelay(db, manager, nil, "example", "m.example.test", "wa.example.test", 64<<20)
			identityToken := configureProxyDesktopIdentityClaims(t, relay, tc.userID, tc.scope, time.Now().Add(time.Hour))
			relay.allowMissingTunnelScope = tc.allowScope
			if tc.invalidJWT {
				identityToken = "invalid.jwt.value"
			}
			var deviceKey string
			if tc.registered {
				registered, err := db.RegisterDesktopDevice(context.Background(), store.RegisterDesktopDeviceInput{DeviceID: "mac-lan", OwnerUserID: "user-1", PublicHost: "desk.m.example.test"})
				if err != nil {
					t.Fatalf("register desktop: %v", err)
				}
				deviceKey = registered.Device.DeviceKey
			}
			server := httptest.NewServer(http.HandlerFunc(relay.HandleTunnel))
			defer server.Close()
			ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
			if err != nil {
				t.Fatalf("dial desktop tunnel: %v", err)
			}
			defer ws.Close()
			if err := ws.WriteJSON(tunnel.NewStreamRequest(tunnel.NamespaceDesktop, tunnel.FrameRequest, tunnel.TypeTunnelOpen, "tun_identity", &tunnel.StreamPayload{IdentityToken: identityToken, DeviceID: "mac-lan", Client: "test"})); err != nil {
				t.Fatalf("write tunnel.open: %v", err)
			}
			var response tunnel.StreamResponse
			if err := ws.ReadJSON(&response); err != nil {
				t.Fatalf("read tunnel.open: %v", err)
			}
			if response.Code != tc.wantCode {
				t.Fatalf("code = %d, want %d, response=%+v", response.Code, tc.wantCode, response)
			}
			if tc.wantCode == 0 {
				session, err := yamux.Client(tunnel.NewWebSocketNetConn(ws), yamux.DefaultConfig())
				if err != nil {
					t.Fatalf("start yamux: %v", err)
				}
				defer session.Close()
				waitForDesktopConnection(t, manager, deviceKey)
			}
		})
	}
}

func TestRelayDesktopIdentityExpiryAndConnectionReplacement(t *testing.T) {
	db := openProxyTestDB(t)
	manager := NewManager()
	relay := NewRelay(db, manager, nil, "example", "m.example.test", "wa.example.test", 64<<20)
	identityToken := configureProxyDesktopIdentityClaims(t, relay, "user-1", "profile tunnel", time.Now().Add(2*time.Second))
	registered, err := db.RegisterDesktopDevice(context.Background(), store.RegisterDesktopDeviceInput{DeviceID: "mac-lan", OwnerUserID: "user-1", PublicHost: "desk.m.example.test"})
	if err != nil {
		t.Fatalf("register desktop: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(relay.HandleTunnel))
	defer server.Close()
	open := func(id string) (*websocket.Conn, *yamux.Session) {
		ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		if err := ws.WriteJSON(tunnel.NewStreamRequest(tunnel.NamespaceDesktop, tunnel.FrameRequest, tunnel.TypeTunnelOpen, id, &tunnel.StreamPayload{IdentityToken: identityToken, DeviceID: "mac-lan", Client: "test"})); err != nil {
			t.Fatalf("write open: %v", err)
		}
		var response tunnel.StreamResponse
		if err := ws.ReadJSON(&response); err != nil || response.Code != 0 {
			t.Fatalf("open response=%+v err=%v", response, err)
		}
		session, err := yamux.Client(tunnel.NewWebSocketNetConn(ws), yamux.DefaultConfig())
		if err != nil {
			t.Fatalf("yamux: %v", err)
		}
		return ws, session
	}
	firstWS, first := open("first")
	defer firstWS.Close()
	waitForDesktopConnection(t, manager, registered.Device.DeviceKey)
	firstActive, _ := manager.ActiveFor(DesktopConnectionKey(registered.Device.DeviceKey))
	secondWS, second := open("second")
	defer secondWS.Close()
	defer second.Close()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		active, ok := manager.ActiveFor(DesktopConnectionKey(registered.Device.DeviceKey))
		if ok && active.SessionID != firstActive.SessionID && first.IsClosed() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !first.IsClosed() {
		t.Fatal("old desktop connection was not replaced")
	}
	select {
	case <-second.CloseChan():
	case <-time.After(4 * time.Second):
		t.Fatal("desktop connection was not closed at identity expiry")
	}
}

func TestRelayTunnelTrustedProxyRemoteAddrPersistsToSessionAndManager(t *testing.T) {
	db := openProxyTestDB(t)
	manager := NewManager()
	relay := NewRelay(db, manager, nil, "example", "m.example.test", "wa.example.test", 64<<20)
	relay.SetTrustedProxyCIDRs("127.0.0.1/32")
	server := httptest.NewServer(http.HandlerFunc(relay.HandleTunnel))
	defer server.Close()

	raw, token := createProxyToken(t, db, "desktop")
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), http.Header{
		"Authorization":   []string{"Bearer " + raw},
		"X-Real-IP":       []string{"203.0.113.24"},
		"X-Forwarded-For": []string{"198.51.100.99, 203.0.113.24"},
	})
	if err != nil {
		t.Fatalf("dial tunnel: %v", err)
	}
	defer ws.Close()
	session, err := yamux.Client(tunnel.NewWebSocketNetConn(ws), yamux.DefaultConfig())
	if err != nil {
		t.Fatalf("start yamux client: %v", err)
	}
	defer session.Close()
	waitForAgentToken(t, manager, token.ID)

	active, ok := manager.ActiveFor(AgentConnectionKey(token.ID))
	if !ok {
		t.Fatal("active agent not found")
	}
	stored, err := db.GetAgentSession(context.Background(), active.SessionID)
	if err != nil {
		t.Fatalf("get agent session: %v", err)
	}
	if stored.RemoteAddr != "203.0.113.24" {
		t.Fatalf("stored RemoteAddr = %q", stored.RemoteAddr)
	}
	if active.RemoteAddr != "203.0.113.24" {
		t.Fatalf("active RemoteAddr = %q", active.RemoteAddr)
	}
}

func TestRelayTunnelFirstFrameMissingIdentityReturnsStandardError(t *testing.T) {
	db := openProxyTestDB(t)
	manager := NewManager()
	relay := NewRelay(db, manager, nil, "example", "m.example.test", "wa.example.test", 64<<20)
	server := httptest.NewServer(http.HandlerFunc(relay.HandleTunnel))
	defer server.Close()

	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial tunnel: %v", err)
	}
	defer ws.Close()
	if err := ws.WriteJSON(tunnel.NewStreamRequest(tunnel.NamespaceDesktop, tunnel.FrameRequest, tunnel.TypeTunnelOpen, "tun_bad", &tunnel.StreamPayload{
		DeviceID: "desktop-1",
	})); err != nil {
		t.Fatalf("write tunnel.open: %v", err)
	}
	var response tunnel.StreamResponse
	if err := ws.ReadJSON(&response); err != nil {
		t.Fatalf("read tunnel.open error: %v", err)
	}
	if response.Frame != tunnel.FrameError || response.Type != tunnel.TypeTunnelOpen || response.ID != "tun_bad" || response.Code != http.StatusBadRequest {
		t.Fatalf("tunnel.open error = %#v", response)
	}
}

func TestRelayTunnelFirstFrameMalformedOrWrongFrameReturnsStandardError(t *testing.T) {
	tests := []struct {
		name string
		send func(*testing.T, *websocket.Conn)
		want int
		id   string
		msg  string
	}{
		{
			name: "malformed json",
			send: func(t *testing.T, ws *websocket.Conn) {
				t.Helper()
				if err := ws.WriteMessage(websocket.TextMessage, []byte("{")); err != nil {
					t.Fatalf("write malformed json: %v", err)
				}
			},
			want: http.StatusBadRequest,
			msg:  "invalid tunnel.open frame",
		},
		{
			name: "wrong frame",
			send: func(t *testing.T, ws *websocket.Conn) {
				t.Helper()
				if err := ws.WriteJSON(tunnel.NewStreamRequest(tunnel.NamespaceDesktop, tunnel.FrameRequest, "wrong.open", "tun_wrong", &tunnel.StreamPayload{})); err != nil {
					t.Fatalf("write wrong frame: %v", err)
				}
			},
			want: http.StatusBadRequest,
			id:   "tun_wrong",
			msg:  "expected tunnel.open request",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := openProxyTestDB(t)
			manager := NewManager()
			relay := NewRelay(db, manager, nil, "example", "m.example.test", "wa.example.test", 64<<20)
			server := httptest.NewServer(http.HandlerFunc(relay.HandleTunnel))
			defer server.Close()

			ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
			if err != nil {
				t.Fatalf("dial tunnel: %v", err)
			}
			defer ws.Close()
			tc.send(t, ws)
			var response tunnel.StreamResponse
			if err := ws.ReadJSON(&response); err != nil {
				t.Fatalf("read tunnel.open error: %v", err)
			}
			if response.Frame != tunnel.FrameError || response.Type != tunnel.TypeTunnelOpen || response.ID != tc.id || response.Code != tc.want || response.Msg != tc.msg {
				t.Fatalf("tunnel.open error = %#v", response)
			}
		})
	}
}

func TestRelayTunnelLegacyBearerCompatibilityStartsYamux(t *testing.T) {
	db := openProxyTestDB(t)
	manager := NewManager()
	relay := NewRelay(db, manager, nil, "example", "m.example.test", "wa.example.test", 64<<20)
	server := httptest.NewServer(http.HandlerFunc(relay.HandleTunnel))
	defer server.Close()

	raw, token := createProxyToken(t, db, "legacy-agent")
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), http.Header{
		"Authorization": []string{"Bearer " + raw},
	})
	if err != nil {
		t.Fatalf("dial legacy tunnel: %v", err)
	}
	defer ws.Close()
	session, err := yamux.Client(tunnel.NewWebSocketNetConn(ws), yamux.DefaultConfig())
	if err != nil {
		t.Fatalf("start legacy yamux client: %v", err)
	}
	defer session.Close()
	waitForAgentToken(t, manager, token.ID)
}

func TestRelayRoutesToAssignedAgent(t *testing.T) {
	upstreamA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("from-agent-a"))
	}))
	defer upstreamA.Close()
	upstreamB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("from-agent-b"))
	}))
	defer upstreamB.Close()

	db := openProxyTestDB(t)
	manager := NewManager()
	relay := NewRelay(db, manager, nil, "example", "m.example.test", "wa.example.test", 64<<20)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/tunnel" {
			relay.HandleTunnel(w, r)
			return
		}
		relay.HandlePublic(w, r)
	}))
	defer server.Close()

	rawA, tokenA := createProxyToken(t, db, "agent-a")
	rawB, tokenB := createProxyToken(t, db, "agent-b")
	if _, err := db.CreateRoute(context.Background(), "a.example.test", upstreamA.URL, true, tokenA.ID); err != nil {
		t.Fatalf("create route a: %v", err)
	}
	if _, err := db.CreateRoute(context.Background(), "b.example.test", upstreamB.URL, true, tokenB.ID); err != nil {
		t.Fatalf("create route b: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runProxyAgent(ctx, server.URL, rawA)
	go runProxyAgent(ctx, server.URL, rawB)
	waitForAgentToken(t, manager, tokenA.ID)
	waitForAgentToken(t, manager, tokenB.ID)

	if body := publicRequestBody(t, server.URL, "a.example.test"); body != "from-agent-a" {
		t.Fatalf("route a body = %q", body)
	}
	if body := publicRequestBody(t, server.URL, "b.example.test"); body != "from-agent-b" {
		t.Fatalf("route b body = %q", body)
	}
}

func TestRelayDoesNotForwardUnassignedRoute(t *testing.T) {
	db := openProxyTestDB(t)
	manager := NewManager()
	relay := NewRelay(db, manager, nil, "example", "m.example.test", "wa.example.test", 64<<20)
	server := httptest.NewServer(http.HandlerFunc(relay.HandlePublic))
	defer server.Close()

	if _, err := db.CreateRoute(context.Background(), "legacy.example.test", "http://127.0.0.1:3000", true, ""); err != nil {
		t.Fatalf("create legacy route: %v", err)
	}
	req, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Host = "legacy.example.test"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func startTunnelPair(t *testing.T, targetURL string) (*store.DB, *Manager, string, context.CancelFunc) {
	t.Helper()
	db := openProxyTestDB(t)
	manager := NewManager()
	raw, token := createProxyToken(t, db, "test-agent")
	relay := NewRelay(db, manager, nil, "example", "m.example.test", "wa.example.test", 64<<20)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/tunnel" {
			relay.HandleTunnel(w, r)
			return
		}
		relay.HandlePublic(w, r)
	}))
	t.Cleanup(server.Close)
	if _, err := db.CreateRoute(context.Background(), strings.TrimPrefix(server.URL, "http://"), targetURL, true, token.ID); err != nil {
		t.Fatalf("create route: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go runProxyAgent(ctx, server.URL, raw)
	return db, manager, server.URL, cancel
}

func createProxyToken(t *testing.T, db *store.DB, name string) (string, store.TunnelToken) {
	t.Helper()
	raw, err := auth.NewToken()
	if err != nil {
		t.Fatalf("new token: %v", err)
	}
	token, err := db.CreateToken(context.Background(), name, raw)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	return raw, token
}

func configureProxyDesktopIdentity(t *testing.T, relay *Relay, userID string) string {
	return configureProxyDesktopIdentityClaims(t, relay, userID, "profile tunnel", time.Now().Add(time.Hour))
}

func configureProxyDesktopIdentityClaims(t *testing.T, relay *Relay, userID, scope string, expiresAt time.Time) string {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate identity key: %v", err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		t.Fatalf("marshal identity key: %v", err)
	}
	publicPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER})
	verifier, err := auth.NewSSOJWTVerifier(auth.SSOJWTConfig{Issuer: "https://sso.example.test", Audience: "tunnel-hub-server", PublicKeyPEM: string(publicPEM)})
	if err != nil {
		t.Fatalf("new identity verifier: %v", err)
	}
	relay.SetDesktopIdentityVerifier(verifier, false)
	header, _ := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT"})
	claims, _ := json.Marshal(map[string]any{
		"iss": "https://sso.example.test", "aud": "tunnel-hub-server", "sub": userID,
		"scope": scope, "iat": time.Now().Unix(), "exp": expiresAt.Unix(),
	})
	signed := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signed))
	signature, err := rsa.SignPKCS1v15(rand.Reader, privateKey, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign identity JWT: %v", err)
	}
	return signed + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func runProxyAgent(ctx context.Context, relayURL, token string) {
	_ = RunAgent(ctx, config.AgentConfig{
		RelayURL:          "ws" + strings.TrimPrefix(relayURL, "http") + "/tunnel",
		Token:             token,
		ReconnectInterval: 50 * time.Millisecond,
	}, nil)
}

func publicRequestBody(t *testing.T, relayURL, host string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, relayURL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Host = host
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(body)
}

func openProxyTestDB(t *testing.T) *store.DB {
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

func waitForAgent(t *testing.T, manager *Manager) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if manager.Metrics().ActiveAgentCount > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("agent did not connect")
}

func waitForAgentToken(t *testing.T, manager *Manager, tokenID string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, active := range manager.ActiveTunnels() {
			if active.Kind == ConnectionKindAgent && active.ConnectionID == tokenID {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("agent token %s did not connect", tokenID)
}

func waitForDesktopConnection(t *testing.T, manager *Manager, deviceKey string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := manager.ActiveFor(DesktopConnectionKey(deviceKey)); ok {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("desktop %s did not connect", deviceKey)
}
