package proxy

import (
	"context"
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

func startTunnelPair(t *testing.T, targetURL string) (*store.DB, *Manager, string, context.CancelFunc) {
	t.Helper()
	db := openProxyTestDB(t)
	manager := NewManager()
	raw, err := auth.NewToken()
	if err != nil {
		t.Fatalf("new token: %v", err)
	}
	if _, err := db.CreateToken(context.Background(), "test-agent", raw); err != nil {
		t.Fatalf("create token: %v", err)
	}
	relay := NewRelay(db, manager, nil, 64<<20)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/tunnel" {
			relay.HandleTunnel(w, r)
			return
		}
		relay.HandlePublic(w, r)
	}))
	t.Cleanup(server.Close)
	if _, err := db.CreateRoute(context.Background(), strings.TrimPrefix(server.URL, "http://"), targetURL, true); err != nil {
		t.Fatalf("create route: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		_ = RunAgent(ctx, config.AgentConfig{
			RelayURL:          "ws" + strings.TrimPrefix(server.URL, "http") + "/tunnel",
			Token:             raw,
			ReconnectInterval: 50 * time.Millisecond,
		}, nil)
	}()
	return db, manager, server.URL, cancel
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
