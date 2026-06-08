package admin

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/linlay/zenmind-tunnel-server/internal/auth"
	"github.com/linlay/zenmind-tunnel-server/internal/config"
	"github.com/linlay/zenmind-tunnel-server/internal/proxy"
	"github.com/linlay/zenmind-tunnel-server/internal/store"
)

const sessionCookie = "zenmind_admin"

type Server struct {
	DB      *store.DB
	Manager *proxy.Manager
	Config  config.RelayConfig
	Logger  *slog.Logger
}

func NewServer(db *store.DB, manager *proxy.Manager, cfg config.RelayConfig, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{DB: db, Manager: manager, Config: cfg, Logger: logger}
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
	case path == "/tokens" || strings.HasPrefix(path, "/tokens/"):
		s.requireAuth(w, r, s.handleTokens)
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

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	ok, err := s.DB.ValidateAdmin(r.Context(), payload.Username, payload.Password)
	if err != nil {
		s.Logger.Error("validate admin", "error", err)
		writeError(w, http.StatusInternalServerError, "login failed")
		return
	}
	if !ok {
		writeError(w, http.StatusUnauthorized, "invalid username or password")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    auth.SignSession(s.Config.CookieSecret, payload.Username, 24*time.Hour),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   false,
		MaxAge:   int((24 * time.Hour).Seconds()),
	})
	writeJSON(w, http.StatusOK, map[string]any{"username": payload.Username})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	username, _ := s.currentUser(r)
	writeJSON(w, http.StatusOK, map[string]any{"username": username})
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
		route, err := s.DB.CreateRoute(r.Context(), payload.PublicHost, payload.TargetURL, payload.Active)
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
		route, err := s.DB.UpdateRoute(r.Context(), id, payload.PublicHost, payload.TargetURL, payload.Active)
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

func (s *Server) requireAuth(w http.ResponseWriter, r *http.Request, next http.HandlerFunc) {
	if _, ok := s.currentUser(r); !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	next(w, r)
}

func (s *Server) currentUser(r *http.Request) (string, bool) {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return "", false
	}
	return auth.VerifySession(s.Config.CookieSecret, cookie.Value)
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
	Active     bool   `json:"active"`
}

func (p routePayload) Validate() error {
	if strings.TrimSpace(p.PublicHost) == "" {
		return errors.New("publicHost is required")
	}
	if strings.TrimSpace(p.TargetURL) == "" {
		return errors.New("targetUrl is required")
	}
	return nil
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
