package desktop

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"example.invalid/tunnel-hub-server/internal/auth"
	"example.invalid/tunnel-hub-server/internal/proxy"
	"example.invalid/tunnel-hub-server/internal/store"
	"example.invalid/tunnel-hub-server/internal/tunnel"
	"github.com/google/uuid"
)

var identityHTTP = &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func bearerCredential(r *http.Request) string {
	parts := strings.Fields(r.Header.Get("Authorization"))
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return parts[1]
}

func (s *Server) validateAccountSession(w http.ResponseWriter, r *http.Request, token, accountID, sessionID string) bool {
	base, err := url.Parse(strings.TrimSpace(s.Config.IdentityAPIBaseURL))
	if err != nil || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || (base.Scheme != "https" && !(base.Scheme == "http" && (base.Hostname() == "localhost" || net.ParseIP(base.Hostname()) != nil && net.ParseIP(base.Hostname()).IsLoopback()))) {
		writeError(w, 503, "Identity API is not configured")
		return false
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, strings.TrimRight(base.String(), "/")+"/api/auth/me", nil)
	if err != nil {
		writeError(w, 503, "Identity validation unavailable")
		return false
	}
	req.Header.Set("Authorization", "Bearer "+token)
	response, err := identityHTTP.Do(req)
	if err != nil {
		writeError(w, 503, "Identity validation unavailable")
		return false
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		status := http.StatusServiceUnavailable
		if response.StatusCode == 401 || response.StatusCode == 403 {
			status = response.StatusCode
		}
		writeError(w, status, "account session unavailable")
		return false
	}
	var valid struct {
		SchemaVersion int    `json:"schemaVersion"`
		AccountID     string `json:"accountId"`
		DeviceID      string `json:"deviceId"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if decoder.Decode(&valid) != nil {
		writeError(w, 502, "invalid Identity response")
		return false
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		writeError(w, http.StatusBadGateway, "invalid Identity response")
		return false
	}
	if valid.SchemaVersion != 1 || valid.AccountID != accountID || valid.DeviceID != sessionID {
		writeError(w, 403, "account session mismatch")
		return false
	}
	return true
}

func (s *Server) authorizeAccountApp(w http.ResponseWriter, r *http.Request) (auth.SSOJWTPrincipal, bool) {
	token := bearerCredential(r)
	principal, err := s.ssoJWT.VerifyAccountApp(token, s.now())
	if err != nil || uuid.Validate(principal.DeviceID) != nil || !principal.HasScope("app") || !principal.HasScope("tunnel") {
		writeError(w, 401, "account session required")
		return auth.SSOJWTPrincipal{}, false
	}
	if !s.validateAccountSession(w, r, token, principal.UserID, principal.DeviceID) {
		return auth.SSOJWTPrincipal{}, false
	}
	return principal, true
}

func (s *Server) handleAccountDevices(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "method not allowed")
		return
	}
	principal, ok := s.authorizeAccountApp(w, r)
	if !ok {
		return
	}
	if s.manager == nil {
		writeError(w, 503, "presence unavailable")
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
		name := device.DeviceName
		if name == "" {
			name = device.DeviceID
		}
		item := map[string]any{"deviceId": device.DeviceID, "deviceName": name, "online": online}
		if online {
			item["connectedAt"] = active.ConnectedAt.UTC()
		}
		items = append(items, item)
	}
	writeJSON(w, 200, map[string]any{"schemaVersion": 1, "items": items})
}

func (s *Server) handleAccountDevice(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPatch && r.Method != http.MethodDelete {
		writeError(w, 405, "method not allowed")
		return
	}
	principal, ok := s.authorizeAccountApp(w, r)
	if !ok {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/desktop/account-devices/")
	if tunnel.ValidateDesktopDeviceID(id) != nil {
		writeError(w, 400, "invalid device id")
		return
	}
	if r.Method == http.MethodDelete {
		s.revokeAccountDesktop(w, r, principal.UserID, id)
		return
	}
	var input struct {
		DeviceName string `json:"deviceName"`
	}
	if decodeJSON(r, &input) != nil {
		writeError(w, 400, "invalid request")
		return
	}
	err := s.DB.RenameAccountDesktop(r.Context(), principal.UserID, id, input.DeviceName)
	if errors.Is(err, store.ErrInvalidDeviceName) {
		writeError(w, 400, err.Error())
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, 404, "Desktop not found")
		return
	}
	if err != nil {
		s.writeInternal(w, "rename Desktop", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
		writeError(w, 405, "method not allowed")
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
	if decodeJSON(r, &input) != nil || uuid.Validate(input.RequestID) != nil || tunnel.ValidateDesktopDeviceID(input.TargetDesktopID) != nil {
		writeError(w, 400, "invalid route request")
		return
	}
	device, err := s.DB.GetDesktopDeviceByOwnerAndID(r.Context(), principal.UserID, input.TargetDesktopID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, 403, "target Desktop unavailable")
		return
	}
	if err != nil {
		s.writeInternal(w, "resolve target Desktop", err)
		return
	}
	if s.manager == nil {
		writeError(w, 503, "presence unavailable")
		return
	}
	if _, online := s.manager.ActiveFor(proxy.DesktopConnectionKey(device.DeviceKey)); !online {
		writeError(w, 409, "Desktop is offline")
		return
	}
	now := s.now().UTC()
	expires := now.Add(routeTicketTTL)
	if principal.ExpiresAt.Before(expires) {
		expires = principal.ExpiresAt
	}
	context := routeContext{SchemaVersion: 1, AccountID: principal.UserID, SourceDeviceID: principal.DeviceID, TargetDesktopID: device.DeviceID, RequestID: input.RequestID, JTI: uuid.NewString(), IssuedAt: now, ExpiresAt: expires}
	credential, err := s.createRouteTicket(routeTicket{Context: context, SourceToken: bearerCredential(r)})
	if err != nil {
		writeError(w, 503, "route ticket unavailable")
		return
	}
	writeJSON(w, 200, routeResponse{SchemaVersion: 1, Credential: credential, RouteContext: context, WebSocketURL: "wss://" + device.PublicHost + "/ws", TokenMode: "subprotocol"})
}

func (s *Server) handleRouteValidation(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed")
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
		writeError(w, 400, "invalid request")
		return
	}
	ticket, ok := s.findRouteTicket(input.Credential)
	if !ok || ticket.Context.AccountID != principal.UserID || ticket.Context.TargetDesktopID != input.TargetDesktopID {
		writeError(w, 403, "route ticket invalid")
		return
	}
	if _, err := s.DB.GetDesktopDeviceByOwnerAndID(r.Context(), principal.UserID, input.TargetDesktopID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, 403, "Desktop is inactive")
		} else {
			s.writeInternal(w, "validate Desktop", err)
		}
		return
	}
	if !s.validateAccountSession(w, r, ticket.SourceToken, ticket.Context.AccountID, ticket.Context.SourceDeviceID) {
		return
	}
	writeJSON(w, 200, routeResponse{SchemaVersion: 1, RouteContext: ticket.Context})
}

func (s *Server) revokeAccountDesktop(w http.ResponseWriter, r *http.Request, accountID, deviceID string) {
	device, err := s.DB.GetDesktopDeviceByOwnerAndID(r.Context(), accountID, deviceID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, 404, "Desktop not found")
		return
	}
	if err != nil {
		s.writeInternal(w, "resolve Desktop revocation", err)
		return
	}
	if err := s.DB.RevokeAccountDesktop(r.Context(), accountID, deviceID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, 404, "Desktop not found")
		} else {
			s.writeInternal(w, "revoke Desktop", err)
		}
		return
	}
	s.revokeDesktopTickets(accountID, deviceID)
	if s.manager != nil {
		if active, online := s.manager.ActiveFor(proxy.DesktopConnectionKey(device.DeviceKey)); online {
			if err := s.manager.CloseSession(active.SessionID); err != nil && !errors.Is(err, proxy.ErrNoTunnel) {
				s.Logger.Error("close revoked Desktop tunnel", "error", err)
			}
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAccountDeviceRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed")
		return
	}
	principal, ok := s.authorizeRegistration(w, r)
	if !ok {
		return
	}
	var input struct {
		TargetDesktopID string `json:"targetDesktopId"`
	}
	if decodeJSON(r, &input) != nil || tunnel.ValidateDesktopDeviceID(input.TargetDesktopID) != nil {
		writeError(w, 400, "invalid device id")
		return
	}
	s.revokeAccountDesktop(w, r, principal.UserID, input.TargetDesktopID)
}
