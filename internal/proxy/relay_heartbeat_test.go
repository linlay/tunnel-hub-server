package proxy

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"example.invalid/tunnel-hub-server/internal/tunnel"
	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
)

func TestTunnelHeartbeatPongKeepsYamuxSessionUsable(t *testing.T) {
	const requestPayload = "through-heartbeat"

	serverResult := make(chan error, 1)
	heartbeatResult := make(chan error, 1)
	pongReceived := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ws, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, req, nil)
		if err != nil {
			serverResult <- fmt.Errorf("upgrade websocket: %w", err)
			return
		}
		defer ws.Close()
		ws.SetPongHandler(func(payload string) error {
			select {
			case pongReceived <- payload:
			default:
			}
			return nil
		})

		session, err := yamux.Server(tunnel.NewWebSocketNetConn(ws))
		if err != nil {
			serverResult <- fmt.Errorf("start yamux server: %w", err)
			return
		}
		go func() {
			heartbeatResult <- runTunnelHeartbeat(ws, session, 10*time.Millisecond, 100*time.Millisecond)
		}()

		stream, err := session.AcceptStream()
		if err != nil {
			serverResult <- fmt.Errorf("accept stream: %w", err)
			return
		}
		payload := make([]byte, len(requestPayload))
		if _, err := io.ReadFull(stream, payload); err != nil {
			serverResult <- fmt.Errorf("read stream: %w", err)
			return
		}
		if _, err := stream.Write([]byte("echo:" + string(payload))); err != nil {
			serverResult <- fmt.Errorf("write stream: %w", err)
			return
		}
		_ = session.Close()
		if err := <-heartbeatResult; err != nil {
			serverResult <- fmt.Errorf("stop heartbeat: %w", err)
			return
		}
		serverResult <- nil
	}))
	defer server.Close()

	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	pingReceived := make(chan string, 1)
	ws.SetPingHandler(func(payload string) error {
		select {
		case pingReceived <- payload:
		default:
		}
		return ws.WriteControl(websocket.PongMessage, []byte(payload), time.Now().Add(time.Second))
	})

	session, err := yamux.Client(tunnel.NewWebSocketNetConn(ws))
	if err != nil {
		t.Fatalf("start yamux client: %v", err)
	}
	defer session.Close()

	select {
	case payload := <-pingReceived:
		if payload != "" {
			t.Fatalf("ping payload = %q, want empty", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for heartbeat ping")
	}
	select {
	case payload := <-pongReceived:
		if payload != "" {
			t.Fatalf("pong payload = %q, want empty", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for heartbeat pong")
	}

	stream, err := session.OpenStream()
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	if _, err := stream.Write([]byte(requestPayload)); err != nil {
		t.Fatalf("write stream: %v", err)
	}
	response := make([]byte, len("echo:")+len(requestPayload))
	if _, err := io.ReadFull(stream, response); err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if got, want := string(response), "echo:"+requestPayload; got != want {
		t.Fatalf("stream response = %q, want %q", got, want)
	}

	select {
	case err := <-serverResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for server session to stop")
	}
}

func TestTunnelHeartbeatFailureLogsOnceAndClosesSession(t *testing.T) {
	serverConn := make(chan *websocket.Conn, 1)
	releaseHandler := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ws, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, req, nil)
		if err != nil {
			return
		}
		serverConn <- ws
		<-releaseHandler
	}))
	defer server.Close()
	defer close(releaseHandler)

	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer client.Close()
	ws := <-serverConn
	if err := ws.Close(); err != nil {
		t.Fatalf("close server websocket: %v", err)
	}

	session, _ := newManagerTestSession(t)
	var logs bytes.Buffer
	relay := &Relay{Logger: slog.New(slog.NewJSONHandler(&logs, nil))}
	relay.monitorTunnelHeartbeat(ws, session, ActiveTunnel{
		SessionID: "desktop_session_heartbeat_failure",
		Key:       DesktopConnectionKey("device_test"),
	}, time.Millisecond, 10*time.Millisecond)
	if !session.IsClosed() {
		t.Fatal("yamux session remains open after heartbeat write failure")
	}
	if got := strings.Count(logs.String(), `"msg":"tunnel heartbeat failed"`); got != 1 {
		t.Fatalf("heartbeat failure log count = %d, logs: %s", got, logs.String())
	}
	if !strings.Contains(logs.String(), `"session":"desktop_session_heartbeat_failure"`) ||
		!strings.Contains(logs.String(), `"kind":"desktop"`) {
		t.Fatalf("heartbeat failure log is missing session or kind: %s", logs.String())
	}
}
