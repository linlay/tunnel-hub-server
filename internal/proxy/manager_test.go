package proxy

import (
	"net"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
)

func TestManagerKeepsIndependentAgentsByToken(t *testing.T) {
	manager := NewManager()
	sessionA, _ := newManagerTestSession(t)
	sessionB, _ := newManagerTestSession(t)

	manager.SetActive(&ActiveAgent{SessionID: "session_a", TokenID: "token_a", ConnectedAt: time.Now().UTC(), Yamux: sessionA})
	manager.SetActive(&ActiveAgent{SessionID: "session_b", TokenID: "token_b", ConnectedAt: time.Now().UTC(), Yamux: sessionB})

	metrics := manager.Metrics()
	if metrics.ActiveAgentCount != 2 {
		t.Fatalf("active agent count = %d", metrics.ActiveAgentCount)
	}
}

func TestManagerReplacesOnlySameToken(t *testing.T) {
	manager := NewManager()
	oldSession, _ := newManagerTestSession(t)
	newSession, _ := newManagerTestSession(t)

	manager.SetActive(&ActiveAgent{SessionID: "session_old", TokenID: "token_a", ConnectedAt: time.Now().UTC(), Yamux: oldSession})
	manager.SetActive(&ActiveAgent{SessionID: "session_new", TokenID: "token_a", ConnectedAt: time.Now().UTC(), Yamux: newSession})

	if !oldSession.IsClosed() {
		t.Fatal("old session should be closed after same-token replacement")
	}
	manager.Clear("session_old")
	metrics := manager.Metrics()
	if metrics.ActiveAgentCount != 1 || metrics.TokenID != "token_a" || metrics.SessionID != "session_new" {
		t.Fatalf("replacement should remain active: %+v", metrics)
	}
	manager.Clear("session_new")
	if manager.Metrics().ActiveAgentCount != 0 {
		t.Fatal("new session should be cleared")
	}
}

func newManagerTestSession(t *testing.T) (*yamux.Session, *yamux.Session) {
	t.Helper()
	left, right := net.Pipe()
	server, err := yamux.Server(left, yamux.DefaultConfig())
	if err != nil {
		t.Fatalf("start yamux server: %v", err)
	}
	client, err := yamux.Client(right, yamux.DefaultConfig())
	if err != nil {
		t.Fatalf("start yamux client: %v", err)
	}
	t.Cleanup(func() {
		_ = server.Close()
		_ = client.Close()
	})
	return server, client
}
