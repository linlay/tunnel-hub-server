package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
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
	DesktopBaseDomain   string
	WebAppBaseDomain    string
	trustedProxyCIDRs   []*net.IPNet
	uploads             *uploadStore
	downloads           *downloadStore
}

func NewRelay(db *store.DB, manager *Manager, logger *slog.Logger, maxRequestBodyBytes int64) *Relay {
	if logger == nil {
		logger = slog.Default()
	}
	if maxRequestBodyBytes <= 0 {
		maxRequestBodyBytes = 64 << 20
	}
	return &Relay{
		DB:                  db,
		Manager:             manager,
		Logger:              logger,
		MaxRequestBodyBytes: maxRequestBodyBytes,
		DesktopBaseDomain:   "m.zenmind.cc",
		WebAppBaseDomain:    "wa.zenmind.cc",
		uploads:             newUploadStore(),
		downloads:           newDownloadStore(),
	}
}

func (r *Relay) SetPublicBaseDomains(desktopBaseDomain, webAppBaseDomain string) {
	if normalized := normalizeBaseDomain(desktopBaseDomain); normalized != "" {
		r.DesktopBaseDomain = normalized
	}
	if normalized := normalizeBaseDomain(webAppBaseDomain); normalized != "" {
		r.WebAppBaseDomain = normalized
	}
}

func (r *Relay) SetTrustedProxyCIDRs(value string) {
	r.trustedProxyCIDRs = parseTrustedProxyCIDRs(value)
}

func (r *Relay) HandleTunnel(w http.ResponseWriter, req *http.Request) {
	clientRemoteAddr := r.clientRemoteAddr(req)
	rawToken := bearerToken(req.Header.Get("Authorization"))
	if rawToken != "" {
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
		r.serveTunnelSession(req, ws, token.ID, clientRemoteAddr, nil)
		return
	}

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	ws, err := upgrader.Upgrade(w, req, nil)
	if err != nil {
		r.Logger.Error("upgrade tunnel websocket", "error", err)
		return
	}
	defer ws.Close()

	var open tunnel.StreamRequest
	if err := ws.ReadJSON(&open); err != nil {
		_ = ws.WriteJSON(tunnel.NewErrorResponse(tunnel.NamespaceDesktop, tunnel.TypeTunnelOpen, "", http.StatusBadRequest, "invalid tunnel.open frame"))
		return
	}
	if open.V != tunnel.ProtocolVersion || open.NS != tunnel.NamespaceDesktop || open.Frame != tunnel.FrameRequest || open.Type != tunnel.TypeTunnelOpen || open.Payload == nil {
		_ = ws.WriteJSON(tunnel.NewErrorResponse(tunnel.NamespaceDesktop, tunnel.TypeTunnelOpen, open.ID, http.StatusBadRequest, "expected tunnel.open request"))
		return
	}
	token, err := r.DB.FindActiveTokenBySecret(req.Context(), open.Payload.AgentToken)
	if err != nil {
		_ = ws.WriteJSON(tunnel.NewErrorResponse(tunnel.NamespaceDesktop, tunnel.TypeTunnelOpen, open.ID, http.StatusUnauthorized, "invalid agent token"))
		return
	}
	dbSession, err := r.DB.CreateAgentSession(req.Context(), token.ID, clientRemoteAddr)
	if err != nil {
		r.Logger.Error("create agent session", "error", err)
		_ = ws.WriteJSON(tunnel.NewErrorResponse(tunnel.NamespaceDesktop, tunnel.TypeTunnelOpen, open.ID, http.StatusInternalServerError, "create agent session failed"))
		return
	}
	success := tunnel.NewSuccessResponse(tunnel.NamespaceDesktop, tunnel.TypeTunnelOpen, open.ID, &tunnel.StreamResponseData{
		SessionID: dbSession.ID,
		Multiplex: "yamux",
	})
	if err := ws.WriteJSON(success); err != nil {
		_ = r.DB.EndAgentSession(context.Background(), dbSession.ID)
		r.Logger.Error("write tunnel.open response", "error", err)
		return
	}
	r.serveTunnelSession(req, ws, token.ID, clientRemoteAddr, &dbSession)
}

func (r *Relay) serveTunnelSession(req *http.Request, ws *websocket.Conn, tokenID, clientRemoteAddr string, dbSession *store.AgentSession) {
	conn := tunnel.NewWebSocketNetConn(ws)
	config := yamux.DefaultConfig()
	config.EnableKeepAlive = true
	config.KeepAliveInterval = 20 * time.Second
	session, err := yamux.Server(conn, config)
	if err != nil {
		_ = conn.Close()
		if dbSession != nil {
			_ = r.DB.EndAgentSession(context.Background(), dbSession.ID)
		}
		r.Logger.Error("start yamux server", "error", err)
		return
	}

	if dbSession == nil {
		nextSession, err := r.DB.CreateAgentSession(req.Context(), tokenID, clientRemoteAddr)
		if err != nil {
			_ = session.Close()
			r.Logger.Error("create agent session", "error", err)
			return
		}
		dbSession = &nextSession
	}
	_ = r.DB.TouchToken(context.Background(), tokenID)
	_ = r.DB.AddEvent(context.Background(), "agent.connected", "Agent connected", dbSession.ID)
	r.Manager.SetActive(&ActiveAgent{
		SessionID:   dbSession.ID,
		TokenID:     tokenID,
		RemoteAddr:  clientRemoteAddr,
		ConnectedAt: dbSession.ConnectedAt,
		Yamux:       session,
	})
	r.Logger.Info("agent connected", "session", dbSession.ID, "remote", clientRemoteAddr)

	<-session.CloseChan()

	r.Manager.Clear(dbSession.ID)
	_ = r.DB.EndAgentSession(context.Background(), dbSession.ID)
	_ = r.DB.AddEvent(context.Background(), "agent.disconnected", "Agent disconnected", dbSession.ID)
	r.Logger.Info("agent disconnected", "session", dbSession.ID)
}

func (r *Relay) HandlePublic(w http.ResponseWriter, req *http.Request) {
	if isHostUnderBaseDomain(req.Host, r.DesktopBaseDomain) {
		r.handleDesktopPublic(w, req)
		return
	}
	route, err := r.DB.GetActiveRouteByHost(req.Context(), req.Host)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, req)
		return
	}
	if err != nil {
		r.writeGatewayError(w, "route lookup failed", err)
		return
	}
	if isHostUnderBaseDomain(req.Host, r.WebAppBaseDomain) {
		if isWebSocketRequest(req) {
			r.handleWebAppPublicWebSocket(w, req, route)
			return
		}
		r.handleWebAppPublicHTTP(w, req, route)
		return
	}
	if isWebSocketRequest(req) {
		r.handlePublicWebSocket(w, req, route)
		return
	}
	r.handlePublicHTTP(w, req, route)
}

func (r *Relay) handleDesktopPublic(w http.ResponseWriter, req *http.Request) {
	if !isWebSocketRequest(req) {
		http.Error(w, "desktop public endpoint requires websocket", http.StatusUpgradeRequired)
		return
	}
	device, err := r.DB.GetDesktopDeviceByPublicHost(req.Context(), req.Host)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, req)
		return
	}
	if err != nil {
		r.writeGatewayError(w, "desktop lookup failed", err)
		return
	}
	stream, err := r.Manager.OpenStream(req.Context(), device.TokenID)
	if errors.Is(err, ErrNoAgent) {
		http.Error(w, "desktop is offline", http.StatusBadGateway)
		return
	}
	if err != nil {
		r.writeGatewayError(w, "open desktop stream failed", err)
		return
	}
	defer func() {
		_ = stream.Close()
		r.Manager.StreamClosed()
	}()
	active, _ := r.Manager.ActiveAgentForToken(device.TokenID)
	var bytesIn atomic.Int64
	var bytesOut atomic.Int64
	statusCode := 0
	trafficError := ""
	defer func() {
		r.recordTrafficEvent(store.TrafficEvent{
			ObjectType: "desktop",
			PublicHost: req.Host,
			TokenID:    device.TokenID,
			SessionID:  active.SessionID,
			Kind:       "websocket",
			Method:     req.Method,
			Path:       requestURIWithoutQueryParam(req, "token"),
			StatusCode: statusCode,
			BytesIn:    bytesIn.Load(),
			BytesOut:   bytesOut.Load(),
			Error:      trafficError,
		})
	}()

	id := requestID()
	authToken, subprotocol := desktopWebSocketAuth(req)
	request := tunnel.NewStreamRequest(tunnel.NamespaceDesktop, tunnel.FrameRequest, tunnel.TypeDesktopWebSocketOpen, id, &tunnel.StreamPayload{
		AuthToken:      authToken,
		Subprotocol:    subprotocol,
		Source:         "tunnel-hub",
		ClientDeviceID: "",
		Public:         desktopPublicRequest(req, tunnel.StripWebSocketDialHeaders(req.Header)),
	})
	if err := tunnel.WriteJSON(stream, request); err != nil {
		r.writeGatewayError(w, "write desktop request metadata failed", err)
		return
	}
	var response tunnel.StreamResponse
	if err := tunnel.ReadJSON(stream, &response); err != nil {
		trafficError = err.Error()
		r.writeGatewayError(w, "read desktop response metadata failed", err)
		return
	}
	if !standardResponseOK(response, tunnel.NamespaceDesktop, tunnel.TypeDesktopWebSocketOpen) {
		statusCode = standardStreamStatus(response, http.StatusBadGateway)
		trafficError = response.Msg
		writeStandardStreamError(w, response, http.StatusBadGateway)
		return
	}
	statusCode = statusOr(tunnel.StreamResponseStatusCode(response), http.StatusSwitchingProtocols)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	clientWS, err := upgrader.Upgrade(w, req, tunnel.StreamResponseHeaders(response))
	if err != nil {
		trafficError = err.Error()
		r.Logger.Error("upgrade desktop public websocket", "error", err)
		return
	}
	defer clientWS.Close()

	errs := make(chan error, 2)
	go func() { errs <- copyWebSocketToFramesCounted(clientWS, stream, &bytesIn) }()
	go func() { errs <- copyFramesToWebSocketCounted(stream, clientWS, &bytesOut) }()
	err = <-errs
	if err != nil {
		trafficError = err.Error()
	}
	r.Logger.Debug("desktop websocket stream closed", "error", err)
}

func (r *Relay) handleWebAppPublicHTTP(w http.ResponseWriter, req *http.Request, route store.Route) {
	upstream, err := tunnel.ParseUpstreamTarget(route.TargetURL, false)
	if err != nil {
		r.writeGatewayError(w, "parse webapp upstream failed", err)
		return
	}
	stream, err := r.Manager.OpenStream(req.Context(), route.TokenID)
	if errors.Is(err, ErrNoAgent) {
		http.Error(w, "assigned desktop is offline", http.StatusBadGateway)
		return
	}
	if err != nil {
		r.writeGatewayError(w, "open webapp stream failed", err)
		return
	}
	defer func() {
		_ = stream.Close()
		r.Manager.StreamClosed()
	}()
	active, _ := r.Manager.ActiveAgentForToken(route.TokenID)
	bytesIn := int64(0)
	bytesOut := int64(0)
	statusCode := 0
	trafficError := ""
	defer func() {
		r.recordTrafficEvent(store.TrafficEvent{
			ObjectType: "webapp",
			PublicHost: req.Host,
			RouteID:    route.ID,
			TokenID:    route.TokenID,
			SessionID:  active.SessionID,
			Kind:       "http",
			Method:     req.Method,
			Path:       requestURI(req),
			StatusCode: statusCode,
			BytesIn:    bytesIn,
			BytesOut:   bytesOut,
			Error:      trafficError,
		})
	}()

	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, r.MaxRequestBodyBytes))
	if err != nil {
		trafficError = err.Error()
		statusCode = http.StatusRequestEntityTooLarge
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	bytesIn = int64(len(body))

	id := requestID()
	request := tunnel.NewStreamRequest(tunnel.NamespaceWebApp, tunnel.FrameRequest, tunnel.TypeWebAppHTTPRequest, id, &tunnel.StreamPayload{
		Public:     publicRequest(req, tunnel.StripHopHeaders(req.Header)),
		Upstream:   &upstream,
		Route:      routeMetadata(route),
		BodyLength: tunnel.Int64Ptr(int64(len(body))),
	})
	if err := tunnel.WriteJSON(stream, request); err != nil {
		r.writeGatewayError(w, "write webapp request metadata failed", err)
		return
	}
	if len(body) > 0 {
		if _, err := stream.Write(body); err != nil {
			r.writeGatewayError(w, "write webapp request body failed", err)
			return
		}
	}

	var response tunnel.StreamResponse
	if err := tunnel.ReadJSON(stream, &response); err != nil {
		trafficError = err.Error()
		r.writeGatewayError(w, "read webapp response metadata failed", err)
		return
	}
	if !standardResponseOK(response, tunnel.NamespaceWebApp, tunnel.TypeWebAppHTTPRequest) {
		statusCode = standardStreamStatus(response, http.StatusBadGateway)
		trafficError = response.Msg
		writeStandardStreamError(w, response, http.StatusBadGateway)
		return
	}
	copyHeaders(w.Header(), tunnel.StripHopHeaders(tunnel.StreamResponseHeaders(response)))
	statusCode = statusOr(tunnel.StreamResponseStatusCode(response), http.StatusOK)
	w.WriteHeader(statusCode)
	bodyLength := tunnel.StreamResponseBodyLength(response)
	bytesOut = bodyLength
	if bodyLength > 0 {
		if _, err := io.CopyN(w, stream, bodyLength); err != nil {
			trafficError = err.Error()
			r.Logger.Error("copy webapp response body", "error", err)
		}
	}
}

func (r *Relay) handleWebAppPublicWebSocket(w http.ResponseWriter, req *http.Request, route store.Route) {
	upstream, err := tunnel.ParseUpstreamTarget(route.TargetURL, true)
	if err != nil {
		r.writeGatewayError(w, "parse webapp websocket upstream failed", err)
		return
	}
	stream, err := r.Manager.OpenStream(req.Context(), route.TokenID)
	if errors.Is(err, ErrNoAgent) {
		http.Error(w, "assigned desktop is offline", http.StatusBadGateway)
		return
	}
	if err != nil {
		r.writeGatewayError(w, "open webapp websocket stream failed", err)
		return
	}
	defer func() {
		_ = stream.Close()
		r.Manager.StreamClosed()
	}()
	active, _ := r.Manager.ActiveAgentForToken(route.TokenID)
	var bytesIn atomic.Int64
	var bytesOut atomic.Int64
	statusCode := 0
	trafficError := ""
	defer func() {
		r.recordTrafficEvent(store.TrafficEvent{
			ObjectType: "webapp",
			PublicHost: req.Host,
			RouteID:    route.ID,
			TokenID:    route.TokenID,
			SessionID:  active.SessionID,
			Kind:       "websocket",
			Method:     req.Method,
			Path:       requestURI(req),
			StatusCode: statusCode,
			BytesIn:    bytesIn.Load(),
			BytesOut:   bytesOut.Load(),
			Error:      trafficError,
		})
	}()

	id := requestID()
	request := tunnel.NewStreamRequest(tunnel.NamespaceWebApp, tunnel.FrameRequest, tunnel.TypeWebSocketConnect, id, &tunnel.StreamPayload{
		Public:   publicRequest(req, tunnel.StripWebSocketDialHeaders(req.Header)),
		Upstream: &upstream,
		Route:    routeMetadata(route),
	})
	if err := tunnel.WriteJSON(stream, request); err != nil {
		r.writeGatewayError(w, "write webapp websocket request metadata failed", err)
		return
	}
	var response tunnel.StreamResponse
	if err := tunnel.ReadJSON(stream, &response); err != nil {
		trafficError = err.Error()
		r.writeGatewayError(w, "read webapp websocket response metadata failed", err)
		return
	}
	if !standardResponseOK(response, tunnel.NamespaceWebApp, tunnel.TypeWebSocketConnect) {
		statusCode = standardStreamStatus(response, http.StatusBadGateway)
		trafficError = response.Msg
		writeStandardStreamError(w, response, http.StatusBadGateway)
		return
	}
	statusCode = statusOr(tunnel.StreamResponseStatusCode(response), http.StatusSwitchingProtocols)

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	clientWS, err := upgrader.Upgrade(w, req, tunnel.StreamResponseHeaders(response))
	if err != nil {
		trafficError = err.Error()
		r.Logger.Error("upgrade webapp public websocket", "error", err)
		return
	}
	defer clientWS.Close()

	errs := make(chan error, 2)
	go func() { errs <- copyWebSocketToFramesCounted(clientWS, stream, &bytesIn) }()
	go func() { errs <- copyFramesToWebSocketCounted(stream, clientWS, &bytesOut) }()
	err = <-errs
	if err != nil {
		trafficError = err.Error()
	}
	r.Logger.Debug("webapp websocket stream closed", "error", err)
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
	copyHeaders(w.Header(), tunnel.StripHopHeaders(tunnel.StreamResponseHeaders(response)))
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
	clientWS, err := upgrader.Upgrade(w, req, tunnel.StreamResponseHeaders(response))
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

func parseTrustedProxyCIDRs(value string) []*net.IPNet {
	parts := strings.Split(value, ",")
	networks := make([]*net.IPNet, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		_, network, err := net.ParseCIDR(part)
		if err != nil {
			continue
		}
		networks = append(networks, network)
	}
	return networks
}

func (r *Relay) clientRemoteAddr(req *http.Request) string {
	remoteAddr := strings.TrimSpace(req.RemoteAddr)
	if !r.isTrustedProxyRemoteAddr(remoteAddr) {
		return remoteAddr
	}
	if ip := parseHeaderIP(req.Header.Get("X-Real-IP")); ip != "" {
		return ip
	}
	if ip := parseLastForwardedForIP(req.Header.Get("X-Forwarded-For")); ip != "" {
		return ip
	}
	return remoteAddr
}

func (r *Relay) isTrustedProxyRemoteAddr(remoteAddr string) bool {
	if len(r.trustedProxyCIDRs) == 0 {
		return false
	}
	ip := parseRemoteAddrIP(remoteAddr)
	if ip == nil {
		return false
	}
	for _, network := range r.trustedProxyCIDRs {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

func parseRemoteAddrIP(remoteAddr string) net.IP {
	host, _, err := net.SplitHostPort(strings.TrimSpace(remoteAddr))
	if err != nil {
		host = remoteAddr
	}
	return net.ParseIP(strings.Trim(strings.TrimSpace(host), "[]"))
}

func parseLastForwardedForIP(value string) string {
	parts := strings.Split(value, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		if ip := parseHeaderIP(parts[i]); ip != "" {
			return ip
		}
	}
	return ""
}

func parseHeaderIP(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(value); err == nil {
		value = host
	}
	ip := net.ParseIP(strings.Trim(value, "[]"))
	if ip == nil {
		return ""
	}
	return ip.String()
}

func normalizeBaseDomain(host string) string {
	return strings.TrimPrefix(tunnel.NormalizeHost(host), ".")
}

func isHostUnderBaseDomain(host, baseDomain string) bool {
	normalizedHost := tunnel.NormalizeHost(host)
	normalizedBase := normalizeBaseDomain(baseDomain)
	return normalizedBase != "" && (normalizedHost == normalizedBase || strings.HasSuffix(normalizedHost, "."+normalizedBase))
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

func publicRequest(req *http.Request, headers http.Header) *tunnel.PublicRequest {
	next := tunnel.NewPublicRequest(req, headers)
	return &next
}

func desktopPublicRequest(req *http.Request, headers http.Header) *tunnel.PublicRequest {
	next := tunnel.NewPublicRequest(req, headers)
	next.Path = requestURIWithoutQueryParam(req, "token")
	return &next
}

func desktopWebSocketAuth(req *http.Request) (string, string) {
	authToken := ""
	if req.URL != nil {
		authToken = strings.TrimSpace(req.URL.Query().Get("token"))
	}
	subprotocol := bearerSubprotocol(req.Header)
	if authToken == "" && subprotocol != "" {
		authToken = strings.TrimSpace(subprotocol[len("bearer."):])
	}
	return authToken, subprotocol
}

func bearerSubprotocol(header http.Header) string {
	for _, value := range header.Values("Sec-WebSocket-Protocol") {
		for _, candidate := range strings.Split(value, ",") {
			candidate = strings.TrimSpace(candidate)
			if strings.HasPrefix(strings.ToLower(candidate), "bearer.") {
				return candidate
			}
		}
	}
	return ""
}

func standardResponseOK(response tunnel.StreamResponse, ns, typ string) bool {
	return response.V == tunnel.ProtocolVersion &&
		response.NS == ns &&
		response.Frame == tunnel.FrameResponse &&
		response.Type == typ &&
		response.Code == 0
}

func writeStandardStreamError(w http.ResponseWriter, response tunnel.StreamResponse, fallbackStatus int) {
	status := standardStreamStatus(response, fallbackStatus)
	msg := response.Msg
	if msg == "" {
		msg = response.Error
	}
	if msg == "" {
		msg = http.StatusText(status)
	}
	http.Error(w, msg, status)
}

func standardStreamStatus(response tunnel.StreamResponse, fallbackStatus int) int {
	status := tunnel.StreamResponseStatusCode(response)
	if status == 0 && response.Code >= http.StatusBadRequest && response.Code <= 599 {
		status = response.Code
	}
	if status == 0 {
		status = fallbackStatus
	}
	return status
}

func (r *Relay) recordTrafficEvent(event store.TrafficEvent) {
	if r.DB == nil {
		return
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = time.Now().UTC()
	}
	if err := r.DB.RecordTrafficEvent(context.Background(), event); err != nil {
		r.Logger.Error("record traffic event", "error", err)
	}
}

func copyWebSocketToFramesCounted(ws *websocket.Conn, dst io.Writer, count *atomic.Int64) error {
	for {
		messageType, payload, err := ws.ReadMessage()
		if err != nil {
			return err
		}
		count.Add(int64(len(payload)))
		if err := tunnel.WriteWSFrame(dst, messageType, payload); err != nil {
			return err
		}
	}
}

func copyFramesToWebSocketCounted(src io.Reader, ws *websocket.Conn, count *atomic.Int64) error {
	for {
		header, payload, err := tunnel.ReadWSFrame(src)
		if err != nil {
			return err
		}
		count.Add(int64(len(payload)))
		if err := ws.WriteMessage(header.Type, payload); err != nil {
			return err
		}
	}
}

func routeMetadata(route store.Route) *tunnel.RouteMetadata {
	return &tunnel.RouteMetadata{
		ID:         route.ID,
		PublicHost: route.PublicHost,
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

func requestURIWithoutQueryParam(req *http.Request, key string) string {
	if req.URL == nil {
		return "/"
	}
	next := *req.URL
	query := next.Query()
	query.Del(key)
	next.RawQuery = query.Encode()
	if next.Path == "" {
		next.Path = "/"
	}
	if uri := next.RequestURI(); uri != "" {
		return uri
	}
	return "/"
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
