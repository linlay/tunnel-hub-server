package desktop

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/linlay/zenmind-tunnel-server/internal/auth"
	"github.com/linlay/zenmind-tunnel-server/internal/config"
	"github.com/linlay/zenmind-tunnel-server/internal/store"
)

const registerPath = "/api/desktop/devices/register"

type Server struct {
	DB     *store.DB
	Config config.RelayConfig
	Logger *slog.Logger
	ssoJWT *auth.SSOJWTVerifier
}

func NewServer(db *store.DB, cfg config.RelayConfig, logger *slog.Logger) *Server {
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
		logger.Error("configure SSO JWT verifier", "error", err)
	}
	return &Server{DB: db, Config: cfg, Logger: logger, ssoJWT: ssoJWT}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.URL.Path != registerPath {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	s.handleRegister(w, r)
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeRegistration(w, r) {
		return
	}
	var payload registerPayload
	if err := decodeJSON(r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	payload.DeviceID = strings.TrimSpace(payload.DeviceID)
	payload.TargetURL = strings.TrimSpace(payload.TargetURL)
	if err := payload.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	publicHost := s.devicePublicHost(payload.DeviceID)
	result, err := s.DB.RegisterDesktopDevice(r.Context(), store.RegisterDesktopDeviceInput{
		DeviceID:     payload.DeviceID,
		DeviceSecret: payload.DeviceSecret,
		PublicHost:   publicHost,
		TargetURL:    payload.TargetURL,
		RotateToken:  payload.RotateToken,
	})
	if errors.Is(err, store.ErrInvalidDesktopDeviceSecret) {
		writeError(w, http.StatusForbidden, "invalid deviceSecret")
		return
	}
	if errors.Is(err, store.ErrDesktopDeviceHostConflict) {
		writeError(w, http.StatusConflict, "device host already exists")
		return
	}
	if err != nil {
		s.writeInternal(w, "register desktop device", err)
		return
	}
	s.addRegistrationEvent(result)
	writeJSON(w, http.StatusOK, s.registrationResponse(result))
}

func (s *Server) authorizeRegistration(w http.ResponseWriter, r *http.Request) bool {
	if _, ok := s.ssoJWT.VerifyBearerHeader(r.Header.Get("Authorization")); ok {
		return true
	}
	secret := strings.TrimSpace(s.Config.DesktopRegistrationSecret)
	token := bearerToken(r.Header.Get("Authorization"))
	if token != "" && secret == "" {
		writeError(w, http.StatusUnauthorized, "invalid registration token")
		return false
	}
	if secret == "" {
		writeError(w, http.StatusServiceUnavailable, "desktop registration is disabled")
		return false
	}
	if token == "" || !hmac.Equal([]byte(token), []byte(secret)) {
		writeError(w, http.StatusUnauthorized, "invalid registration token")
		return false
	}
	return true
}

func (s *Server) addRegistrationEvent(result store.RegisterDesktopDeviceResult) {
	eventType := "desktop_device.updated"
	message := "Desktop device updated"
	if result.Created {
		eventType = "desktop_device.registered"
		message = "Desktop device registered"
	} else if result.Rotated {
		eventType = "desktop_device.token_rotated"
		message = "Desktop device token rotated"
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
		RelayURL:     "wss://" + s.baseDomain() + "/tunnel",
		TargetURL:    result.Device.TargetURL,
		TokenID:      result.Token.ID,
		AgentToken:   result.AgentToken,
		Created:      result.Created,
		Rotated:      result.Rotated,
	}
}

func (s *Server) devicePublicHost(deviceID string) string {
	return deviceID + "." + s.baseDomain()
}

func (s *Server) baseDomain() string {
	baseDomain := strings.TrimPrefix(tunnelHost(s.Config.PublicBaseDomain), ".")
	if baseDomain == "" {
		baseDomain = "tunnel-hub.zenmind.cc"
	}
	return baseDomain
}

func (s *Server) writeInternal(w http.ResponseWriter, message string, err error) {
	s.Logger.Error(message, "error", err)
	writeError(w, http.StatusInternalServerError, message)
}

type registerPayload struct {
	DeviceID     string `json:"deviceId"`
	DeviceSecret string `json:"deviceSecret"`
	TargetURL    string `json:"targetUrl"`
	RotateToken  bool   `json:"rotateToken"`
}

func (p registerPayload) Validate() error {
	if err := validateDeviceID(p.DeviceID); err != nil {
		return err
	}
	if strings.TrimSpace(p.DeviceSecret) == "" {
		return errors.New("deviceSecret is required")
	}
	if strings.TrimSpace(p.TargetURL) == "" {
		return errors.New("targetUrl is required")
	}
	return validateTargetURL(p.TargetURL)
}

type registerResponse struct {
	DeviceID     string `json:"deviceId"`
	PublicHost   string `json:"publicHost"`
	PublicURL    string `json:"publicUrl"`
	WebSocketURL string `json:"webSocketUrl"`
	RelayURL     string `json:"relayUrl"`
	TargetURL    string `json:"targetUrl"`
	TokenID      string `json:"tokenId"`
	AgentToken   string `json:"agentToken,omitempty"`
	Created      bool   `json:"created"`
	Rotated      bool   `json:"rotated"`
}

func validateDeviceID(deviceID string) error {
	if deviceID == "" {
		return errors.New("deviceId is required")
	}
	if deviceID != strings.ToLower(deviceID) {
		return errors.New("deviceId must be lowercase")
	}
	if len(deviceID) > 63 {
		return errors.New("deviceId must be 63 characters or fewer")
	}
	if strings.HasPrefix(deviceID, "-") || strings.HasSuffix(deviceID, "-") {
		return errors.New("deviceId cannot start or end with hyphen")
	}
	if reservedDeviceIDs[deviceID] {
		return errors.New("deviceId is reserved")
	}
	for _, char := range deviceID {
		if char >= 97 && char <= 122 {
			continue
		}
		if char >= 48 && char <= 57 {
			continue
		}
		if char == 45 {
			continue
		}
		return errors.New("deviceId must contain only lowercase letters, numbers, and hyphens")
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

func tunnelHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	host = strings.TrimSuffix(host, ".")
	return host
}

func bearerToken(header string) string {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(header, prefix))
}

var reservedDeviceIDs = map[string]bool{
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
