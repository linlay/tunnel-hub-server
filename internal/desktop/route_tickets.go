package desktop

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"time"
)

const routeTicketTTL = 2 * time.Minute
const routeTicketCapacity = 4096

type routeContext struct {
	SchemaVersion   int       `json:"schemaVersion"`
	AccountID       string    `json:"accountId"`
	SourceDeviceID  string    `json:"sourceDeviceId"`
	TargetDesktopID string    `json:"targetDesktopId"`
	RequestID       string    `json:"requestId"`
	JTI             string    `json:"jti"`
	IssuedAt        time.Time `json:"issuedAt"`
	ExpiresAt       time.Time `json:"expiresAt"`
}

type routeTicket struct {
	Context     routeContext
	SourceToken string // Memory only; rechecks the originating login session.
}

func (s *Server) pruneRouteTickets(now time.Time) {
	for digest, ticket := range s.routeTickets {
		if !ticket.Context.ExpiresAt.After(now) {
			delete(s.routeTickets, digest)
		}
	}
}

func (s *Server) createRouteTicket(ticket routeTicket) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	credential := "tr_" + base64.RawURLEncoding.EncodeToString(raw)
	s.routeTicketsMu.Lock()
	defer s.routeTicketsMu.Unlock()
	s.pruneRouteTickets(s.now())
	if len(s.routeTickets) >= routeTicketCapacity {
		return "", errors.New("route ticket capacity reached")
	}
	if s.routeTickets == nil {
		s.routeTickets = make(map[[32]byte]routeTicket)
	}
	s.routeTickets[sha256.Sum256([]byte(credential))] = ticket
	return credential, nil
}

func (s *Server) findRouteTicket(credential string) (routeTicket, bool) {
	if !strings.HasPrefix(credential, "tr_") {
		return routeTicket{}, false
	}
	s.routeTicketsMu.Lock()
	defer s.routeTicketsMu.Unlock()
	s.pruneRouteTickets(s.now())
	ticket, ok := s.routeTickets[sha256.Sum256([]byte(credential))]
	return ticket, ok
}

func (s *Server) revokeDesktopTickets(accountID, deviceID string) {
	s.routeTicketsMu.Lock()
	defer s.routeTicketsMu.Unlock()
	for digest, ticket := range s.routeTickets {
		if ticket.Context.AccountID == accountID && ticket.Context.TargetDesktopID == deviceID {
			delete(s.routeTickets, digest)
		}
	}
}
