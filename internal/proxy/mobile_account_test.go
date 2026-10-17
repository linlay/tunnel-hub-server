package proxy

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"example.invalid/tunnel-hub-server/internal/auth"
	"example.invalid/tunnel-hub-server/internal/tunnel"
)

func mobileCredential(device string) tunnel.PlatformCredential {
	expires := time.Now().Add(time.Hour).Truncate(time.Second)
	claims, _ := json.Marshal(map[string]any{"scope": "app", "device_id": device, "exp": expires.Unix()})
	return tunnel.PlatformCredential{Token: "header." + base64.RawURLEncoding.EncodeToString(claims) + ".signature", PlatformDeviceID: device, ExpiresAt: expires}
}

func TestMobileDataPlanesRefuseForeignAndMissingOfficialAccount(t *testing.T) {
	db := openProxyTestDB(t)
	manager := NewManager()
	relay := NewRelay(db, manager, nil, "example", "m.example.test", "example.test", 64<<20)
	registration := registerUploadDesktop(t, db, "desk.m.example.test")
	session, _ := newManagerTestSession(t)
	manager.SetActive(&ActiveTunnel{Key: DesktopConnectionKey(registration.Device.DeviceKey), SessionID: "live", OwnerUserID: "user_1", DesktopID: registration.Device.DeviceID, Yamux: session})
	foreignToken := configureProxyDesktopIdentity(t, relay, "another-user")
	for _, plane := range []string{"websocket", "upload", "resource", "webapp"} {
		for _, proof := range []string{"", foreignToken} {
			t.Run(plane+"/"+map[bool]string{true: "foreign", false: "missing"}[proof != ""], func(t *testing.T) {
				method, path, host := http.MethodGet, "/ws", registration.Device.PublicHost
				switch plane {
				case "upload":
					method, path = http.MethodPost, "/api/upload"
				case "resource":
					path = "/api/resource?file=chat_resource/note.txt"
				case "webapp":
					method, path, host = http.MethodPost, "/api/mobile-session", "desk-43210.m.example.test"
				}
				req := httptest.NewRequest(method, "https://"+host+path, nil)
				req.Header.Set("Authorization", "Bearer stolen-business-token")
				if proof != "" {
					req.Header.Set("X-Account-Authorization", "Bearer "+proof)
				}
				if plane == "websocket" {
					req.Header.Set("Upgrade", "websocket")
					req.Header.Set("Connection", "Upgrade")
				}
				rec := httptest.NewRecorder()
				switch plane {
				case "upload":
					relay.HandleUpload(rec, req)
				case "resource":
					relay.HandleResource(rec, req)
				default:
					relay.HandlePublic(rec, req)
				}
				want := 401
				if proof != "" {
					want = 403
				}
				if rec.Code != want {
					t.Fatalf("status=%d want=%d body=%s", rec.Code, want, rec.Body.String())
				}
				if len(rec.Result().Cookies()) != 0 {
					t.Fatal("unauthorized request created a cookie")
				}
			})
		}
	}
}

func TestMobileCookieSessionExpiresRevokesAndDoesNotSurviveReconnect(t *testing.T) {
	manager := NewManager()
	key := DesktopConnectionKey("device-key")
	connection, _ := newManagerTestSession(t)
	credential := mobileCredential("local-device")
	active := &ActiveTunnel{Key: key, SessionID: "old", OwnerUserID: "owner", DesktopID: "desktop", Yamux: connection, PlatformCredential: &credential}
	manager.SetActive(active)
	proof := auth.SSOJWTPrincipal{UserID: "owner", ExpiresAt: time.Now().Add(time.Minute)}
	secret, expires, ok := manager.createMobileWebAppSession(key, proof, credential.Token)
	if !ok || expires != proof.ExpiresAt || secret == credential.Token {
		t.Fatal("invalid opaque session or expiry")
	}
	if _, ok := manager.mobileWebAppSessionFor(key, "other", secret); ok {
		t.Fatal("foreign owner accepted")
	}
	if _, _, ok := manager.createMobileWebAppSession(key, proof, "stolen-token"); ok {
		t.Fatal("different business token accepted")
	}
	if token, ok := manager.mobileWebAppSessionFor(key, "owner", secret); !ok || token != credential.Token {
		t.Fatal("valid session rejected")
	}
	manager.revokeMobileWebAppSession(key, secret)
	if _, ok := manager.mobileWebAppSessionFor(key, "owner", secret); ok {
		t.Fatal("revoked cookie accepted")
	}
	secret, _, _ = manager.createMobileWebAppSession(key, proof, credential.Token)
	manager.mu.Lock()
	s := active.mobileWebAppSessions[secret]
	s.ExpiresAt = time.Now().Add(-time.Second)
	active.mobileWebAppSessions[secret] = s
	manager.mu.Unlock()
	if _, ok := manager.mobileWebAppSessionFor(key, "owner", secret); ok {
		t.Fatal("expired cookie accepted")
	}
	secret, _, _ = manager.createMobileWebAppSession(key, proof, credential.Token)
	next, _ := newManagerTestSession(t)
	manager.SetActive(&ActiveTunnel{Key: key, SessionID: "new", OwnerUserID: "owner", DesktopID: "desktop", Yamux: next, PlatformCredential: &credential})
	manager.Clear("old")
	if _, ok := manager.mobileWebAppSessionFor(key, "owner", secret); ok {
		t.Fatal("cookie survived reconnect")
	}
	if _, ok := manager.PlatformCredentialFor(key, "owner", "desktop", time.Now()); !ok {
		t.Fatal("stale clear removed the new connection")
	}
}

func TestPlatformCredentialUpdateReadAndClearAreConcurrentSafe(t *testing.T) {
	manager := NewManager()
	key := DesktopConnectionKey("device-key")
	session, _ := newManagerTestSession(t)
	credential := mobileCredential("local-device")
	manager.SetActive(&ActiveTunnel{Key: key, SessionID: "session", OwnerUserID: "owner", DesktopID: "desktop", Yamux: session, PlatformCredential: &credential})
	var group sync.WaitGroup
	for index := 0; index < 12; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for n := 0; n < 100; n++ {
				_ = manager.UpdatePlatformCredential(key, "owner", "desktop", "session", credential)
				_, _ = manager.PlatformCredentialFor(key, "owner", "desktop", time.Now())
			}
		}()
	}
	manager.Clear("session")
	group.Wait()
	if _, ok := manager.PlatformCredentialFor(key, "owner", "desktop", time.Now()); ok {
		t.Fatal("credential survived disconnect")
	}
	if err := manager.UpdatePlatformCredential(key, "owner", "desktop", "session", credential); err == nil {
		t.Fatal("late update resurrected disconnected tunnel")
	}
}
