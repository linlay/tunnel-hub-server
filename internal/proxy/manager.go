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

var ErrNoAgent = errors.New("no active agent session")

type Manager struct {
	mu sync.RWMutex

	active map[string]*ActiveAgent

	totalStreams  atomic.Int64
	activeStreams atomic.Int64
}

type ActiveAgent struct {
	SessionID   string
	TokenID     string
	RemoteAddr  string
	ConnectedAt time.Time
	Yamux       *yamux.Session
}

type ActiveAgentMetric struct {
	SessionID   string    `json:"sessionId"`
	TokenID     string    `json:"tokenId"`
	RemoteAddr  string    `json:"remoteAddr"`
	ConnectedAt time.Time `json:"connectedAt"`
}

type Metrics struct {
	HasActiveAgent   bool                `json:"hasActiveAgent"`
	SessionID        string              `json:"sessionId,omitempty"`
	TokenID          string              `json:"tokenId,omitempty"`
	TotalStreams     int64               `json:"totalStreams"`
	ActiveStreams    int64               `json:"activeStreams"`
	ActiveAgentCount int                 `json:"activeAgentCount"`
	ActiveAgents     []ActiveAgentMetric `json:"activeAgents"`
}

func NewManager() *Manager {
	return &Manager{active: make(map[string]*ActiveAgent)}
}

func (m *Manager) SetActive(agent *ActiveAgent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active == nil {
		m.active = make(map[string]*ActiveAgent)
	}
	if old := m.active[agent.TokenID]; old != nil && old.Yamux != nil && !old.Yamux.IsClosed() {
		_ = old.Yamux.Close()
	}
	m.active[agent.TokenID] = agent
}

func (m *Manager) Clear(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for tokenID, active := range m.active {
		if active.SessionID == sessionID {
			delete(m.active, tokenID)
			return
		}
	}
}

func (m *Manager) CloseSession(sessionID string) error {
	m.mu.RLock()
	var session *yamux.Session
	for _, active := range m.active {
		if active != nil && active.SessionID == sessionID {
			session = active.Yamux
			break
		}
	}
	m.mu.RUnlock()
	if session == nil || session.IsClosed() {
		return ErrNoAgent
	}
	return session.Close()
}

func (m *Manager) OpenStream(ctx context.Context, tokenID string) (*yamux.Stream, error) {
	m.mu.RLock()
	active := m.active[tokenID]
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
	metrics := Metrics{
		TotalStreams:  m.totalStreams.Load(),
		ActiveStreams: m.activeStreams.Load(),
	}
	metrics.ActiveAgents = m.ActiveAgents()
	metrics.ActiveAgentCount = len(metrics.ActiveAgents)
	if metrics.ActiveAgentCount > 0 {
		metrics.HasActiveAgent = true
		metrics.SessionID = metrics.ActiveAgents[0].SessionID
		metrics.TokenID = metrics.ActiveAgents[0].TokenID
	}
	return metrics
}

func (m *Manager) ActiveAgents() []ActiveAgentMetric {
	m.mu.RLock()
	defer m.mu.RUnlock()
	agents := make([]ActiveAgentMetric, 0, len(m.active))
	for _, active := range m.active {
		if active == nil || active.Yamux == nil || active.Yamux.IsClosed() {
			continue
		}
		agents = append(agents, ActiveAgentMetric{
			SessionID:   active.SessionID,
			TokenID:     active.TokenID,
			RemoteAddr:  active.RemoteAddr,
			ConnectedAt: active.ConnectedAt,
		})
	}
	sort.Slice(agents, func(i, j int) bool {
		return agents[i].TokenID < agents[j].TokenID
	})
	return agents
}

func (m *Manager) ActiveAgentForToken(tokenID string) (ActiveAgentMetric, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	active := m.active[tokenID]
	if active == nil || active.Yamux == nil || active.Yamux.IsClosed() {
		return ActiveAgentMetric{}, false
	}
	return ActiveAgentMetric{
		SessionID:   active.SessionID,
		TokenID:     active.TokenID,
		RemoteAddr:  active.RemoteAddr,
		ConnectedAt: active.ConnectedAt,
	}, true
}
