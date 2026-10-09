package desktop

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"example.invalid/tunnel-hub-server/internal/auth"
	"example.invalid/tunnel-hub-server/internal/proxy"
)

var identityHTTP = &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func (s *Server) identityRequest(w http.ResponseWriter, r *http.Request, path string, payload any, out any) bool {
	base, err := url.Parse(strings.TrimSpace(s.Config.IdentityAPIBaseURL))
	if err != nil || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || (base.Scheme != "https" && !(base.Scheme == "http" && (base.Hostname() == "localhost" || net.ParseIP(base.Hostname()) != nil && net.ParseIP(base.Hostname()).IsLoopback()))) {
		writeError(w, http.StatusServiceUnavailable, "Identity API is not configured")
		return false
	}
	body, err := json.Marshal(payload)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return false
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, strings.TrimRight(base.String(), "/")+path, bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "Identity API unavailable")
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", r.Header.Get("Authorization"))
	response, err := identityHTTP.Do(req)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "Identity validation unavailable")
		return false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		status := response.StatusCode
		if status != 400 && status != 401 && status != 403 {
			status = http.StatusServiceUnavailable
		}
		writeError(w, status, "Identity validation refused")
		return false
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(out); err != nil {
		writeError(w, http.StatusBadGateway, "invalid Identity response")
		return false
	}
	return true
}

func (s *Server) authorizeAccountApp(w http.ResponseWriter, r *http.Request) (auth.SSOJWTPrincipal, bool) {
	token := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	principal, err := s.ssoJWT.VerifyAccountApp(token, s.now())
	if err != nil || principal.DeviceID == "" || principal.DeviceKind != "mobile" || !principal.HasScope("app") || !principal.HasScope("tunnel") {
		writeError(w, http.StatusUnauthorized, "account mobile session required")
		return auth.SSOJWTPrincipal{}, false
	}
	return principal, true
}

func (s *Server) handleAccountPresence(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	principal, ok := s.authorizeAccountApp(w, r)
	if !ok {
		return
	}
	if s.manager == nil {
		writeError(w, http.StatusServiceUnavailable, "presence unavailable")
		return
	}
	var valid struct {
		AccountID      string `json:"accountId"`
		SourceDeviceID string `json:"sourceDeviceId"`
		SchemaVersion  int    `json:"schemaVersion"`
	}
	if !s.identityRequest(w, r, "/api/auth/account-devices/validate", map[string]any{}, &valid) {
		return
	}
	if valid.SchemaVersion != 1 || valid.AccountID != principal.UserID || valid.SourceDeviceID != principal.DeviceID {
		writeError(w, http.StatusForbidden, "account session mismatch")
		return
	}
	devices, err := s.DB.ListDesktopDevicesByOwner(r.Context(), principal.UserID)
	if err != nil {
		s.writeInternal(w, "list account Desktops", err)
		return
	}
	items := make([]map[string]any, 0, len(devices))
	for _, device := range devices {
		active, online := s.manager.ActiveFor(proxy.DesktopConnectionKey(device.DeviceKey))
		item := map[string]any{"deviceId": device.DeviceID, "online": online}
		if online {
			item["connectedAt"] = active.ConnectedAt.UTC()
			item["lastConnectedAt"] = active.ConnectedAt.UTC()
		}
		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"schemaVersion": 1, "items": items})
}

type routeContext struct {
	AccountID       string    `json:"accountId"`
	SourceDeviceID  string    `json:"sourceDeviceId"`
	TargetDesktopID string    `json:"targetDesktopId"`
	RequestID       string    `json:"requestId"`
	Issuer          string    `json:"issuer"`
	Audience        string    `json:"audience"`
	JTI             string    `json:"jti"`
	IssuedAt        time.Time `json:"issuedAt"`
	ExpiresAt       time.Time `json:"expiresAt"`
}

type routeResponse struct {
	SchemaVersion int          `json:"schemaVersion"`
	Credential    string       `json:"credential,omitempty"`
	RouteContext  routeContext `json:"routeContext"`
	WebSocketURL  string       `json:"webSocketUrl,omitempty"`
	TokenMode     string       `json:"tokenMode,omitempty"`
}

func (s *Server) handleRouteCredential(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	principal, ok := s.authorizeAccountApp(w, r)
	if !ok {
		return
	}
	var input struct {
		TargetDesktopID string `json:"targetDesktopId"`
		RequestID       string `json:"requestId"`
	}
	if decodeJSON(r, &input) != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	device, err := s.DB.GetDesktopDeviceByOwnerAndID(r.Context(), principal.UserID, input.TargetDesktopID)
	if err != nil {
		writeError(w, http.StatusForbidden, "target Desktop unavailable")
		return
	}
	if s.manager == nil {
		writeError(w, http.StatusServiceUnavailable, "presence unavailable")
		return
	}
	if _, online := s.manager.ActiveFor(proxy.DesktopConnectionKey(device.DeviceKey)); !online {
		writeError(w, http.StatusConflict, "Desktop is offline")
		return
	}
	var result routeResponse
	if !s.identityRequest(w, r, "/api/auth/desktop-route", input, &result) {
		return
	}
	context := result.RouteContext
	if result.SchemaVersion != 1 || result.Credential == "" || context.AccountID != principal.UserID || context.SourceDeviceID != principal.DeviceID || context.TargetDesktopID != input.TargetDesktopID || context.RequestID != input.RequestID || context.Audience != "desktop-route" || context.Issuer != s.Config.SSOJWTIssuer || !context.ExpiresAt.After(s.now()) || context.ExpiresAt.Sub(context.IssuedAt) > 2*time.Minute {
		writeError(w, http.StatusBadGateway, "route identity mismatch")
		return
	}
	result.WebSocketURL = "wss://" + device.PublicHost + "/ws"
	result.TokenMode = "subprotocol"
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleRouteValidation(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	principal, ok := s.authorizeRegistration(w, r)
	if !ok {
		return
	}
	var input struct {
		Credential      string `json:"credential"`
		TargetDesktopID string `json:"targetDesktopId"`
	}
	if decodeJSON(r, &input) != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if _, err := s.DB.GetDesktopDeviceByOwnerAndID(r.Context(), principal.UserID, input.TargetDesktopID); err != nil {
		writeError(w, http.StatusForbidden, "Desktop is not owned by this account")
		return
	}
	var result routeResponse
	if !s.identityRequest(w, r, "/api/auth/desktop-route/validate", input, &result) {
		return
	}
	if result.SchemaVersion != 1 || result.RouteContext.AccountID != principal.UserID || result.RouteContext.TargetDesktopID != input.TargetDesktopID {
		writeError(w, http.StatusBadGateway, "route identity mismatch")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleAccountDeviceRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if _, ok := s.authorizeRegistration(w, r); !ok {
		return
	}
	var input struct {
		DeviceID string `json:"deviceId"`
	}
	if decodeJSON(r, &input) != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	var result map[string]any
	if !s.identityRequest(w, r, "/api/auth/desktop-account-device", map[string]any{"deviceId": input.DeviceID, "revoke": true}, &result) {
		return
	}
	writeJSON(w, http.StatusOK, result)
}
