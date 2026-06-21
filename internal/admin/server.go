package admin

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/linlay/zenmind-tunnel-server/internal/auth"
	"github.com/linlay/zenmind-tunnel-server/internal/config"
	"github.com/linlay/zenmind-tunnel-server/internal/proxy"
	"github.com/linlay/zenmind-tunnel-server/internal/store"
)

const adminSessionCookieName = "tunnel_hub_session"

type Server struct {
	DB      *store.DB
	Manager *proxy.Manager
	Config  config.RelayConfig
	Logger  *slog.Logger
	ssoJWT  *auth.SSOJWTVerifier
}

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
	case path == "/users" || strings.HasPrefix(path, "/users/"):
		s.requireAuth(w, r, s.handleUsers)
	case path == "/overview" && r.Method == http.MethodGet:
		s.requireAuth(w, r, s.handleOverview)
	case path == "/desktops" && r.Method == http.MethodGet:
		s.requireAuth(w, r, s.handleDesktops)
	case path == "/webapps" && r.Method == http.MethodGet:
		s.requireAuth(w, r, s.handleWebApps)
	case path == "/activity" && r.Method == http.MethodGet:
		s.requireAuth(w, r, s.handleActivity)
	case path == "/agents" && r.Method == http.MethodGet:
		s.requireAuth(w, r, s.handleAgents)
	case path == "/sessions" && r.Method == http.MethodGet:
		s.requireAuth(w, r, s.handleSessions)
	case strings.HasPrefix(path, "/sessions/"):
		s.requireAuth(w, r, s.handleSessionActions)
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
	writeJSON(w, http.StatusOK, principalResponse(principal))
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var payload loginPayload
	if err := decodeJSON(r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	user, err := s.DB.VerifyAdminLogin(r.Context(), payload.Username, payload.Password)
	if errors.Is(err, store.ErrUserNotFound) || errors.Is(err, store.ErrInvalidPassword) || errors.Is(err, store.ErrUserInactive) {
		writeError(w, http.StatusUnauthorized, "invalid username or password")
		return
	}
	if err != nil {
		s.writeInternal(w, "admin login", err)
		return
	}
	session, err := s.DB.CreateAdminSession(r.Context(), user.ID, s.Config.AdminSessionTTL)
	if err != nil {
		s.writeInternal(w, "create admin session", err)
		return
	}
	http.SetCookie(w, s.adminSessionCookie(session.Token, session.ExpiresAt))
	writeJSON(w, http.StatusOK, principalResponse(localAdminPrincipal(user)))
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(adminSessionCookieName); err == nil && cookie.Value != "" {
		_ = s.DB.DeleteAdminSession(r.Context(), cookie.Value)
	}
	http.SetCookie(w, s.expiredAdminSessionCookie())
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
		writeError(w, http.StatusMethodNotAllowed, "manual token creation is disabled")
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

func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/api/admin/users"), "/")
	switch r.Method {
	case http.MethodGet:
		if id != "" {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		users, err := s.DB.ListAdminUsers(r.Context())
		if err != nil {
			s.writeInternal(w, "list admin users", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": users})
	case http.MethodPost:
		if id != "" {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		var payload createAdminUserPayload
		if err := decodeJSON(r, &payload); err != nil {
			writeError(w, http.StatusBadRequest, "invalid json")
			return
		}
		user, err := s.DB.CreateAdminUserWithStatus(r.Context(), payload.Username, payload.Password, payload.Status)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		_ = s.DB.AddEvent(context.Background(), "admin_user.created", "Admin user created", user.Username)
		writeJSON(w, http.StatusCreated, user)
	case http.MethodPatch:
		if id == "" {
			writeError(w, http.StatusBadRequest, "admin user id is required")
			return
		}
		var payload patchAdminUserPayload
		if err := decodeJSON(r, &payload); err != nil {
			writeError(w, http.StatusBadRequest, "invalid json")
			return
		}
		user, err := s.DB.UpdateAdminUser(r.Context(), id, store.AdminUserPatch{
			Username: payload.Username,
			Password: payload.Password,
			Status:   payload.Status,
		})
		if errors.Is(err, store.ErrUserNotFound) {
			writeError(w, http.StatusNotFound, "admin user not found")
			return
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		_ = s.DB.AddEvent(context.Background(), "admin_user.updated", "Admin user updated", user.Username)
		writeJSON(w, http.StatusOK, user)
	case http.MethodDelete:
		if id == "" {
			writeError(w, http.StatusBadRequest, "admin user id is required")
			return
		}
		user, err := s.DB.DisableAdminUser(r.Context(), id)
		if errors.Is(err, store.ErrUserNotFound) {
			writeError(w, http.StatusNotFound, "admin user not found")
			return
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		_ = s.DB.AddEvent(context.Background(), "admin_user.disabled", "Admin user disabled", user.Username)
		writeJSON(w, http.StatusOK, user)
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

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	devices, err := s.DB.ListDesktopDevices(r.Context())
	if err != nil {
		s.writeInternal(w, "list desktop devices", err)
		return
	}
	webApps, err := s.DB.ListDesktopWebApps(r.Context())
	if err != nil {
		s.writeInternal(w, "list desktop webapps", err)
		return
	}
	sessions, err := s.DB.ListAgentSessions(r.Context(), 500)
	if err != nil {
		s.writeInternal(w, "list agent sessions", err)
		return
	}
	totals, err := s.DB.TrafficTotals(r.Context())
	if err != nil {
		s.writeInternal(w, "traffic totals", err)
		return
	}
	trafficRange := normalizeTrafficRange(r.URL.Query().Get("range"))
	series, err := s.trafficSeries(r.Context(), trafficRange)
	if err != nil {
		s.writeInternal(w, "traffic series", err)
		return
	}

	tokenToDevice := desktopIdentityByToken(devices)
	var recentAt *time.Time
	recentIdentity := ""
	recentDevice := ""
	if len(sessions) > 0 {
		recentAt = &sessions[0].ConnectedAt
		if device, ok := tokenToDevice[sessions[0].TokenID]; ok {
			recentIdentity = desktopIdentity(device)
			recentDevice = device.DeviceName
			if recentDevice == "" {
				recentDevice = device.DeviceID
			}
		}
	}
	activeWebApps := 0
	for _, webApp := range webApps {
		if webApp.Active {
			activeWebApps++
		}
	}
	metrics := s.Manager.Metrics()
	writeJSON(w, http.StatusOK, overviewResponse{
		Range:                  trafficRange,
		DesktopConnectionCount: metrics.ActiveAgentCount,
		WebAppCount:            len(webApps),
		TotalTrafficBytes:      totals.BytesIn + totals.BytesOut,
		RecentConnectionAt:     recentAt,
		RecentIdentity:         recentIdentity,
		RecentDevice:           recentDevice,
		Resources: overviewResourceSummary{
			TotalDesktops:  len(devices),
			OnlineDesktops: metrics.ActiveAgentCount,
			TotalWebApps:   len(webApps),
			ActiveWebApps:  activeWebApps,
			ActiveStreams:  metrics.ActiveStreams,
			TotalStreams:   metrics.TotalStreams,
		},
		Traffic: series,
	})
}

func (s *Server) handleDesktops(w http.ResponseWriter, r *http.Request) {
	devices, err := s.DB.ListDesktopDevices(r.Context())
	if err != nil {
		s.writeInternal(w, "list desktop devices", err)
		return
	}
	tokens, err := s.DB.ListTokens(r.Context())
	if err != nil {
		s.writeInternal(w, "list tokens for desktops", err)
		return
	}
	webApps, err := s.DB.ListDesktopWebApps(r.Context())
	if err != nil {
		s.writeInternal(w, "list desktop webapps", err)
		return
	}
	sessions, err := s.DB.ListAgentSessions(r.Context(), 500)
	if err != nil {
		s.writeInternal(w, "list desktop sessions", err)
		return
	}
	statsByToken, err := s.DB.TrafficStatsByToken(r.Context())
	if err != nil {
		s.writeInternal(w, "desktop traffic stats", err)
		return
	}

	activeByToken := make(map[string]proxy.ActiveAgentMetric)
	for _, active := range s.Manager.ActiveAgents() {
		activeByToken[active.TokenID] = active
	}
	tokenByID := make(map[string]store.TunnelToken)
	for _, token := range tokens {
		tokenByID[token.ID] = token
	}
	lastSessionByToken := make(map[string]store.AgentSession)
	for _, session := range sessions {
		if _, ok := lastSessionByToken[session.TokenID]; !ok {
			lastSessionByToken[session.TokenID] = session
		}
	}
	webAppCountByDevice := make(map[string]int)
	for _, webApp := range webApps {
		webAppCountByDevice[webApp.DeviceKey]++
	}

	response := make([]desktopAdminResponse, 0, len(devices))
	for _, device := range devices {
		token := tokenByID[device.TokenID]
		item := desktopAdminResponse{
			DeviceID:    device.DeviceID,
			DeviceName:  device.DeviceName,
			OwnerUserID: device.OwnerUserID,
			OwnerEmail:  device.OwnerEmail,
			OwnerName:   device.OwnerName,
			PublicHost:  device.PublicHost,
			PublicURL:   "https://" + device.PublicHost,
			TokenID:     device.TokenID,
			TokenName:   token.Name,
			TokenActive: token.Active,
			CreatedAt:   device.CreatedAt,
			UpdatedAt:   device.UpdatedAt,
			WebAppCount: webAppCountByDevice[device.DeviceKey],
			Traffic:     statsByToken[device.TokenID],
		}
		if active, ok := activeByToken[device.TokenID]; ok {
			item.Online = true
			item.SessionID = active.SessionID
			item.RemoteAddr = active.RemoteAddr
			item.ConnectedAt = &active.ConnectedAt
		}
		if session, ok := lastSessionByToken[device.TokenID]; ok {
			item.LastConnectedAt = &session.ConnectedAt
		}
		response = append(response, item)
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleWebApps(w http.ResponseWriter, r *http.Request) {
	webApps, err := s.DB.ListDesktopWebApps(r.Context())
	if err != nil {
		s.writeInternal(w, "list desktop webapps", err)
		return
	}
	routes, err := s.DB.ListRoutes(r.Context())
	if err != nil {
		s.writeInternal(w, "list webapp routes", err)
		return
	}
	devices, err := s.DB.ListDesktopDevices(r.Context())
	if err != nil {
		s.writeInternal(w, "list webapp devices", err)
		return
	}
	statsByHost, err := s.DB.TrafficStatsByPublicHost(r.Context())
	if err != nil {
		s.writeInternal(w, "webapp traffic stats", err)
		return
	}

	routeByID := make(map[string]store.Route)
	for _, route := range routes {
		routeByID[route.ID] = route
	}
	deviceByKey := make(map[string]store.DesktopDevice)
	for _, device := range devices {
		deviceByKey[device.DeviceKey] = device
	}
	onlineTokenIDs := make(map[string]bool)
	for _, active := range s.Manager.ActiveAgents() {
		onlineTokenIDs[active.TokenID] = true
	}

	response := make([]webAppAdminResponse, 0, len(webApps))
	for _, webApp := range webApps {
		route := routeByID[webApp.RouteID]
		device := deviceByKey[webApp.DeviceKey]
		stats := statsByHost[webApp.PublicHost]
		item := webAppAdminResponse{
			ID:           webApp.ID,
			Name:         webApp.Name,
			RouteID:      webApp.RouteID,
			PublicHost:   webApp.PublicHost,
			PublicURL:    "https://" + webApp.PublicHost,
			TargetURL:    webApp.TargetURL,
			TokenID:      route.TokenID,
			DeviceID:     device.DeviceID,
			DeviceName:   device.DeviceName,
			Active:       webApp.Active,
			Online:       onlineTokenIDs[route.TokenID],
			Route:        route,
			RequestCount: stats.RequestCount,
			LastAccessAt: stats.LastAt,
			Traffic:      stats,
			CreatedAt:    webApp.CreatedAt,
			UpdatedAt:    webApp.UpdatedAt,
		}
		response = append(response, item)
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	objectType := normalizeActivityObjectType(r.URL.Query().Get("objectType"))
	query := strings.TrimSpace(r.URL.Query().Get("q"))

	sessions, err := s.DB.ListAgentSessions(r.Context(), 300)
	if err != nil {
		s.writeInternal(w, "list activity sessions", err)
		return
	}
	events, err := s.DB.ListEvents(r.Context(), 300)
	if err != nil {
		s.writeInternal(w, "list activity events", err)
		return
	}
	trafficEvents, err := s.DB.ListTrafficEvents(r.Context(), 500, objectType, query)
	if err != nil {
		s.writeInternal(w, "list traffic activity", err)
		return
	}

	items := make([]activityResponseItem, 0, len(sessions)+len(events)+len(trafficEvents))
	for _, session := range sessions {
		items = appendActivityItem(items, activityResponseItem{
			ID:         "session-connected-" + session.ID,
			ObjectType: "desktop",
			Type:       "agent.connected",
			Message:    "Agent connected",
			Details:    session.RemoteAddr,
			TokenID:    session.TokenID,
			SessionID:  session.ID,
			CreatedAt:  session.ConnectedAt,
		}, objectType, query)
		if session.DisconnectedAt != nil {
			items = appendActivityItem(items, activityResponseItem{
				ID:         "session-disconnected-" + session.ID,
				ObjectType: "desktop",
				Type:       "agent.disconnected",
				Message:    "Agent disconnected",
				Details:    session.RemoteAddr,
				TokenID:    session.TokenID,
				SessionID:  session.ID,
				CreatedAt:  *session.DisconnectedAt,
			}, objectType, query)
		}
	}
	for _, event := range events {
		items = appendActivityItem(items, activityResponseItem{
			ID:         "event-" + strconv.FormatInt(event.ID, 10),
			ObjectType: eventObjectType(event.Type),
			Type:       event.Type,
			Message:    event.Message,
			Details:    event.Details,
			CreatedAt:  event.CreatedAt,
		}, objectType, query)
	}
	for _, event := range trafficEvents {
		message := "Tunnel request"
		if event.ObjectType == "webapp" {
			message = "Webapp request"
		}
		if event.ObjectType == "desktop" {
			message = "Desktop websocket"
		}
		items = append(items, activityResponseItem{
			ID:         "traffic-" + strconv.FormatInt(event.ID, 10),
			ObjectType: event.ObjectType,
			Type:       "traffic." + event.Kind,
			Message:    message,
			Details:    event.PublicHost + event.Path,
			PublicHost: event.PublicHost,
			RouteID:    event.RouteID,
			TokenID:    event.TokenID,
			SessionID:  event.SessionID,
			StatusCode: event.StatusCode,
			BytesIn:    event.BytesIn,
			BytesOut:   event.BytesOut,
			Error:      event.Error,
			CreatedAt:  event.OccurredAt,
		})
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].CreatedAt.After(items[j].CreatedAt)
	})
	if len(items) > 500 {
		items = items[:500]
	}
	writeJSON(w, http.StatusOK, activityResponse{Items: items})
}

func (s *Server) handleSessionActions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	suffix := strings.TrimPrefix(r.URL.Path, "/api/admin/sessions/")
	sessionID := strings.TrimSuffix(suffix, "/close")
	if sessionID == suffix || strings.TrimSpace(sessionID) == "" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	session, err := s.DB.GetAgentSession(r.Context(), sessionID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	if err != nil {
		s.writeInternal(w, "get agent session", err)
		return
	}
	if session.DisconnectedAt != nil {
		writeError(w, http.StatusConflict, "session already disconnected")
		return
	}
	if err := s.Manager.CloseSession(sessionID); errors.Is(err, proxy.ErrNoAgent) {
		writeError(w, http.StatusConflict, "session is not active")
		return
	} else if err != nil {
		s.writeInternal(w, "close agent session", err)
		return
	}
	_ = s.DB.AddEvent(context.Background(), "agent.closed", "Agent connection closed", sessionID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
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

type adminPrincipal struct {
	UserID   string
	Username string
	Email    string
	Role     string
	Source   string
}

func (s *Server) currentPrincipal(r *http.Request) (adminPrincipal, int, string, bool) {
	if principal, ok := s.cookiePrincipal(r); ok {
		return principal, 0, "", true
	}
	header := r.Header.Get("Authorization")
	jwtPrincipal, err := s.ssoJWT.VerifyBearerHeader(header)
	if err != nil {
		if errors.Is(err, auth.ErrBearerTokenMissing) {
			return adminPrincipal{}, http.StatusUnauthorized, "authentication required", false
		}
		if errors.Is(err, auth.ErrSSOJWTNotConfigured) {
			if strings.TrimSpace(header) == "" {
				return adminPrincipal{}, http.StatusUnauthorized, "authentication required", false
			}
			return adminPrincipal{}, http.StatusServiceUnavailable, "official JWT verifier is not configured", false
		}
		return adminPrincipal{}, http.StatusUnauthorized, "invalid bearer token", false
	}
	if jwtPrincipal.Role != "admin" {
		return adminPrincipal{}, http.StatusForbidden, "admin role required", false
	}
	if !jwtPrincipal.HasScope("tunnel") {
		return adminPrincipal{}, http.StatusForbidden, "tunnel scope required", false
	}
	return ssoAdminPrincipal(jwtPrincipal), 0, "", true
}

func (s *Server) cookiePrincipal(r *http.Request) (adminPrincipal, bool) {
	cookie, err := r.Cookie(adminSessionCookieName)
	if err != nil || cookie.Value == "" {
		return adminPrincipal{}, false
	}
	user, err := s.DB.AuthenticateAdminSession(r.Context(), cookie.Value, time.Now().UTC())
	if err != nil {
		return adminPrincipal{}, false
	}
	return localAdminPrincipal(user), true
}

func (s *Server) adminSessionCookie(token string, expiresAt time.Time) *http.Cookie {
	return &http.Cookie{
		Name:     adminSessionCookieName,
		Value:    token,
		Path:     "/",
		Expires:  expiresAt,
		MaxAge:   int(time.Until(expiresAt).Seconds()),
		HttpOnly: true,
		Secure:   s.Config.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	}
}

func (s *Server) expiredAdminSessionCookie() *http.Cookie {
	return &http.Cookie{
		Name:     adminSessionCookieName,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.Config.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	}
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

type overviewResponse struct {
	Range                  string                  `json:"range"`
	DesktopConnectionCount int                     `json:"desktopConnectionCount"`
	WebAppCount            int                     `json:"webAppCount"`
	TotalTrafficBytes      int64                   `json:"totalTrafficBytes"`
	RecentConnectionAt     *time.Time              `json:"recentConnectionAt,omitempty"`
	RecentIdentity         string                  `json:"recentIdentity,omitempty"`
	RecentDevice           string                  `json:"recentDevice,omitempty"`
	Resources              overviewResourceSummary `json:"resources"`
	Traffic                []trafficPoint          `json:"traffic"`
}

type overviewResourceSummary struct {
	TotalDesktops  int   `json:"totalDesktops"`
	OnlineDesktops int   `json:"onlineDesktops"`
	TotalWebApps   int   `json:"totalWebApps"`
	ActiveWebApps  int   `json:"activeWebApps"`
	ActiveStreams  int64 `json:"activeStreams"`
	TotalStreams   int64 `json:"totalStreams"`
}

type trafficPoint struct {
	Bucket     time.Time `json:"bucket"`
	Label      string    `json:"label"`
	BytesIn    int64     `json:"bytesIn"`
	BytesOut   int64     `json:"bytesOut"`
	TotalBytes int64     `json:"totalBytes"`
}

type desktopAdminResponse struct {
	DeviceID        string             `json:"deviceId"`
	DeviceName      string             `json:"deviceName,omitempty"`
	OwnerUserID     string             `json:"ownerUserId,omitempty"`
	OwnerEmail      string             `json:"ownerEmail,omitempty"`
	OwnerName       string             `json:"ownerName,omitempty"`
	PublicHost      string             `json:"publicHost"`
	PublicURL       string             `json:"publicUrl"`
	TokenID         string             `json:"tokenId"`
	TokenName       string             `json:"tokenName,omitempty"`
	TokenActive     bool               `json:"tokenActive"`
	Online          bool               `json:"online"`
	SessionID       string             `json:"sessionId,omitempty"`
	RemoteAddr      string             `json:"remoteAddr,omitempty"`
	ConnectedAt     *time.Time         `json:"connectedAt,omitempty"`
	LastConnectedAt *time.Time         `json:"lastConnectedAt,omitempty"`
	Traffic         store.TrafficStats `json:"traffic"`
	WebAppCount     int                `json:"webAppCount"`
	CreatedAt       time.Time          `json:"createdAt"`
	UpdatedAt       time.Time          `json:"updatedAt"`
}

type webAppAdminResponse struct {
	ID           string             `json:"id"`
	Name         string             `json:"name"`
	RouteID      string             `json:"routeId"`
	PublicHost   string             `json:"publicHost"`
	PublicURL    string             `json:"publicUrl"`
	TargetURL    string             `json:"targetUrl"`
	TokenID      string             `json:"tokenId,omitempty"`
	DeviceID     string             `json:"deviceId,omitempty"`
	DeviceName   string             `json:"deviceName,omitempty"`
	Active       bool               `json:"active"`
	Online       bool               `json:"online"`
	Route        store.Route        `json:"route"`
	RequestCount int64              `json:"requestCount"`
	LastAccessAt *time.Time         `json:"lastAccessAt,omitempty"`
	Traffic      store.TrafficStats `json:"traffic"`
	CreatedAt    time.Time          `json:"createdAt"`
	UpdatedAt    time.Time          `json:"updatedAt"`
}

type activityResponse struct {
	Items []activityResponseItem `json:"items"`
}

type activityResponseItem struct {
	ID         string    `json:"id"`
	ObjectType string    `json:"objectType"`
	Type       string    `json:"type"`
	Message    string    `json:"message"`
	Details    string    `json:"details,omitempty"`
	PublicHost string    `json:"publicHost,omitempty"`
	RouteID    string    `json:"routeId,omitempty"`
	TokenID    string    `json:"tokenId,omitempty"`
	SessionID  string    `json:"sessionId,omitempty"`
	StatusCode int       `json:"statusCode,omitempty"`
	BytesIn    int64     `json:"bytesIn,omitempty"`
	BytesOut   int64     `json:"bytesOut,omitempty"`
	Error      string    `json:"error,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
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

type createAdminUserPayload struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Status   string `json:"status"`
}

type patchAdminUserPayload struct {
	Username *string `json:"username"`
	Password *string `json:"password"`
	Status   *string `json:"status"`
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

func (s *Server) trafficSeries(ctx context.Context, rangeName string) ([]trafficPoint, error) {
	now := time.Now().UTC()
	start, step, count, label := trafficRangeSpec(now, rangeName)
	events, err := s.DB.ListTrafficEventsSince(ctx, start)
	if err != nil {
		return nil, err
	}
	points := make([]trafficPoint, 0, count)
	indexByBucket := make(map[time.Time]int, count)
	for i := 0; i < count; i++ {
		bucket := start.Add(time.Duration(i) * step)
		if rangeName == "month" {
			bucket = start.AddDate(0, i, 0)
		}
		points = append(points, trafficPoint{Bucket: bucket, Label: label(bucket)})
		indexByBucket[bucket] = i
	}
	for _, event := range events {
		bucket := trafficBucket(event.OccurredAt.UTC(), rangeName)
		index, ok := indexByBucket[bucket]
		if !ok {
			continue
		}
		points[index].BytesIn += event.BytesIn
		points[index].BytesOut += event.BytesOut
		points[index].TotalBytes = points[index].BytesIn + points[index].BytesOut
	}
	return points, nil
}

func trafficRangeSpec(now time.Time, rangeName string) (time.Time, time.Duration, int, func(time.Time) string) {
	switch rangeName {
	case "day":
		start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -29)
		return start, 24 * time.Hour, 30, func(t time.Time) string { return t.Format("01-02") }
	case "month":
		start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -11, 0)
		return start, 0, 12, func(t time.Time) string { return t.Format("2006-01") }
	default:
		start := now.Truncate(time.Hour).Add(-23 * time.Hour)
		return start, time.Hour, 24, func(t time.Time) string { return t.Format("15:04") }
	}
}

func trafficBucket(value time.Time, rangeName string) time.Time {
	switch rangeName {
	case "day":
		return time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, time.UTC)
	case "month":
		return time.Date(value.Year(), value.Month(), 1, 0, 0, 0, 0, time.UTC)
	default:
		return value.Truncate(time.Hour)
	}
}

func normalizeTrafficRange(value string) string {
	switch strings.TrimSpace(value) {
	case "day", "month":
		return strings.TrimSpace(value)
	default:
		return "hour"
	}
}

func normalizeActivityObjectType(value string) string {
	switch strings.TrimSpace(value) {
	case "desktop", "webapp", "admin", "system":
		return strings.TrimSpace(value)
	default:
		return "all"
	}
}

func desktopIdentityByToken(devices []store.DesktopDevice) map[string]store.DesktopDevice {
	out := make(map[string]store.DesktopDevice, len(devices))
	for _, device := range devices {
		out[device.TokenID] = device
	}
	return out
}

func desktopIdentity(device store.DesktopDevice) string {
	for _, value := range []string{device.OwnerName, device.OwnerEmail, device.DeviceName, device.DeviceID, device.PublicHost} {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return device.TokenID
}

func eventObjectType(eventType string) string {
	switch {
	case strings.HasPrefix(eventType, "admin_user"):
		return "admin"
	case strings.HasPrefix(eventType, "route"), strings.HasPrefix(eventType, "service"), strings.HasPrefix(eventType, "desktop_webapp"):
		return "webapp"
	case strings.HasPrefix(eventType, "agent"), strings.HasPrefix(eventType, "desktop_device"), strings.HasPrefix(eventType, "token"):
		return "desktop"
	default:
		return "system"
	}
}

func appendActivityItem(items []activityResponseItem, item activityResponseItem, objectType, query string) []activityResponseItem {
	if objectType != "all" && item.ObjectType != objectType {
		return items
	}
	if query != "" && !activityMatches(item, query) {
		return items
	}
	return append(items, item)
}

func activityMatches(item activityResponseItem, query string) bool {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return true
	}
	haystack := strings.ToLower(strings.Join([]string{
		item.ObjectType,
		item.Type,
		item.Message,
		item.Details,
		item.PublicHost,
		item.RouteID,
		item.TokenID,
		item.SessionID,
		item.Error,
	}, " "))
	return strings.Contains(haystack, query)
}

func tunnelHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	host = strings.TrimSuffix(host, ".")
	return host
}

func localAdminPrincipal(user store.AdminUser) adminPrincipal {
	return adminPrincipal{
		UserID:   user.ID,
		Username: user.Username,
		Role:     "admin",
		Source:   "local",
	}
}

func ssoAdminPrincipal(principal auth.SSOJWTPrincipal) adminPrincipal {
	username := principal.Email
	if username == "" {
		username = "sso:" + principal.UserID
	}
	return adminPrincipal{
		UserID:   principal.UserID,
		Username: username,
		Email:    principal.Email,
		Role:     principal.Role,
		Source:   "sso",
	}
}

func principalResponse(principal adminPrincipal) map[string]any {
	return map[string]any{
		"username": principal.Username,
		"userId":   principal.UserID,
		"email":    principal.Email,
		"role":     principal.Role,
		"source":   principal.Source,
	}
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
