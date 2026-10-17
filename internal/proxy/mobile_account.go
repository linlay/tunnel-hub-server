package proxy

import (
	"example.invalid/tunnel-hub-server/internal/auth"
	"example.invalid/tunnel-hub-server/internal/store"
	"net/http"
	"time"
)

type mobileWebAppSession struct {
	OwnerUserID string
	Token       string
	ExpiresAt   time.Time
}

func (r *Relay) authorizeMobileAccount(w http.ResponseWriter, req *http.Request, device store.DesktopDevice) (auth.SSOJWTPrincipal, bool) {
	if r.desktopIdentityVerifier == nil {
		http.Error(w, "account verifier unavailable", 503)
		return auth.SSOJWTPrincipal{}, false
	}
	principal, err := r.desktopIdentityVerifier.VerifyBearerHeader(req.Header.Get("X-Account-Authorization"))
	if err != nil {
		http.Error(w, "official account authentication required", 401)
		return auth.SSOJWTPrincipal{}, false
	}
	if !principal.HasScope("tunnel") || principal.UserID != device.OwnerUserID {
		http.Error(w, "Desktop access denied", 403)
		return auth.SSOJWTPrincipal{}, false
	}
	if _, online := r.Manager.ActiveFor(DesktopConnectionKey(device.DeviceKey)); !online {
		http.Error(w, "Desktop is offline", 409)
		return auth.SSOJWTPrincipal{}, false
	}
	return principal, true
}

// Cookie sessions belong to the existing live Desktop connection and disappear with it.
func (m *Manager) createMobileWebAppSession(key ConnectionKey, principal auth.SSOJWTPrincipal, token string) (string, time.Time, bool) {
	secret, err := randomUploadToken("")
	if err != nil {
		return "", time.Time{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.active[key]
	now := time.Now()
	if a == nil || a.Yamux == nil || a.Yamux.IsClosed() || a.OwnerUserID != principal.UserID || a.PlatformCredential == nil || a.PlatformCredential.Token != token || !a.PlatformCredential.ExpiresAt.After(now) {
		return "", time.Time{}, false
	}
	if a.mobileWebAppSessions == nil {
		a.mobileWebAppSessions = make(map[string]mobileWebAppSession)
	}
	for id, session := range a.mobileWebAppSessions {
		if !session.ExpiresAt.After(now) {
			delete(a.mobileWebAppSessions, id)
		}
	}
	if len(a.mobileWebAppSessions) >= 64 {
		return "", time.Time{}, false
	}
	expires := a.PlatformCredential.ExpiresAt
	if principal.ExpiresAt.Before(expires) {
		expires = principal.ExpiresAt
	}
	a.mobileWebAppSessions[secret] = mobileWebAppSession{OwnerUserID: principal.UserID, Token: token, ExpiresAt: expires}
	return secret, expires, true
}

func (m *Manager) mobileWebAppSessionFor(key ConnectionKey, owner, cookie string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	a := m.active[key]
	if a == nil || a.Yamux == nil || a.Yamux.IsClosed() || a.OwnerUserID != owner {
		return "", false
	}
	s, ok := a.mobileWebAppSessions[cookie]
	return s.Token, ok && s.OwnerUserID == owner && s.ExpiresAt.After(time.Now())
}

func (m *Manager) revokeMobileWebAppSession(key ConnectionKey, secret string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a := m.active[key]; a != nil {
		delete(a.mobileWebAppSessions, secret)
	}
}
