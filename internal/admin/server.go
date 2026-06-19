package admin

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/linlay/zenmind-tunnel-server/internal/auth"
	"github.com/linlay/zenmind-tunnel-server/internal/config"
	"github.com/linlay/zenmind-tunnel-server/internal/proxy"
	"github.com/linlay/zenmind-tunnel-server/internal/store"
)

type Server struct {
	DB      *store.DB
	Manager *proxy.Manager
	Config  config.RelayConfig
	Logger  *slog.Logger
	ssoJWT  *auth.SSOJWTVerifier
}

const sessionCookie = "zenmind_admin"

func NewServer(db *store.DB, manager *proxy.Manager, cfg config.RelayConfig, logger *slog.Logger) (*Server, error) {
	if logger == nil {
		logger = slog.Default()
	}
	ssoJWT, err := auth.NewSSOJWTVerifier(auth.SSOJWTConfig{
		Issuer:        cfg.SSOJWTIssuer,
		Audience:      cfg.SSOJWTAudience,
		PublicKeyFile: cfg.SSOJWTPublicKeyFile,
		PublicKeyPEM:  cfg.SSOJWTPublicKeyPEM,
	})
	if err != nil {
		return nil, err
	}
	return &Server{DB: db, Manager: manager, Config: cfg, Logger: logger, ssoJWT: ssoJWT}, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.withCORS(w, r)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/api/admin")
	switch {
	case path == "/login" && r.Method == http.MethodPost:
		s.handleLogin(w, r)
	case path == "/logout" && r.Method == http.MethodPost:
		s.handleLogout(w, r)
	case path == "/me" && r.Method == http.MethodGet:
		s.requireAuth(w, r, s.handleMe)
	case path == "/routes" || strings.HasPrefix(path, "/routes/"):
		s.requireAuth(w, r, s.handleRoutes)
	case path == "/services" || strings.HasPrefix(path, "/services/"):
		s.requireAuth(w, r, s.handleServices)
	case path == "/tokens" || strings.HasPrefix(path, "/tokens/"):
		s.requireAuth(w, r, s.handleTokens)
	case path == "/agents" && r.Method == http.MethodGet:
		s.requireAuth(w, r, s.handleAgents)
	case path == "/sessions" && r.Method == http.MethodGet:
		s.requireAuth(w, r, s.handleSessions)
	case path == "/events" && r.Method == http.MethodGet:
		s.requireAuth(w, r, s.handleEvents)
	case path == "/metrics" && r.Method == http.MethodGet:
		s.requireAuth(w, r, s.handleMetrics)
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	principal, _, _, _ := s.currentPrincipal(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"username": principalName(principal),
		"userId":   principal.UserID,
		"email":    principal.Email,
		"role":     principal.Role,
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var payload loginPayload
	if err := decodeJSON(r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	username := strings.TrimSpace(payload.Username)
	ok, err := s.DB.ValidateAdmin(r.Context(), username, payload.Password)
	if err != nil {
		s.writeInternal(w, "validate admin login", err)
		return
	}
	if !ok {
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	expires := time.Now().Add(24 * time.Hour)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    s.newSessionValue(username, expires),
		Path:     "/api/admin",
		Expires:  expires,
		MaxAge:   int(time.Until(expires).Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"username": username,
		"role":     "admin",
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/api/admin",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleRoutes(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/api/admin/routes"), "/")
	switch r.Method {
	case http.MethodGet:
		routes, err := s.DB.ListRoutes(r.Context())
		if err != nil {
			s.writeInternal(w, "list routes", err)
			return
		}
		writeJSON(w, http.StatusOK, routes)
	case http.MethodPost:
		var payload routePayload
		if err := decodeJSON(r, &payload); err != nil {
			writeError(w, http.StatusBadRequest, "invalid json")
			return
		}
		if err := payload.Validate(); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := s.validateActiveTokenID(r.Context(), payload.TokenID); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		route, err := s.DB.CreateRoute(r.Context(), payload.PublicHost, payload.TargetURL, payload.Active, payload.TokenID)
		if err != nil {
			s.writeInternal(w, "create route", err)
			return
		}
		_ = s.DB.AddEvent(context.Background(), "route.created", "Route created", route.PublicHost)
		writeJSON(w, http.StatusCreated, route)
	case http.MethodPut:
		if id == "" {
			writeError(w, http.StatusBadRequest, "route id is required")
			return
		}
		var payload routePayload
		if err := decodeJSON(r, &payload); err != nil {
			writeError(w, http.StatusBadRequest, "invalid json")
			return
		}
		if err := payload.Validate(); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := s.validateActiveTokenID(r.Context(), payload.TokenID); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		route, err := s.DB.UpdateRoute(r.Context(), id, payload.PublicHost, payload.TargetURL, payload.Active, payload.TokenID)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "route not found")
			return
		}
		if err != nil {
			s.writeInternal(w, "update route", err)
			return
		}
		_ = s.DB.AddEvent(context.Background(), "route.updated", "Route updated", route.PublicHost)
		writeJSON(w, http.StatusOK, route)
	case http.MethodDelete:
		if id == "" {
			writeError(w, http.StatusBadRequest, "route id is required")
			return
		}
		err := s.DB.DeleteRoute(r.Context(), id)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "route not found")
			return
		}
		if err != nil {
			s.writeInternal(w, "delete route", err)
			return
		}
		_ = s.DB.AddEvent(context.Background(), "route.deleted", "Route deleted", id)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleTokens(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/api/admin/tokens"), "/")
	switch r.Method {
	case http.MethodGet:
		tokens, err := s.DB.ListTokens(r.Context())
		if err != nil {
			s.writeInternal(w, "list tokens", err)
			return
		}
		writeJSON(w, http.StatusOK, tokens)
	case http.MethodPost:
		var payload struct {
			Name string `json:"name"`
		}
		if err := decodeJSON(r, &payload); err != nil {
			writeError(w, http.StatusBadRequest, "invalid json")
			return
		}
		payload.Name = strings.TrimSpace(payload.Name)
		if payload.Name == "" {
			writeError(w, http.StatusBadRequest, "name is required")
			return
		}
		rawToken, err := auth.NewToken()
		if err != nil {
			s.writeInternal(w, "generate token", err)
			return
		}
		token, err := s.DB.CreateToken(r.Context(), payload.Name, rawToken)
		if err != nil {
			s.writeInternal(w, "create token", err)
			return
		}
		_ = s.DB.AddEvent(context.Background(), "token.created", "Tunnel token created", token.Name)
		writeJSON(w, http.StatusCreated, map[string]any{"token": token, "secret": rawToken})
	case http.MethodDelete:
		if id == "" {
			writeError(w, http.StatusBadRequest, "token id is required")
			return
		}
		err := s.DB.DeactivateToken(r.Context(), id)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "token not found")
			return
		}
		if err != nil {
			s.writeInternal(w, "deactivate token", err)
			return
		}
		_ = s.DB.AddEvent(context.Background(), "token.deactivated", "Tunnel token deactivated", id)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleServices(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/api/admin/services"), "/")
	if name == "" {
		writeError(w, http.StatusBadRequest, "service name is required")
		return
	}
	if err := validateServiceName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	publicHost := s.servicePublicHost(name)
	switch r.Method {
	case http.MethodGet:
		route, err := s.DB.GetRouteByHost(r.Context(), publicHost)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "service not found")
			return
		}
		if err != nil {
			s.writeInternal(w, "get service route", err)
			return
		}
		writeJSON(w, http.StatusOK, s.serviceResponse(route))
	case http.MethodPut:
		var payload servicePayload
		if err := decodeJSON(r, &payload); err != nil {
			writeError(w, http.StatusBadRequest, "invalid json")
			return
		}
		if err := payload.Validate(); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := s.validateActiveTokenID(r.Context(), payload.TokenID); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		active := true
		if payload.Active != nil {
			active = *payload.Active
		}
		route, err := s.DB.GetRouteByHost(r.Context(), publicHost)
		switch {
		case errors.Is(err, store.ErrNotFound):
			route, err = s.DB.CreateRoute(r.Context(), publicHost, payload.TargetURL, active, payload.TokenID)
			if err == nil {
				_ = s.DB.AddEvent(context.Background(), "service.published", "Service published", publicHost)
			}
		case err == nil:
			route, err = s.DB.UpdateRoute(r.Context(), route.ID, publicHost, payload.TargetURL, active, payload.TokenID)
			if err == nil {
				_ = s.DB.AddEvent(context.Background(), "service.updated", "Service updated", publicHost)
			}
		}
		if err != nil {
			s.writeInternal(w, "publish service", err)
			return
		}
		writeJSON(w, http.StatusOK, s.serviceResponse(route))
	case http.MethodDelete:
		err := s.DB.DeleteRouteByHost(r.Context(), publicHost)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "service not found")
			return
		}
		if err != nil {
			s.writeInternal(w, "delete service", err)
			return
		}
		_ = s.DB.AddEvent(context.Background(), "service.deleted", "Service deleted", publicHost)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "publicHost": publicHost})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	tokens, err := s.DB.ListTokens(r.Context())
	if err != nil {
		s.writeInternal(w, "list tokens for agents", err)
		return
	}
	routes, err := s.DB.ListRoutes(r.Context())
	if err != nil {
		s.writeInternal(w, "list routes for agents", err)
		return
	}
	routesByToken := make(map[string][]store.Route)
	for _, route := range routes {
		if route.TokenID == "" {
			continue
		}
		routesByToken[route.TokenID] = append(routesByToken[route.TokenID], route)
	}
	activeByToken := make(map[string]proxy.ActiveAgentMetric)
	for _, agent := range s.Manager.ActiveAgents() {
		activeByToken[agent.TokenID] = agent
	}
	agents := make([]agentResponse, 0, len(tokens))
	for _, token := range tokens {
		routes := routesByToken[token.ID]
		if routes == nil {
			routes = []store.Route{}
		}
		response := agentResponse{
			Token:      token,
			Routes:     routes,
			RouteCount: len(routes),
		}
		if active, ok := activeByToken[token.ID]; ok {
			response.Online = true
			response.SessionID = active.SessionID
			response.RemoteAddr = active.RemoteAddr
			response.ConnectedAt = &active.ConnectedAt
		}
		agents = append(agents, response)
	}
	writeJSON(w, http.StatusOK, agents)
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := s.DB.ListAgentSessions(r.Context(), 100)
	if err != nil {
		s.writeInternal(w, "list sessions", err)
		return
	}
	writeJSON(w, http.StatusOK, sessions)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	events, err := s.DB.ListEvents(r.Context(), 100)
	if err != nil {
		s.writeInternal(w, "list events", err)
		return
	}
	writeJSON(w, http.StatusOK, events)
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Manager.Metrics())
}

func (s *Server) ServeComponents(w http.ResponseWriter, r *http.Request) {
	s.withCORS(w, r)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.URL.Path != "/api/components" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	routes, err := s.DB.ListRoutes(r.Context())
	if err != nil {
		s.writeInternal(w, "list components", err)
		return
	}
	components := make([]componentResponse, 0, len(routes))
	for _, route := range routes {
		components = append(components, componentResponse{
			PublicHost: route.PublicHost,
			PublicURL:  "https://" + route.PublicHost,
			Active:     route.Active,
			UpdatedAt:  route.UpdatedAt,
		})
	}
	writeJSON(w, http.StatusOK, components)
}

func (s *Server) requireAuth(w http.ResponseWriter, r *http.Request, next http.HandlerFunc) {
	if _, status, message, ok := s.currentPrincipal(r); !ok {
		writeError(w, status, message)
		return
	}
	next(w, r)
}

func (s *Server) currentPrincipal(r *http.Request) (auth.SSOJWTPrincipal, int, string, bool) {
	if principal, ok := s.cookiePrincipal(r); ok {
		return principal, 0, "", true
	}

	header := r.Header.Get("Authorization")
	principal, err := s.ssoJWT.VerifyBearerHeader(header)
	if err != nil {
		if errors.Is(err, auth.ErrBearerTokenMissing) {
			return auth.SSOJWTPrincipal{}, http.StatusUnauthorized, "authentication required", false
		}
		if errors.Is(err, auth.ErrSSOJWTNotConfigured) {
			if strings.TrimSpace(header) == "" {
				return auth.SSOJWTPrincipal{}, http.StatusUnauthorized, "authentication required", false
			}
			return auth.SSOJWTPrincipal{}, http.StatusServiceUnavailable, "official JWT verifier is not configured", false
		}
		return auth.SSOJWTPrincipal{}, http.StatusUnauthorized, "invalid bearer token", false
	}
	if principal.Role != "admin" {
		return auth.SSOJWTPrincipal{}, http.StatusForbidden, "admin role required", false
	}
	if !principal.HasScope("tunnel") {
		return auth.SSOJWTPrincipal{}, http.StatusForbidden, "tunnel scope required", false
	}
	return principal, 0, "", true
}

func (s *Server) cookiePrincipal(r *http.Request) (auth.SSOJWTPrincipal, bool) {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return auth.SSOJWTPrincipal{}, false
	}
	username, ok := s.verifySessionValue(cookie.Value)
	if !ok {
		return auth.SSOJWTPrincipal{}, false
	}
	return auth.SSOJWTPrincipal{
		UserID: "local:" + username,
		Role:   "admin",
		Scope:  "tunnel",
	}, true
}

func (s *Server) writeInternal(w http.ResponseWriter, message string, err error) {
	s.Logger.Error(message, "error", err)
	writeError(w, http.StatusInternalServerError, message)
}

func (s *Server) withCORS(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if strings.HasPrefix(origin, "http://localhost:") || strings.HasPrefix(origin, "http://127.0.0.1:") {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	}
}

type routePayload struct {
	PublicHost string `json:"publicHost"`
	TargetURL  string `json:"targetUrl"`
	TokenID    string `json:"tokenId"`
	Active     bool   `json:"active"`
}

func (p routePayload) Validate() error {
	if strings.TrimSpace(p.PublicHost) == "" {
		return errors.New("publicHost is required")
	}
	if strings.TrimSpace(p.TargetURL) == "" {
		return errors.New("targetUrl is required")
	}
	if strings.TrimSpace(p.TokenID) == "" {
		return errors.New("tokenId is required")
	}
	return validateTargetURL(p.TargetURL)
}

type servicePayload struct {
	TargetURL string `json:"targetUrl"`
	TokenID   string `json:"tokenId"`
	Active    *bool  `json:"active"`
}

func (p servicePayload) Validate() error {
	if strings.TrimSpace(p.TargetURL) == "" {
		return errors.New("targetUrl is required")
	}
	if strings.TrimSpace(p.TokenID) == "" {
		return errors.New("tokenId is required")
	}
	return validateTargetURL(p.TargetURL)
}

type servicePublishResponse struct {
	Route      store.Route `json:"route"`
	PublicHost string      `json:"publicHost"`
	PublicURL  string      `json:"publicUrl"`
}

type componentResponse struct {
	PublicHost string    `json:"publicHost"`
	PublicURL  string    `json:"publicUrl"`
	Active     bool      `json:"active"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

type agentResponse struct {
	Token       store.TunnelToken `json:"token"`
	Online      bool              `json:"online"`
	SessionID   string            `json:"sessionId,omitempty"`
	RemoteAddr  string            `json:"remoteAddr,omitempty"`
	ConnectedAt *time.Time        `json:"connectedAt,omitempty"`
	Routes      []store.Route     `json:"routes"`
	RouteCount  int               `json:"routeCount"`
}

type loginPayload struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) servicePublicHost(name string) string {
	baseDomain := strings.TrimPrefix(tunnelHost(s.Config.PublicBaseDomain), ".")
	if baseDomain == "" {
		baseDomain = "tunnel-hub.zenmind.cc"
	}
	return name + "." + baseDomain
}

func (s *Server) serviceResponse(route store.Route) servicePublishResponse {
	return servicePublishResponse{
		Route:      route,
		PublicHost: route.PublicHost,
		PublicURL:  "https://" + route.PublicHost,
	}
}

func validateServiceName(name string) error {
	if name != strings.ToLower(name) {
		return errors.New("service name must be lowercase")
	}
	if len(name) > 63 {
		return errors.New("service name must be 63 characters or fewer")
	}
	if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
		return errors.New("service name cannot start or end with hyphen")
	}
	if reservedServiceNames[name] {
		return errors.New("service name is reserved")
	}
	for _, char := range name {
		if char >= 'a' && char <= 'z' {
			continue
		}
		if char >= '0' && char <= '9' {
			continue
		}
		if char == '-' {
			continue
		}
		return errors.New("service name must contain only lowercase letters, numbers, and hyphens")
	}
	return nil
}

func validateTargetURL(raw string) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return errors.New("targetUrl must be a valid URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errors.New("targetUrl must use http or https")
	}
	if parsed.Host == "" {
		return errors.New("targetUrl must include a host")
	}
	return nil
}

func (s *Server) validateActiveTokenID(ctx context.Context, tokenID string) error {
	if strings.TrimSpace(tokenID) == "" {
		return errors.New("tokenId is required")
	}
	if _, err := s.DB.GetActiveToken(ctx, tokenID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return errors.New("tokenId must reference an active token")
		}
		return err
	}
	return nil
}

func tunnelHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	host = strings.TrimSuffix(host, ".")
	return host
}

func principalName(principal auth.SSOJWTPrincipal) string {
	if principal.Email != "" {
		return principal.Email
	}
	if strings.HasPrefix(principal.UserID, "local:") {
		return strings.TrimPrefix(principal.UserID, "local:")
	}
	return "sso:" + principal.UserID
}

func (s *Server) newSessionValue(username string, expires time.Time) string {
	payload := username + "|" + strconv.FormatInt(expires.Unix(), 10)
	signature := s.signSessionPayload(payload)
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func (s *Server) verifySessionValue(value string) (string, bool) {
	parts := strings.Split(value, ".")
	if len(parts) != 2 {
		return "", false
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", false
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false
	}
	payload := string(payloadBytes)
	if !hmac.Equal(signature, s.signSessionPayload(payload)) {
		return "", false
	}
	fields := strings.Split(payload, "|")
	if len(fields) != 2 || strings.TrimSpace(fields[0]) == "" {
		return "", false
	}
	expiresUnix, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || time.Now().Unix() >= expiresUnix {
		return "", false
	}
	return fields[0], true
}

func (s *Server) signSessionPayload(payload string) []byte {
	mac := hmac.New(sha256.New, []byte(s.Config.CookieSecret))
	_, _ = mac.Write([]byte(payload))
	return mac.Sum(nil)
}

var reservedServiceNames = map[string]bool{
	"admin":  true,
	"api":    true,
	"www":    true,
	"tunnel": true,
	"relay":  true,
}

func decodeJSON(r *http.Request, value any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(value)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
