package desktop

import (
	"encoding/base64"
	"encoding/json"
	"example.invalid/tunnel-hub-server/internal/proxy"
	"example.invalid/tunnel-hub-server/internal/tunnel"
	"github.com/hashicorp/yamux"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func accountAPI(t *testing.T, s *Server, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}
func platformFixture(device string) tunnel.PlatformCredential {
	expiry := time.Now().Add(time.Hour).Truncate(time.Second)
	raw, _ := json.Marshal(map[string]any{"scope": "app", "device_id": device, "exp": expiry.Unix()})
	return tunnel.PlatformCredential{Token: "header." + base64.RawURLEncoding.EncodeToString(raw) + ".signature", PlatformDeviceID: device, ExpiresAt: expiry}
}
func liveAccountDesktop(t *testing.T, s *Server, id, sessionID string, credential *tunnel.PlatformCredential) {
	t.Helper()
	device, err := s.DB.GetDesktopDeviceByOwnerAndID(t.Context(), "42", id)
	if err != nil {
		t.Fatal(err)
	}
	a, b := net.Pipe()
	client, err := yamux.Client(a)
	if err != nil {
		t.Fatal(err)
	}
	server, err := yamux.Server(b)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close(); server.Close() })
	s.manager.SetActive(&proxy.ActiveTunnel{SessionID: sessionID, Key: proxy.DesktopConnectionKey(device.DeviceKey), OwnerUserID: "42", DesktopID: id, PlatformCredential: credential, Yamux: client, ConnectedAt: time.Now()})
}
func TestAccountCredentialUsesUploadedIdentityAndIsolatesDesktops(t *testing.T) {
	s, _ := newDesktopTestServer(t)
	s.SetPresenceManager(proxy.NewManager())
	a, b := platformFixture("local-a"), platformFixture("local-b")
	for _, d := range []struct {
		id         string
		credential *tunnel.PlatformCredential
	}{{"desktop-a", &a}, {"desktop-b", &b}} {
		if rec := performRegister(t, s, desktopRegisterBody(d.id, "", false), defaultDesktopJWT); rec.Code != 200 {
			t.Fatal(rec.Code)
		}
		liveAccountDesktop(t, s, d.id, d.id, d.credential)
		rec := accountAPI(t, s, "POST", "/api/desktop/route-credentials", defaultDesktopJWT, `{"targetDesktopId":"`+d.id+`"}`)
		if rec.Code != 200 {
			t.Fatalf("credential status %d: %s", rec.Code, rec.Body.String())
		}
		var body map[string]any
		json.Unmarshal(rec.Body.Bytes(), &body)
		if body["credential"] != d.credential.Token || body["platformDeviceId"] != d.credential.PlatformDeviceID || body["routeContext"] != nil {
			t.Fatal("wrong Desktop credential")
		}
	}
	rec := accountAPI(t, s, "GET", "/api/desktop/account-devices", defaultDesktopJWT, "")
	if rec.Code != 200 || strings.Contains(rec.Body.String(), a.Token) || strings.Contains(rec.Body.String(), b.Token) {
		t.Fatal("directory exposed credentials")
	}
	if rec = accountAPI(t, s, "POST", "/api/desktop/route-credentials", defaultDesktopJWT, `{"targetDesktopId":"unknown-desktop"}`); rec.Code != 403 {
		t.Fatal("unknown owner target accepted")
	}
}
func TestAccountCredentialDistinguishesOfflineMissingAndExpired(t *testing.T) {
	s, _ := newDesktopTestServer(t)
	s.SetPresenceManager(proxy.NewManager())
	performRegister(t, s, desktopRegisterBody("desktop-a", "", false), defaultDesktopJWT)
	read := func(want int) {
		t.Helper()
		rec := accountAPI(t, s, "POST", "/api/desktop/route-credentials", defaultDesktopJWT, `{"targetDesktopId":"desktop-a"}`)
		if rec.Code != want {
			t.Fatalf("status %d want %d", rec.Code, want)
		}
	}
	read(409)
	liveAccountDesktop(t, s, "desktop-a", "missing", nil)
	read(503)
	expired := platformFixture("local-a")
	expired.ExpiresAt = time.Now().Add(-time.Second)
	liveAccountDesktop(t, s, "desktop-a", "expired", &expired)
	read(503)
}
func TestPlatformUpdateRejectsStaleSessionAndRevocationWithoutRegistration(t *testing.T) {
	s, db := newDesktopTestServer(t)
	s.SetPresenceManager(proxy.NewManager())
	performRegister(t, s, desktopRegisterBody("desktop-a", "", false), defaultDesktopJWT)
	old, next := platformFixture("local-a"), platformFixture("local-a")
	next.ExpiresAt = next.ExpiresAt.Add(time.Minute)
	raw, _ := json.Marshal(map[string]any{"scope": "app", "device_id": "local-a", "exp": next.ExpiresAt.Unix()})
	next.Token = "header." + base64.RawURLEncoding.EncodeToString(raw) + ".signature"
	liveAccountDesktop(t, s, "desktop-a", "current", &old)
	update := func(id, session string, want int) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"deviceId": id, "tunnelSessionId": session, "platformCredential": next})
		rec := accountAPI(t, s, "POST", registerPath, defaultDesktopJWT, string(body))
		if rec.Code != want {
			t.Fatalf("update %d want %d: %s", rec.Code, want, rec.Body.String())
		}
	}
	update("desktop-a", "stale", 409)
	device, _ := db.GetDesktopDeviceByOwnerAndID(t.Context(), "42", "desktop-a")
	cred, _ := s.manager.PlatformCredentialFor(proxy.DesktopConnectionKey(device.DeviceKey), "42", "desktop-a", time.Now())
	if cred.Token != old.Token {
		t.Fatal("failed update partially replaced credential")
	}
	update("desktop-a", "current", 200)
	s.manager.Clear("stale")
	cred, ok := s.manager.PlatformCredentialFor(proxy.DesktopConnectionKey(device.DeviceKey), "42", "desktop-a", time.Now())
	if !ok || cred.Token != next.Token {
		t.Fatal("current update missing")
	}
	update("never-registered", "current", 403)
	if err := db.RevokeAccountDesktop(t.Context(), "42", "desktop-a"); err != nil {
		t.Fatal(err)
	}
	update("desktop-a", "current", 403)
}
func TestAccountDirectoryRejectsMissingOrForeignProof(t *testing.T) {
	s, _ := newDesktopTestServer(t)
	s.SetPresenceManager(proxy.NewManager())
	performRegister(t, s, desktopRegisterBody("desktop-a", "", false), defaultDesktopJWT)
	if rec := accountAPI(t, s, "GET", "/api/desktop/account-devices", "", ""); rec.Code != 401 {
		t.Fatal(rec.Code)
	}
	other := signTestSSOJWT(t, defaultDesktopPrivateKey, testSSOJWTClaims{Issuer: "https://official.example.test", Audience: "tunnel-hub-server", UserID: "other", Scope: "tunnel", Expires: time.Now().Add(time.Hour)})
	if rec := accountAPI(t, s, "POST", "/api/desktop/route-credentials", other, `{"targetDesktopId":"desktop-a"}`); rec.Code != 403 {
		t.Fatal("cross-user credential accepted")
	}
}
