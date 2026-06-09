package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/linlay/zenmind-tunnel-server/internal/auth"
	"github.com/linlay/zenmind-tunnel-server/internal/config"
	"github.com/linlay/zenmind-tunnel-server/internal/store"
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
	relay := NewRelay(db, manager, nil, 64<<20)
	server := httptest.NewServer(http.HandlerFunc(relay.HandleTunnel))
	defer server.Close()

	_, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), http.Header{
		"Authorization": []string{"Bearer wrong"},
	})
	if err == nil {
		t.Fatal("expected invalid token dial to fail")
	}
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
	relay := NewRelay(db, manager, nil, 64<<20)
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
	relay := NewRelay(db, manager, nil, 64<<20)
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
	relay := NewRelay(db, manager, nil, 64<<20)
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
		if manager.Metrics().HasActiveAgent {
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
		for _, agent := range manager.ActiveAgents() {
			if agent.TokenID == tokenID {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("agent token %s did not connect", tokenID)
}
