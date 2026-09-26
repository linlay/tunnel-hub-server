package proxy

import (
	"context"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/yamux"
)

var ErrNoTunnel = errors.New("no active tunnel session")

type ConnectionKind string

const (
	ConnectionKindAgent   ConnectionKind = "agent"
	ConnectionKindDesktop ConnectionKind = "desktop"
)

type ConnectionKey struct {
	Kind ConnectionKind
	ID   string
}

func AgentConnectionKey(tokenID string) ConnectionKey {
	return ConnectionKey{Kind: ConnectionKindAgent, ID: tokenID}
}

func DesktopConnectionKey(deviceKey string) ConnectionKey {
	return ConnectionKey{Kind: ConnectionKindDesktop, ID: deviceKey}
}

type Manager struct {
	mu sync.RWMutex

	active       map[ConnectionKey]*ActiveTunnel
	keyBySession map[string]ConnectionKey

	totalStreams  atomic.Int64
	activeStreams atomic.Int64
}

type ActiveTunnel struct {
	SessionID   string
	Key         ConnectionKey
	RemoteAddr  string
	ConnectedAt time.Time
	Yamux       *yamux.Session
}

type ActiveTunnelMetric struct {
	SessionID    string         `json:"sessionId"`
	Kind         ConnectionKind `json:"kind"`
	ConnectionID string         `json:"connectionId"`
	RemoteAddr   string         `json:"remoteAddr"`
	ConnectedAt  time.Time      `json:"connectedAt"`
}

type Metrics struct {
	TotalStreams       int64                `json:"totalStreams"`
	ActiveStreams      int64                `json:"activeStreams"`
	ActiveAgentCount   int                  `json:"activeAgentCount"`
	ActiveDesktopCount int                  `json:"activeDesktopCount"`
	ActiveTunnels      []ActiveTunnelMetric `json:"activeTunnels"`
}

func NewManager() *Manager {
	return &Manager{
		active:       make(map[ConnectionKey]*ActiveTunnel),
		keyBySession: make(map[string]ConnectionKey),
	}
}

func (m *Manager) SetActive(tunnel *ActiveTunnel) {
	m.mu.Lock()
	if m.active == nil {
		m.active = make(map[ConnectionKey]*ActiveTunnel)
	}
	if m.keyBySession == nil {
		m.keyBySession = make(map[string]ConnectionKey)
	}
	old := m.active[tunnel.Key]
	if old != nil {
		delete(m.keyBySession, old.SessionID)
	}
	m.active[tunnel.Key] = tunnel
	m.keyBySession[tunnel.SessionID] = tunnel.Key
	m.mu.Unlock()

	if old != nil && old.Yamux != nil && !old.Yamux.IsClosed() {
		_ = old.Yamux.Close()
	}
}

func (m *Manager) Clear(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key, ok := m.keyBySession[sessionID]
	if !ok {
		return
	}
	delete(m.keyBySession, sessionID)
	if active := m.active[key]; active != nil && active.SessionID == sessionID {
		delete(m.active, key)
	}
}

func (m *Manager) CloseSession(sessionID string) error {
	m.mu.RLock()
	key, ok := m.keyBySession[sessionID]
	active := m.active[key]
	m.mu.RUnlock()
	if !ok || active == nil || active.Yamux == nil || active.Yamux.IsClosed() {
		return ErrNoTunnel
	}
	return active.Yamux.Close()
}

func (m *Manager) OpenStream(ctx context.Context, key ConnectionKey) (*yamux.Stream, error) {
	m.mu.RLock()
	active := m.active[key]
	m.mu.RUnlock()
	if active == nil || active.Yamux == nil || active.Yamux.IsClosed() {
		return nil, ErrNoTunnel
	}
	stream, err := active.Yamux.OpenStream()
	if err != nil {
		return nil, err
	}
	m.totalStreams.Add(1)
	m.activeStreams.Add(1)
	go func() {
		<-ctx.Done()
		_ = stream.Close()
	}()
	return stream, nil
}

func (m *Manager) StreamClosed() {
	m.activeStreams.Add(-1)
}

func (m *Manager) Metrics() Metrics {
	metrics := Metrics{
		TotalStreams:  m.totalStreams.Load(),
		ActiveStreams: m.activeStreams.Load(),
	}
	metrics.ActiveTunnels = m.ActiveTunnels()
	for _, active := range metrics.ActiveTunnels {
		switch active.Kind {
		case ConnectionKindAgent:
			metrics.ActiveAgentCount++
		case ConnectionKindDesktop:
			metrics.ActiveDesktopCount++
		}
	}
	return metrics
}

func (m *Manager) ActiveTunnels() []ActiveTunnelMetric {
	m.mu.RLock()
	defer m.mu.RUnlock()
	tunnels := make([]ActiveTunnelMetric, 0, len(m.active))
	for _, active := range m.active {
		if active == nil || active.Yamux == nil || active.Yamux.IsClosed() {
			continue
		}
		tunnels = append(tunnels, ActiveTunnelMetric{
			SessionID:    active.SessionID,
			Kind:         active.Key.Kind,
			ConnectionID: active.Key.ID,
			RemoteAddr:   active.RemoteAddr,
			ConnectedAt:  active.ConnectedAt,
		})
	}
	sort.Slice(tunnels, func(i, j int) bool {
		if tunnels[i].Kind == tunnels[j].Kind {
			return tunnels[i].ConnectionID < tunnels[j].ConnectionID
		}
		return tunnels[i].Kind < tunnels[j].Kind
	})
	return tunnels
}

func (m *Manager) ActiveFor(key ConnectionKey) (ActiveTunnelMetric, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	active := m.active[key]
	if active == nil || active.Yamux == nil || active.Yamux.IsClosed() {
		return ActiveTunnelMetric{}, false
	}
	return ActiveTunnelMetric{
		SessionID:    active.SessionID,
		Kind:         active.Key.Kind,
		ConnectionID: active.Key.ID,
		RemoteAddr:   active.RemoteAddr,
		ConnectedAt:  active.ConnectedAt,
	}, true
}
