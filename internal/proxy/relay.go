package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
	"github.com/linlay/zenmind-tunnel-server/internal/store"
	"github.com/linlay/zenmind-tunnel-server/internal/tunnel"
)

type Relay struct {
	DB                  *store.DB
	Manager             *Manager
	Logger              *slog.Logger
	MaxRequestBodyBytes int64
}

func NewRelay(db *store.DB, manager *Manager, logger *slog.Logger, maxRequestBodyBytes int64) *Relay {
	if logger == nil {
		logger = slog.Default()
	}
	if maxRequestBodyBytes <= 0 {
		maxRequestBodyBytes = 64 << 20
	}
	return &Relay{DB: db, Manager: manager, Logger: logger, MaxRequestBodyBytes: maxRequestBodyBytes}
}

func (r *Relay) HandleTunnel(w http.ResponseWriter, req *http.Request) {
	rawToken := bearerToken(req.Header.Get("Authorization"))
	if rawToken == "" {
		http.Error(w, "missing bearer token", http.StatusUnauthorized)
		return
	}
	token, err := r.DB.FindActiveTokenBySecret(req.Context(), rawToken)
	if err != nil {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	ws, err := upgrader.Upgrade(w, req, nil)
	if err != nil {
		r.Logger.Error("upgrade tunnel websocket", "error", err)
		return
	}

	conn := tunnel.NewWebSocketNetConn(ws)
	config := yamux.DefaultConfig()
	config.EnableKeepAlive = true
	config.KeepAliveInterval = 20 * time.Second
	session, err := yamux.Server(conn, config)
	if err != nil {
		_ = conn.Close()
		r.Logger.Error("start yamux server", "error", err)
		return
	}

	dbSession, err := r.DB.CreateAgentSession(req.Context(), token.ID, req.RemoteAddr)
	if err != nil {
		_ = session.Close()
		r.Logger.Error("create agent session", "error", err)
		return
	}
	_ = r.DB.TouchToken(context.Background(), token.ID)
	_ = r.DB.AddEvent(context.Background(), "agent.connected", "Agent connected", dbSession.ID)
	r.Manager.SetActive(&ActiveAgent{
		SessionID:   dbSession.ID,
		TokenID:     token.ID,
		RemoteAddr:  req.RemoteAddr,
		ConnectedAt: dbSession.ConnectedAt,
		Yamux:       session,
	})
	r.Logger.Info("agent connected", "session", dbSession.ID, "remote", req.RemoteAddr)

	<-session.CloseChan()

	r.Manager.Clear(dbSession.ID)
	_ = r.DB.EndAgentSession(context.Background(), dbSession.ID)
	_ = r.DB.AddEvent(context.Background(), "agent.disconnected", "Agent disconnected", dbSession.ID)
	r.Logger.Info("agent disconnected", "session", dbSession.ID)
}

func (r *Relay) HandlePublic(w http.ResponseWriter, req *http.Request) {
	route, err := r.DB.GetActiveRouteByHost(req.Context(), req.Host)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, req)
		return
	}
	if err != nil {
		r.writeGatewayError(w, "route lookup failed", err)
		return
	}
	if isWebSocketRequest(req) {
		r.handlePublicWebSocket(w, req, route)
		return
	}
	r.handlePublicHTTP(w, req, route)
}

func (r *Relay) handlePublicHTTP(w http.ResponseWriter, req *http.Request, route store.Route) {
	stream, err := r.Manager.OpenStream(req.Context(), route.TokenID)
	if errors.Is(err, ErrNoAgent) {
		http.Error(w, "assigned agent is offline", http.StatusBadGateway)
		return
	}
	if err != nil {
		r.writeGatewayError(w, "open tunnel stream failed", err)
		return
	}
	defer func() {
		_ = stream.Close()
		r.Manager.StreamClosed()
	}()

	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, r.MaxRequestBodyBytes))
	if err != nil {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}

	request := tunnel.StreamRequest{
		Kind:       tunnel.KindHTTP,
		RequestID:  requestID(),
		Method:     req.Method,
		Path:       requestURI(req),
		Host:       req.Host,
		Target:     route.TargetURL,
		Header:     tunnel.StripHopHeaders(req.Header),
		BodyLength: int64(len(body)),
	}
	if err := tunnel.WriteJSON(stream, request); err != nil {
		r.writeGatewayError(w, "write request metadata failed", err)
		return
	}
	if len(body) > 0 {
		if _, err := stream.Write(body); err != nil {
			r.writeGatewayError(w, "write request body failed", err)
			return
		}
	}

	var response tunnel.StreamResponse
	if err := tunnel.ReadJSON(stream, &response); err != nil {
		r.writeGatewayError(w, "read response metadata failed", err)
		return
	}
	if !response.OK {
		http.Error(w, response.Error, statusOr(response.StatusCode, http.StatusBadGateway))
		return
	}
	copyHeaders(w.Header(), tunnel.StripHopHeaders(response.Header))
	w.WriteHeader(statusOr(response.StatusCode, http.StatusOK))
	if response.BodyLength > 0 {
		if _, err := io.CopyN(w, stream, response.BodyLength); err != nil {
			r.Logger.Error("copy response body", "error", err)
		}
	}
}

func (r *Relay) handlePublicWebSocket(w http.ResponseWriter, req *http.Request, route store.Route) {
	stream, err := r.Manager.OpenStream(req.Context(), route.TokenID)
	if errors.Is(err, ErrNoAgent) {
		http.Error(w, "assigned agent is offline", http.StatusBadGateway)
		return
	}
	if err != nil {
		r.writeGatewayError(w, "open websocket tunnel stream failed", err)
		return
	}
	defer func() {
		_ = stream.Close()
		r.Manager.StreamClosed()
	}()

	request := tunnel.StreamRequest{
		Kind:      tunnel.KindWebSocket,
		RequestID: requestID(),
		Method:    req.Method,
		Path:      requestURI(req),
		Host:      req.Host,
		Target:    route.TargetURL,
		Header:    tunnel.StripHopHeaders(req.Header),
	}
	if err := tunnel.WriteJSON(stream, request); err != nil {
		r.writeGatewayError(w, "write websocket request metadata failed", err)
		return
	}
	var response tunnel.StreamResponse
	if err := tunnel.ReadJSON(stream, &response); err != nil {
		r.writeGatewayError(w, "read websocket response metadata failed", err)
		return
	}
	if !response.OK {
		http.Error(w, response.Error, statusOr(response.StatusCode, http.StatusBadGateway))
		return
	}

	upgrader := websocket.Upgrader{
		CheckOrigin: func(*http.Request) bool { return true },
	}
	clientWS, err := upgrader.Upgrade(w, req, response.Header)
	if err != nil {
		r.Logger.Error("upgrade public websocket", "error", err)
		return
	}
	defer clientWS.Close()

	errs := make(chan error, 2)
	go func() { errs <- tunnel.CopyWebSocketToFrames(clientWS, stream) }()
	go func() { errs <- tunnel.CopyFramesToWebSocket(stream, clientWS) }()
	err = <-errs
	r.Logger.Debug("websocket stream closed", "error", err)
}

func (r *Relay) writeGatewayError(w http.ResponseWriter, message string, err error) {
	r.Logger.Error(message, "error", err)
	http.Error(w, message, http.StatusBadGateway)
}

func bearerToken(header string) string {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(header, prefix))
}

func isWebSocketRequest(req *http.Request) bool {
	return strings.EqualFold(req.Header.Get("Upgrade"), "websocket") &&
		strings.Contains(strings.ToLower(req.Header.Get("Connection")), "upgrade")
}

func copyHeaders(dst, src http.Header) {
	for key, values := range src {
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func requestURI(req *http.Request) string {
	if req.URL == nil {
		return "/"
	}
	if req.URL.RequestURI() == "" {
		return "/"
	}
	return req.URL.RequestURI()
}

func statusOr(status, fallback int) int {
	if status == 0 {
		return fallback
	}
	return status
}

func requestID() string {
	return fmt.Sprintf("req_%d", time.Now().UTC().UnixNano())
}
