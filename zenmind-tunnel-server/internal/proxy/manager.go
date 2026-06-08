package proxy

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/hashicorp/yamux"
)

var ErrNoAgent = errors.New("no active agent session")

type Manager struct {
	mu sync.RWMutex

	active *ActiveAgent

	totalStreams  atomic.Int64
	activeStreams atomic.Int64
}

type ActiveAgent struct {
	SessionID string
	TokenID   string
	Yamux     *yamux.Session
}

type Metrics struct {
	HasActiveAgent bool   `json:"hasActiveAgent"`
	SessionID      string `json:"sessionId,omitempty"`
	TokenID        string `json:"tokenId,omitempty"`
	TotalStreams   int64  `json:"totalStreams"`
	ActiveStreams  int64  `json:"activeStreams"`
}

func NewManager() *Manager {
	return &Manager{}
}

func (m *Manager) SetActive(agent *ActiveAgent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active != nil && m.active.Yamux != nil && !m.active.Yamux.IsClosed() {
		_ = m.active.Yamux.Close()
	}
	m.active = agent
}

func (m *Manager) Clear(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active != nil && m.active.SessionID == sessionID {
		m.active = nil
	}
}

func (m *Manager) OpenStream(ctx context.Context) (*yamux.Stream, error) {
	m.mu.RLock()
	active := m.active
	m.mu.RUnlock()
	if active == nil || active.Yamux == nil || active.Yamux.IsClosed() {
		return nil, ErrNoAgent
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
	m.mu.RLock()
	defer m.mu.RUnlock()
	metrics := Metrics{
		TotalStreams:  m.totalStreams.Load(),
		ActiveStreams: m.activeStreams.Load(),
	}
	if m.active != nil && m.active.Yamux != nil && !m.active.Yamux.IsClosed() {
		metrics.HasActiveAgent = true
		metrics.SessionID = m.active.SessionID
		metrics.TokenID = m.active.TokenID
	}
	return metrics
}
