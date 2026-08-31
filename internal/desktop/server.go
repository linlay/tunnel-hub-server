package desktop

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"example.invalid/tunnel-hub-server/internal/auth"
	"example.invalid/tunnel-hub-server/internal/config"
	"example.invalid/tunnel-hub-server/internal/store"
	"example.invalid/tunnel-hub-server/internal/tunnel"
)

const registerPath = "/api/desktop/devices/register"
const conversationSharesPath = "/api/desktop/shares"
const publicConversationSharePagePath = "/share/"
const publicHostRetryLimit = 8
const publicLabelRandomBytes = 8

type Server struct {
	DB                            *store.DB
	Config                        config.RelayConfig
	Logger                        *slog.Logger
	ssoJWT                        *auth.SSOJWTVerifier
	now                           func() time.Time
	recordConversationShareAccess func(context.Context, string, time.Time) error
}

func NewServer(db *store.DB, cfg config.RelayConfig, logger *slog.Logger, ssoJWT *auth.SSOJWTVerifier) (*Server, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if ssoJWT == nil {
		return nil, errors.New("SSO JWT verifier is required")
	}
	return &Server{
		DB:                            db,
		Config:                        cfg,
		Logger:                        logger,
		ssoJWT:                        ssoJWT,
		now:                           time.Now,
		recordConversationShareAccess: db.RecordConversationShareAccess,
	}, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, publicConversationSharePagePath) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writePublicConversationShareError(w, http.StatusMethodNotAllowed)
			return
		}
		s.handleGetPublicConversationSharePage(w, r)
		return
	}
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	switch {
	case r.URL.Path == conversationSharesPath:
		switch r.Method {
		case http.MethodPost:
			s.handleCreateConversationShare(w, r)
		case http.MethodGet:
			s.handleListConversationShares(w, r)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	case strings.HasPrefix(r.URL.Path, conversationSharesPath+"/"):
		if r.Method != http.MethodDelete {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.handleRevokeConversationShare(w, r)
	case r.URL.Path == registerPath:
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.handleRegister(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/desktop/devices/"):
		s.handleDeviceSubresource(w, r)
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

func (s *Server) handleDeviceSubresource(w http.ResponseWriter, r *http.Request) {
	deviceID, webAppName, ok := parseWebAppPath(r.URL.Path)
	if !ok {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodPut {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	s.handleRegisterWebApp(w, r, deviceID, webAppName)
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.authorizeRegistration(w, r)
	if !ok {
		return
	}
	var payload registerPayload
	if err := decodeJSON(r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	payload.DeviceID = strings.TrimSpace(payload.DeviceID)
	payload.DeviceName = strings.TrimSpace(payload.DeviceName)
	if err := payload.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	result, err := s.registerDesktopDevice(r, principal, payload)
	if errors.Is(err, store.ErrDesktopDeviceHostConflict) {
		writeError(w, http.StatusConflict, "desktop public host already exists")
		return
	}
	if err != nil {
		s.writeInternal(w, "register desktop device", err)
		return
	}
	s.addRegistrationEvent(result)
	writeJSON(w, http.StatusOK, s.registrationResponse(result))
}

func (s *Server) handleRegisterWebApp(w http.ResponseWriter, r *http.Request, deviceID, name string) {
	principal, ok := s.authorizeRegistration(w, r)
	if !ok {
		return
	}
	var payload webAppPayload
	if err := decodeJSON(r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	payload.TargetURL = strings.TrimSpace(payload.TargetURL)
	if err := tunnel.ValidateDesktopDeviceID(deviceID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateWebAppName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := payload.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := s.registerDesktopWebApp(r, principal, deviceID, name, payload)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "desktop device not found")
		return
	}
	if errors.Is(err, store.ErrDesktopDeviceHostConflict) {
		writeError(w, http.StatusConflict, "webapp public host already exists")
		return
	}
	if err != nil {
		s.writeInternal(w, "register desktop webapp", err)
		return
	}
	_ = s.DB.AddEvent(context.Background(), "desktop_webapp.registered", "Desktop webapp registered", result.WebApp.PublicHost)
	writeJSON(w, http.StatusOK, s.webAppResponse(result))
}

func (s *Server) registerDesktopDevice(r *http.Request, principal auth.SSOJWTPrincipal, payload registerPayload) (store.RegisterDesktopDeviceResult, error) {
	var lastErr error
	for attempt := 0; attempt < publicHostRetryLimit; attempt++ {
		publicHost, err := s.randomDesktopPublicHost()
		if err != nil {
			return store.RegisterDesktopDeviceResult{}, err
		}
		result, err := s.DB.RegisterDesktopDevice(r.Context(), store.RegisterDesktopDeviceInput{
			DeviceID:    payload.DeviceID,
			DeviceName:  payload.DeviceName,
			OwnerUserID: principal.UserID,
			OwnerEmail:  principal.Email,
			OwnerName:   principal.Name,
			PublicHost:  publicHost,
		})
		if !errors.Is(err, store.ErrDesktopDeviceHostConflict) {
			return result, err
		}
		lastErr = err
	}
	if lastErr != nil {
		return store.RegisterDesktopDeviceResult{}, lastErr
	}
	return store.RegisterDesktopDeviceResult{}, errors.New("desktop public host allocation failed")
}

func (s *Server) registerDesktopWebApp(r *http.Request, principal auth.SSOJWTPrincipal, deviceID, name string, payload webAppPayload) (store.RegisterDesktopWebAppResult, error) {
	var lastErr error
	for attempt := 0; attempt < publicHostRetryLimit; attempt++ {
		publicHost, err := s.randomWebAppPublicHost()
		if err != nil {
			return store.RegisterDesktopWebAppResult{}, err
		}
		result, err := s.DB.RegisterDesktopWebApp(r.Context(), store.RegisterDesktopWebAppInput{
			OwnerUserID: principal.UserID,
			DeviceID:    deviceID,
			Name:        name,
			PublicHost:  publicHost,
			TargetURL:   payload.TargetURL,
			Active:      payload.active(),
		})
		if !errors.Is(err, store.ErrDesktopDeviceHostConflict) {
			return result, err
		}
		lastErr = err
	}
	if lastErr != nil {
		return store.RegisterDesktopWebAppResult{}, lastErr
	}
	return store.RegisterDesktopWebAppResult{}, errors.New("webapp public host allocation failed")
}

func (s *Server) authorizeRegistration(w http.ResponseWriter, r *http.Request) (auth.SSOJWTPrincipal, bool) {
	header := r.Header.Get("Authorization")
	principal, err := s.ssoJWT.VerifyBearerHeader(header)
	if err != nil {
		if errors.Is(err, auth.ErrBearerTokenMissing) {
			writeError(w, http.StatusUnauthorized, "official JWT required")
			return auth.SSOJWTPrincipal{}, false
		}
		if errors.Is(err, auth.ErrSSOJWTNotConfigured) {
			if strings.TrimSpace(header) == "" {
				writeError(w, http.StatusUnauthorized, "official JWT required")
				return auth.SSOJWTPrincipal{}, false
			}
			writeError(w, http.StatusServiceUnavailable, "official JWT verifier is not configured")
			return auth.SSOJWTPrincipal{}, false
		}
		writeError(w, http.StatusUnauthorized, "invalid bearer token")
		return auth.SSOJWTPrincipal{}, false
	}
	if !s.Config.SSOJWTAllowMissingScope && !principal.HasScope("tunnel") {
		writeError(w, http.StatusForbidden, "tunnel scope required")
		return auth.SSOJWTPrincipal{}, false
	}
	return principal, true
}

func (s *Server) addRegistrationEvent(result store.RegisterDesktopDeviceResult) {
	eventType := "desktop_device.updated"
	message := "Desktop device updated"
	if result.Created {
		eventType = "desktop_device.registered"
		message = "Desktop device registered"
	}
	if err := s.DB.AddEvent(context.Background(), eventType, message, result.Device.PublicHost); err != nil {
		s.Logger.Error("add desktop device event", "error", err)
	}
}

func (s *Server) registrationResponse(result store.RegisterDesktopDeviceResult) registerResponse {
	publicHost := result.Device.PublicHost
	return registerResponse{
		DeviceID:     result.Device.DeviceID,
		PublicHost:   publicHost,
		PublicURL:    "https://" + publicHost,
		WebSocketURL: "wss://" + publicHost + "/ws",
		RelayURL:     s.relayURL(),
	}
}

func (s *Server) relayURL() string {
	if configured := strings.TrimSpace(s.Config.RelayPublicURL); configured != "" {
		return configured
	}
	return "wss://" + s.baseDomain() + "/tunnel"
}

func (s *Server) webAppResponse(result store.RegisterDesktopWebAppResult) webAppResponse {
	publicHost := result.WebApp.PublicHost
	return webAppResponse{
		DeviceID:   result.Device.DeviceID,
		Name:       result.WebApp.Name,
		PublicHost: publicHost,
		PublicURL:  "https://" + publicHost,
		TargetURL:  result.WebApp.TargetURL,
		RouteID:    result.WebApp.RouteID,
		Active:     result.WebApp.Active,
	}
}

func (s *Server) randomDesktopPublicHost() (string, error) {
	label, err := randomPublicLabel()
	if err != nil {
		return "", err
	}
	return label + "." + s.desktopPublicBaseDomain(), nil
}

func (s *Server) randomWebAppPublicHost() (string, error) {
	label, err := randomPublicLabel()
	if err != nil {
		return "", err
	}
	return tunnel.BuildWebAppPublicHost(label, s.webAppPublicBaseDomain())
}

func (s *Server) baseDomain() string {
	return strings.TrimPrefix(tunnelHost(s.Config.PublicBaseDomain), ".")
}

func (s *Server) desktopPublicBaseDomain() string {
	return strings.TrimPrefix(tunnelHost(s.Config.DesktopPublicBaseDomain), ".")
}

func (s *Server) webAppPublicBaseDomain() string {
	return strings.TrimPrefix(tunnelHost(s.Config.WebAppPublicBaseDomain), ".")
}

func randomPublicLabel() (string, error) {
	raw := make([]byte, publicLabelRandomBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)
	return strings.ToLower(encoded), nil
}

func (s *Server) writeInternal(w http.ResponseWriter, message string, err error) {
	s.Logger.Error(message, "error", err)
	writeError(w, http.StatusInternalServerError, message)
}

type registerPayload struct {
	DeviceID   string `json:"deviceId"`
	DeviceName string `json:"deviceName"`
}

func (p registerPayload) Validate() error {
	return tunnel.ValidateDesktopDeviceID(p.DeviceID)
}

type registerResponse struct {
	DeviceID     string `json:"deviceId"`
	PublicHost   string `json:"publicHost"`
	PublicURL    string `json:"publicUrl"`
	WebSocketURL string `json:"webSocketUrl"`
	RelayURL     string `json:"relayUrl"`
}

type webAppPayload struct {
	TargetURL string `json:"targetUrl"`
	Active    *bool  `json:"active"`
}

func (p webAppPayload) Validate() error {
	if strings.TrimSpace(p.TargetURL) == "" {
		return errors.New("targetUrl is required")
	}
	return validateTargetURL(p.TargetURL)
}

func (p webAppPayload) active() bool {
	if p.Active == nil {
		return true
	}
	return *p.Active
}

type webAppResponse struct {
	DeviceID   string `json:"deviceId"`
	Name       string `json:"name"`
	PublicHost string `json:"publicHost"`
	PublicURL  string `json:"publicUrl"`
	TargetURL  string `json:"targetUrl"`
	RouteID    string `json:"routeId"`
	Active     bool   `json:"active"`
}

func validateWebAppName(name string) error {
	if name == "" {
		return errors.New("webapp name is required")
	}
	if len(name) > 63 {
		return errors.New("webapp name must be 63 characters or fewer")
	}
	if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
		return errors.New("webapp name cannot start or end with hyphen")
	}
	for _, char := range name {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '-' {
			continue
		}
		return errors.New("webapp name must contain only lowercase letters, numbers, and hyphens")
	}
	return nil
}

func parseWebAppPath(path string) (string, string, bool) {
	const prefix = "/api/desktop/devices/"
	rest := strings.TrimPrefix(path, prefix)
	if rest == path {
		return "", "", false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 3 || parts[1] != "webapps" || parts[0] == "" || parts[2] == "" {
		return "", "", false
	}
	return parts[0], parts[2], true
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

func tunnelHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	host = strings.TrimSuffix(host, ".")
	return host
}

func decodeJSON(r *http.Request, value any) error {
	defer r.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(r.Body, (1<<20)+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("request body must contain one JSON object")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
