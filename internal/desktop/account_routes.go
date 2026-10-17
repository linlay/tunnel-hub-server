package desktop

import (
	"errors"
	"net/http"
	"strings"

	"example.invalid/tunnel-hub-server/internal/auth"
	"example.invalid/tunnel-hub-server/internal/proxy"
	"example.invalid/tunnel-hub-server/internal/store"
	"example.invalid/tunnel-hub-server/internal/tunnel"
)

func (s *Server) authorizeAccountApp(w http.ResponseWriter, r *http.Request) (auth.SSOJWTPrincipal, bool) {
	return s.authorizeRegistration(w, r)
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
	}
	if decodeJSON(r, &input) != nil || tunnel.ValidateDesktopDeviceID(input.TargetDesktopID) != nil {
		writeError(w, 400, "invalid credential request")
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
	credential, ok := s.manager.PlatformCredentialFor(proxy.DesktopConnectionKey(device.DeviceKey), principal.UserID, device.DeviceID, s.now())
	if !ok {
		writeError(w, 503, "Platform credential unavailable")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{"schemaVersion": 1, "credential": credential.Token, "expiresAt": credential.ExpiresAt, "platformDeviceId": credential.PlatformDeviceID, "webSocketUrl": "wss://" + device.PublicHost + "/ws", "tokenMode": "subprotocol"})
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
	if s.manager != nil {
		if active, online := s.manager.ActiveFor(proxy.DesktopConnectionKey(device.DeviceKey)); online {
			if err := s.manager.CloseSession(active.SessionID); err != nil && !errors.Is(err, proxy.ErrNoTunnel) {
				s.Logger.Error("close revoked Desktop tunnel", "error", err)
			}
		}
	}
	w.WriteHeader(http.StatusNoContent)
}
