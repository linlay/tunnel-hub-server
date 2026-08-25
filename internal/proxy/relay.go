package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"example.invalid/tunnel-hub-server/internal/auth"
	"example.invalid/tunnel-hub-server/internal/store"
	"example.invalid/tunnel-hub-server/internal/tunnel"
	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
)

type Relay struct {
	DB                       *store.DB
	Manager                  *Manager
	Logger                   *slog.Logger
	BrandID                  string
	MaxRequestBodyBytes      int64
	DesktopBaseDomain        string
	WebAppBaseDomain         string
	MobileWebAppCookieSecure bool
	desktopIdentityVerifier  *auth.SSOJWTVerifier
	allowMissingTunnelScope  bool
	trustedProxyCIDRs        []*net.IPNet
	uploads                  *uploadStore
	resources                *resourceStore
}

type webAppRelayOptions struct {
	AuthToken   string
	Subprotocol string
	Source      string
	ObjectType  string
	Connection  ConnectionKey
	DeviceID    string
}

type desktopTunnelOpenFrame struct {
	V       int                      `json:"v"`
	NS      string                   `json:"ns"`
	Frame   string                   `json:"frame"`
	Type    string                   `json:"type"`
	ID      string                   `json:"id"`
	Payload desktopTunnelOpenPayload `json:"payload"`
}

type desktopTunnelOpenPayload struct {
	IdentityToken string   `json:"identityToken"`
	DeviceID      string   `json:"deviceId"`
	Client        string   `json:"client"`
	Capabilities  []string `json:"capabilities"`
}

func NewRelay(db *store.DB, manager *Manager, logger *slog.Logger, brandID, desktopBaseDomain, webAppBaseDomain string, maxRequestBodyBytes int64) *Relay {
	if logger == nil {
		logger = slog.Default()
	}
	if maxRequestBodyBytes <= 0 {
		maxRequestBodyBytes = 64 << 20
	}
	return &Relay{
		DB:                       db,
		Manager:                  manager,
		Logger:                   logger,
		BrandID:                  brandID,
		MaxRequestBodyBytes:      maxRequestBodyBytes,
		DesktopBaseDomain:        normalizeBaseDomain(desktopBaseDomain),
		WebAppBaseDomain:         normalizeBaseDomain(webAppBaseDomain),
		MobileWebAppCookieSecure: true,
		uploads:                  newUploadStore(),
		resources:                newResourceStore(),
	}
}

func (r *Relay) SetMobileWebAppCookieSecure(secure bool) {
	r.MobileWebAppCookieSecure = secure
}

func (r *Relay) mobileWebAppSessionCookieName() string {
	if r.MobileWebAppCookieSecure {
		return "__Host-" + r.BrandID + "_mobile_session"
	}
	return r.BrandID + "_mobile_session"
}

func (r *Relay) SetTrustedProxyCIDRs(value string) {
	r.trustedProxyCIDRs = parseTrustedProxyCIDRs(value)
}

func (r *Relay) SetDesktopIdentityVerifier(verifier *auth.SSOJWTVerifier, allowMissingScope bool) {
	r.desktopIdentityVerifier = verifier
	r.allowMissingTunnelScope = allowMissingScope
}

func (r *Relay) HandleTunnel(w http.ResponseWriter, req *http.Request) {
	clientRemoteAddr := r.clientRemoteAddr(req)
	authorizations, authorizationPresent := req.Header[http.CanonicalHeaderKey("Authorization")]
	if authorizationPresent {
		rawToken := ""
		if len(authorizations) == 1 {
			rawToken = bearerToken(authorizations[0])
		}
		if rawToken == "" {
			http.Error(w, "invalid authorization header", http.StatusUnauthorized)
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
		defer ws.Close()
		dbSession, err := r.DB.CreateAgentSession(req.Context(), token.ID, clientRemoteAddr)
		if err != nil {
			_ = ws.Close()
			r.Logger.Error("create agent session", "error", err)
			return
		}
		_ = r.DB.TouchToken(req.Context(), token.ID)
		r.serveTunnelSession(ws, ActiveTunnel{
			SessionID:   dbSession.ID,
			Key:         AgentConnectionKey(token.ID),
			RemoteAddr:  clientRemoteAddr,
			ConnectedAt: dbSession.ConnectedAt,
		}, time.Time{}, func() {
			_ = r.DB.EndAgentSession(context.Background(), dbSession.ID)
		})
		return
	}

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	ws, err := upgrader.Upgrade(w, req, nil)
	if err != nil {
		r.Logger.Error("upgrade tunnel websocket", "error", err)
		return
	}
	defer ws.Close()
	ws.SetReadLimit(1 << 20)
	if err := ws.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		r.Logger.Warn("set desktop tunnel handshake deadline", "error", err)
	}

	_, rawOpen, err := ws.ReadMessage()
	if err != nil {
		_ = ws.WriteJSON(tunnel.NewErrorResponse(tunnel.NamespaceDesktop, tunnel.TypeTunnelOpen, "", http.StatusBadRequest, "invalid tunnel.open frame"))
		return
	}
	var open desktopTunnelOpenFrame
	decoder := json.NewDecoder(bytes.NewReader(rawOpen))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&open); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		_ = ws.WriteJSON(tunnel.NewErrorResponse(tunnel.NamespaceDesktop, tunnel.TypeTunnelOpen, "", http.StatusBadRequest, "invalid tunnel.open frame"))
		return
	}
	if open.V != tunnel.ProtocolVersion || open.NS != tunnel.NamespaceDesktop || open.Frame != tunnel.FrameRequest || open.Type != tunnel.TypeTunnelOpen {
		_ = ws.WriteJSON(tunnel.NewErrorResponse(tunnel.NamespaceDesktop, tunnel.TypeTunnelOpen, open.ID, http.StatusBadRequest, "expected tunnel.open request"))
		return
	}
	open.Payload.IdentityToken = strings.TrimSpace(open.Payload.IdentityToken)
	open.Payload.DeviceID = strings.TrimSpace(open.Payload.DeviceID)
	if open.Payload.IdentityToken == "" || tunnel.ValidateDesktopDeviceID(open.Payload.DeviceID) != nil {
		_ = ws.WriteJSON(tunnel.NewErrorResponse(tunnel.NamespaceDesktop, tunnel.TypeTunnelOpen, open.ID, http.StatusBadRequest, "identityToken and a valid deviceId are required"))
		return
	}
	if r.desktopIdentityVerifier == nil {
		_ = ws.WriteJSON(tunnel.NewErrorResponse(tunnel.NamespaceDesktop, tunnel.TypeTunnelOpen, open.ID, http.StatusServiceUnavailable, "desktop identity verifier is unavailable"))
		return
	}
	principal, err := r.desktopIdentityVerifier.Verify(open.Payload.IdentityToken, time.Now())
	if err != nil {
		_ = ws.WriteJSON(tunnel.NewErrorResponse(tunnel.NamespaceDesktop, tunnel.TypeTunnelOpen, open.ID, http.StatusUnauthorized, "invalid identity token"))
		return
	}
	if !r.allowMissingTunnelScope && !principal.HasScope("tunnel") {
		_ = ws.WriteJSON(tunnel.NewErrorResponse(tunnel.NamespaceDesktop, tunnel.TypeTunnelOpen, open.ID, http.StatusForbidden, "tunnel scope required"))
		return
	}
	device, err := r.DB.GetDesktopDeviceByOwnerAndID(req.Context(), principal.UserID, open.Payload.DeviceID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			r.Logger.Error("resolve desktop tunnel owner", "error", err)
		}
		_ = ws.WriteJSON(tunnel.NewErrorResponse(tunnel.NamespaceDesktop, tunnel.TypeTunnelOpen, open.ID, http.StatusForbidden, "desktop device is not registered for this identity"))
		return
	}
	if err := ws.SetReadDeadline(time.Time{}); err != nil {
		r.Logger.Warn("clear desktop tunnel handshake deadline", "error", err)
	}
	dbSession, err := r.DB.CreateDesktopSession(req.Context(), device, clientRemoteAddr)
	if err != nil {
		r.Logger.Error("create desktop session", "error", err)
		_ = ws.WriteJSON(tunnel.NewErrorResponse(tunnel.NamespaceDesktop, tunnel.TypeTunnelOpen, open.ID, http.StatusInternalServerError, "create desktop session failed"))
		return
	}
	success := tunnel.NewSuccessResponse(tunnel.NamespaceDesktop, tunnel.TypeTunnelOpen, open.ID, &tunnel.StreamResponseData{
		SessionID: dbSession.ID,
		Multiplex: "yamux",
	})
	if err := ws.WriteJSON(success); err != nil {
		_ = r.DB.EndDesktopSession(context.Background(), dbSession.ID)
		r.Logger.Error("write tunnel.open response", "error", err)
		return
	}
	r.serveTunnelSession(ws, ActiveTunnel{
		SessionID:   dbSession.ID,
		Key:         DesktopConnectionKey(device.DeviceKey),
		RemoteAddr:  clientRemoteAddr,
		ConnectedAt: dbSession.ConnectedAt,
	}, principal.ExpiresAt, func() {
		_ = r.DB.EndDesktopSession(context.Background(), dbSession.ID)
	})
}

func (r *Relay) serveTunnelSession(ws *websocket.Conn, active ActiveTunnel, expiresAt time.Time, finish func()) {
	conn := tunnel.NewWebSocketNetConn(ws)
	config := yamux.DefaultConfig()
	config.EnableKeepAlive = true
	config.KeepAliveInterval = 20 * time.Second
	session, err := yamux.Server(conn, config)
	if err != nil {
		_ = conn.Close()
		finish()
		r.Logger.Error("start yamux server", "error", err)
		return
	}
	active.Yamux = session
	var expiryTimer *time.Timer
	if !expiresAt.IsZero() {
		expiryTimer = time.AfterFunc(time.Until(expiresAt), func() {
			_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "identity expired"), time.Now().Add(time.Second))
			_ = session.Close()
		})
		defer expiryTimer.Stop()
	}
	eventPrefix := string(active.Key.Kind)
	eventSubject := "Agent"
	if active.Key.Kind == ConnectionKindDesktop {
		eventSubject = "Desktop"
	}
	_ = r.DB.AddEvent(context.Background(), eventPrefix+".connected", eventSubject+" connected", active.SessionID)
	r.Manager.SetActive(&active)
	r.Logger.Info(eventPrefix+" connected", "session", active.SessionID, "remote", active.RemoteAddr)

	<-session.CloseChan()

	r.Manager.Clear(active.SessionID)
	finish()
	_ = r.DB.AddEvent(context.Background(), eventPrefix+".disconnected", eventSubject+" disconnected", active.SessionID)
	r.Logger.Info(eventPrefix+" disconnected", "session", active.SessionID)
}

func (r *Relay) HandlePublic(w http.ResponseWriter, req *http.Request) {
	if isHostUnderBaseDomain(req.Host, r.DesktopBaseDomain) {
		if _, _, ok := mobileWebAppHost(req.Host, r.DesktopBaseDomain); ok {
			r.handleMobileWebAppPublic(w, req)
			return
		}
		r.handleDesktopPublic(w, req)
		return
	}
	if isHostUnderBaseDomain(req.Host, r.WebAppBaseDomain) {
		webApp, err := r.DB.GetActiveDesktopWebAppRouteByHost(req.Context(), req.Host)
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, req)
			return
		}
		if err != nil {
			r.writeGatewayError(w, "webapp route lookup failed", err)
			return
		}
		options := webAppRelayOptions{
			Connection: DesktopConnectionKey(webApp.Device.DeviceKey),
			DeviceID:   webApp.Device.DeviceKey,
		}
		if isWebSocketRequest(req) {
			r.handleWebAppPublicWebSocket(w, req, webApp.Route, options)
			return
		}
		r.handleWebAppPublicHTTP(w, req, webApp.Route, options)
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
	if isWebSocketRequest(req) {
		r.handlePublicWebSocket(w, req, route)
		return
	}
	r.handlePublicHTTP(w, req, route)
}

func (r *Relay) handleMobileWebAppPublic(w http.ResponseWriter, req *http.Request) {
	deviceHost, port, ok := mobileWebAppHost(req.Host, r.DesktopBaseDomain)
	if !ok {
		http.NotFound(w, req)
		return
	}
	device, err := r.DB.GetDesktopDeviceByPublicHost(req.Context(), deviceHost)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, req)
		return
	}
	if err != nil {
		r.writeGatewayError(w, "desktop lookup failed", err)
		return
	}
	sessionCookieName := r.mobileWebAppSessionCookieName()
	if req.URL != nil && !isWebSocketRequest(req) {
		queryToken := strings.TrimSpace(req.URL.Query().Get("token"))
		if queryToken != "" && (req.Method == http.MethodGet || req.Method == http.MethodHead) {
			http.SetCookie(w, &http.Cookie{
				Name:     sessionCookieName,
				Value:    queryToken,
				Path:     "/",
				Secure:   r.MobileWebAppCookieSecure,
				HttpOnly: true,
				SameSite: http.SameSiteLaxMode,
			})
			nextURL := *req.URL
			query := nextURL.Query()
			query.Del("token")
			nextURL.RawQuery = query.Encode()
			http.Redirect(w, req, nextURL.RequestURI(), http.StatusSeeOther)
			return
		}
	}
	authToken, subprotocol := mobileWebAppAuth(req, sessionCookieName)
	if authToken == "" {
		http.Error(w, "mobile authentication token is required", http.StatusUnauthorized)
		return
	}
	next := req.Clone(req.Context())
	nextURL := *req.URL
	next.URL = &nextURL
	query := next.URL.Query()
	query.Del("token")
	next.URL.RawQuery = query.Encode()
	next.RequestURI = next.URL.RequestURI()
	next.Header = req.Header.Clone()
	next.Header.Del("Authorization")
	removeCookie(next.Header, sessionCookieName)
	next.Header.Set("X-Forwarded-Host", tunnel.NormalizeHost(req.Host))
	next.Header.Set("X-Forwarded-Proto", "https")
	next.Header.Del("X-Forwarded-Prefix")

	route := store.Route{
		ID:         fmt.Sprintf("mobile:%d", port),
		PublicHost: tunnel.NormalizeHost(req.Host),
		TargetURL:  fmt.Sprintf("http://127.0.0.1:%d", port),
		Active:     true,
	}
	options := webAppRelayOptions{
		AuthToken:   authToken,
		Subprotocol: subprotocol,
		Source:      "mobile",
		ObjectType:  "mobile-webapp",
		Connection:  DesktopConnectionKey(device.DeviceKey),
		DeviceID:    device.DeviceKey,
	}
	if isWebSocketRequest(req) {
		r.handleWebAppPublicWebSocket(w, next, route, options)
		return
	}
	r.handleWebAppPublicHTTP(w, next, route, options)
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
	connection := DesktopConnectionKey(device.DeviceKey)
	stream, err := r.Manager.OpenStream(req.Context(), connection)
	if errors.Is(err, ErrNoTunnel) {
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
	active, _ := r.Manager.ActiveFor(connection)
	var bytesIn atomic.Int64
	var bytesOut atomic.Int64
	statusCode := 0
	trafficError := ""
	defer func() {
		r.recordTrafficEvent(store.TrafficEvent{
			ObjectType: "desktop",
			PublicHost: req.Host,
			DeviceID:   device.DeviceKey,
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

func (r *Relay) handleWebAppPublicHTTP(w http.ResponseWriter, req *http.Request, route store.Route, options webAppRelayOptions) {
	upstream, err := tunnel.ParseUpstreamTarget(route.TargetURL, false)
	if err != nil {
		r.writeGatewayError(w, "parse webapp upstream failed", err)
		return
	}
	connection := webAppConnection(route, options)
	stream, err := r.Manager.OpenStream(req.Context(), connection)
	if errors.Is(err, ErrNoTunnel) {
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
	active, _ := r.Manager.ActiveFor(connection)
	bytesIn := int64(0)
	bytesOut := int64(0)
	statusCode := 0
	trafficError := ""
	defer func() {
		r.recordTrafficEvent(store.TrafficEvent{
			ObjectType: valueOr(options.ObjectType, "webapp"),
			PublicHost: req.Host,
			RouteID:    route.ID,
			TokenID:    route.TokenID,
			DeviceID:   options.DeviceID,
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
		AuthToken:   options.AuthToken,
		Subprotocol: options.Subprotocol,
		Source:      options.Source,
		Public:      publicRequest(req, tunnel.StripHopHeaders(req.Header)),
		Upstream:    &upstream,
		Route:       routeMetadata(route),
		BodyLength:  tunnel.Int64Ptr(int64(len(body))),
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
	bodyLength := tunnel.StreamResponseBodyLength(response)
	if bodyLength < tunnel.UnknownBodyLength {
		trafficError = fmt.Sprintf("invalid webapp response body length: %d", bodyLength)
		statusCode = http.StatusBadGateway
		http.Error(w, trafficError, statusCode)
		return
	}
	responseHeaders := tunnel.StripHopHeaders(tunnel.StreamResponseHeaders(response))
	if options.Source == "mobile" {
		responseHeaders = rewriteMobileWebAppResponseHeaders(responseHeaders, req, upstream)
	}
	if bodyLength == tunnel.UnknownBodyLength {
		responseHeaders.Del("Content-Length")
	}
	copyHeaders(w.Header(), responseHeaders)
	statusCode = statusOr(tunnel.StreamResponseStatusCode(response), http.StatusOK)
	w.WriteHeader(statusCode)
	bytesOut, err = copyWebAppResponseBody(w, stream, bodyLength)
	if err != nil {
		trafficError = err.Error()
		r.Logger.Error("copy webapp response body", "error", err)
	}
}

func (r *Relay) handleWebAppPublicWebSocket(w http.ResponseWriter, req *http.Request, route store.Route, options webAppRelayOptions) {
	upstream, err := tunnel.ParseUpstreamTarget(route.TargetURL, true)
	if err != nil {
		r.writeGatewayError(w, "parse webapp websocket upstream failed", err)
		return
	}
	connection := webAppConnection(route, options)
	stream, err := r.Manager.OpenStream(req.Context(), connection)
	if errors.Is(err, ErrNoTunnel) {
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
	active, _ := r.Manager.ActiveFor(connection)
	var bytesIn atomic.Int64
	var bytesOut atomic.Int64
	statusCode := 0
	trafficError := ""
	defer func() {
		r.recordTrafficEvent(store.TrafficEvent{
			ObjectType: valueOr(options.ObjectType, "webapp"),
			PublicHost: req.Host,
			RouteID:    route.ID,
			TokenID:    route.TokenID,
			DeviceID:   options.DeviceID,
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
		AuthToken:   options.AuthToken,
		Subprotocol: options.Subprotocol,
		Source:      options.Source,
		Public:      publicRequest(req, tunnel.StripWebSocketDialHeaders(req.Header)),
		Upstream:    &upstream,
		Route:       routeMetadata(route),
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
	stream, err := r.Manager.OpenStream(req.Context(), AgentConnectionKey(route.TokenID))
	if errors.Is(err, ErrNoTunnel) {
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
	stream, err := r.Manager.OpenStream(req.Context(), AgentConnectionKey(route.TokenID))
	if errors.Is(err, ErrNoTunnel) {
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
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return parts[1]
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

func copyWebAppResponseBody(dst io.Writer, src io.Reader, bodyLength int64) (int64, error) {
	switch {
	case bodyLength == tunnel.UnknownBodyLength:
		return io.Copy(dst, src)
	case bodyLength == 0:
		return 0, nil
	case bodyLength > 0:
		return io.CopyN(dst, src, bodyLength)
	default:
		return 0, fmt.Errorf("invalid webapp response body length: %d", bodyLength)
	}
}

func rewriteMobileWebAppResponseHeaders(headers http.Header, req *http.Request, upstream tunnel.UpstreamTarget) http.Header {
	next := headers.Clone()
	if location := strings.TrimSpace(next.Get("Location")); location != "" {
		next.Set("Location", rewriteMobileWebAppLocation(location, req, upstream))
	}
	return next
}

func rewriteMobileWebAppLocation(location string, req *http.Request, upstream tunnel.UpstreamTarget) string {
	parsed, err := url.Parse(location)
	if err != nil {
		return location
	}
	if parsed.IsAbs() {
		hostname := strings.ToLower(parsed.Hostname())
		if hostname != "127.0.0.1" && hostname != "localhost" && hostname != "::1" && !strings.EqualFold(hostname, upstream.Host) {
			return location
		}
		parsed.Scheme = "https"
		parsed.Host = tunnel.NormalizeHost(req.Host)
	}
	return parsed.String()
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

func mobileWebAppAuth(req *http.Request, sessionCookieName string) (string, string) {
	authToken := ""
	if req.URL != nil {
		authToken = strings.TrimSpace(req.URL.Query().Get("token"))
	}
	if authToken == "" {
		authToken = bearerToken(req.Header.Get("Authorization"))
	}
	if authToken == "" {
		if cookie, err := req.Cookie(sessionCookieName); err == nil {
			authToken = strings.TrimSpace(cookie.Value)
		}
	}
	subprotocol := bearerSubprotocol(req.Header)
	if authToken == "" && subprotocol != "" {
		authToken = strings.TrimSpace(subprotocol[len("bearer."):])
	}
	return authToken, subprotocol
}

func removeCookie(header http.Header, name string) {
	cookies := (&http.Request{Header: header}).Cookies()
	header.Del("Cookie")
	for _, cookie := range cookies {
		if cookie.Name != name {
			header.Add("Cookie", cookie.String())
		}
	}
}

func mobileWebAppHost(host, baseDomain string) (string, int, bool) {
	normalizedHost := tunnel.NormalizeHost(host)
	normalizedBase := normalizeBaseDomain(baseDomain)
	suffix := "." + normalizedBase
	if normalizedBase == "" || !strings.HasSuffix(normalizedHost, suffix) {
		return "", 0, false
	}
	label := strings.TrimSuffix(normalizedHost, suffix)
	if label == "" || strings.Contains(label, ".") {
		return "", 0, false
	}
	separator := strings.LastIndexByte(label, '-')
	if separator <= 0 || separator == len(label)-1 {
		return "", 0, false
	}
	deviceLabel := label[:separator]
	portText := label[separator+1:]
	port := 0
	for _, char := range portText {
		if char < '0' || char > '9' {
			return "", 0, false
		}
		port = port*10 + int(char-'0')
		if port > 65535 {
			return "", 0, false
		}
	}
	if port <= 0 {
		return "", 0, false
	}
	return deviceLabel + suffix, port, true
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

func valueOr(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func webAppConnection(route store.Route, options webAppRelayOptions) ConnectionKey {
	if options.Connection.Kind != "" && options.Connection.ID != "" {
		return options.Connection
	}
	return AgentConnectionKey(route.TokenID)
}

func requestID() string {
	return fmt.Sprintf("req_%d", time.Now().UTC().UnixNano())
}
