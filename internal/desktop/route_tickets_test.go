package desktop

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"example.invalid/tunnel-hub-server/internal/proxy"
	"github.com/hashicorp/yamux"
)

func mockAccountSession(t *testing.T, s *Server, token string) *int {
	t.Helper()
	s.Config.IdentityAPIBaseURL = "https://identity.example.test"
	status := 200
	previous := identityHTTP
	t.Cleanup(func() { identityHTTP = previous })
	identityHTTP = &http.Client{Transport: identityTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != "GET" || r.URL.Path != "/api/auth/me" || r.Header.Get("Authorization") != "Bearer "+token {
			t.Fatal("incorrect session verification boundary")
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(`{"schemaVersion":1,"accountId":"42","deviceId":"11111111-1111-4111-8111-111111111111"}`)), Header: make(http.Header)}, nil
	})}
	return &status
}

func connectAccountDesktop(t *testing.T, s *Server, key string) *yamux.Session {
	t.Helper()
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
	s.manager.SetActive(&proxy.ActiveTunnel{SessionID: "session", Key: proxy.DesktopConnectionKey(key), Yamux: client, ConnectedAt: time.Now()})
	return client
}

func TestHubRouteTicketsAndDeviceRevocation(t *testing.T) {
	s, db := newDesktopTestServer(t)
	token := accountMobileJWT(t, defaultDesktopPrivateKey)
	status := mockAccountSession(t, s, token)
	s.SetPresenceManager(proxy.NewManager())
	if rr := performRegister(t, s, desktopRegisterBody("desktop-a", "", false), defaultDesktopJWT); rr.Code != 200 {
		t.Fatal(rr.Body.String())
	}
	device, err := db.GetDesktopDeviceByOwnerAndID(t.Context(), "42", "desktop-a")
	if err != nil {
		t.Fatal(err)
	}
	client := connectAccountDesktop(t, s, device.DeviceKey)
	issue := func() *httptest.ResponseRecorder {
		return accountAPI(t, s, "POST", "/api/desktop/route-credentials", token, `{"targetDesktopId":"desktop-a","requestId":"22222222-2222-4222-8222-222222222222"}`)
	}
	rr := issue()
	if rr.Code != 200 {
		t.Fatalf("issue: %d %s", rr.Code, rr.Body.String())
	}
	var result routeResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(result.Credential, "tr_") || result.RouteContext.ExpiresAt.Sub(result.RouteContext.IssuedAt) > 2*time.Minute {
		t.Fatal("invalid short-lived ticket")
	}
	validate := func(credential, target string) *httptest.ResponseRecorder {
		return accountAPI(t, s, "POST", "/api/desktop/route-credentials/validate", defaultDesktopJWT, `{"credential":"`+credential+`","targetDesktopId":"`+target+`"}`)
	}
	for range 2 {
		if rr := validate(result.Credential, "desktop-a"); rr.Code != 200 {
			t.Fatal("HTTP revalidation failed", rr.Code)
		}
	}
	if rr := validate("tr_forged", "desktop-a"); rr.Code != 403 {
		t.Fatal("forged ticket accepted")
	}
	if rr := validate(result.Credential, "other-desktop"); rr.Code != 403 {
		t.Fatal("wrong target accepted")
	}
	other := signTestSSOJWT(t, defaultDesktopPrivateKey, testSSOJWTClaims{Issuer: s.Config.SSOJWTIssuer, Audience: s.Config.SSOJWTAudience, UserID: "43", Scope: "tunnel", Expires: time.Now().Add(time.Hour)})
	if rr := accountAPI(t, s, "POST", "/api/desktop/route-credentials/validate", other, `{"credential":"`+result.Credential+`","targetDesktopId":"desktop-a"}`); rr.Code != 403 {
		t.Fatal("cross-account ticket accepted", rr.Code)
	}
	*status = 401
	if rr := validate(result.Credential, "desktop-a"); rr.Code != 401 {
		t.Fatal("revoked phone accepted", rr.Code)
	}
	*status = 503
	if rr := accountAPI(t, s, "GET", "/api/desktop/account-devices", token, ""); rr.Code != 503 {
		t.Fatal("Identity outage appeared as empty directory")
	}
	*status = 200
	s.routeTickets = nil
	if rr := validate(result.Credential, "desktop-a"); rr.Code != 403 {
		t.Fatal("ticket survived restart")
	}
	rr = issue()
	if rr.Code != 200 {
		t.Fatal("could not recover after restart")
	}
	json.Unmarshal(rr.Body.Bytes(), &result)
	s.now = func() time.Time { return result.RouteContext.ExpiresAt }
	if rr := validate(result.Credential, "desktop-a"); rr.Code != 403 {
		t.Fatal("expired ticket accepted", rr.Code)
	}
	s.now = time.Now
	if rr := accountAPI(t, s, "PATCH", "/api/desktop/account-devices/desktop-a", token, `{"deviceName":"中文电脑"}`); rr.Code != 204 {
		t.Fatal("rename failed", rr.Code)
	}
	if rr := performRegister(t, s, desktopRegisterBody("desktop-a", "", false), defaultDesktopJWT); rr.Code != 200 {
		t.Fatal("reregister failed", rr.Code)
	}
	updated, err := db.GetDesktopDeviceByOwnerAndID(t.Context(), "42", "desktop-a")
	if err != nil || updated.DeviceName != "中文电脑" {
		t.Fatal("reconnect undid account rename", err)
	}
	if rr := accountAPI(t, s, "DELETE", "/api/desktop/account-devices/unknown", token, ""); rr.Code != 404 {
		t.Fatal("unknown device revoked")
	}
	rr = issue()
	if rr.Code != 200 {
		t.Fatal("new ticket failed")
	}
	json.Unmarshal(rr.Body.Bytes(), &result)
	if rr := accountAPI(t, s, "DELETE", "/api/desktop/account-devices/desktop-a", token, ""); rr.Code != 204 {
		t.Fatal("revoke failed", rr.Code)
	}
	if !client.IsClosed() {
		t.Fatal("revocation did not close tunnel")
	}
	if rr := validate(result.Credential, "desktop-a"); rr.Code != 403 {
		t.Fatal("revoked Desktop accepted")
	}
	if rr := performRegister(t, s, desktopRegisterBody("desktop-a", "", false), defaultDesktopJWT); rr.Code != 403 {
		t.Fatal("automatic registration restored revoked device", rr.Code)
	}
	if devices, err := db.ListDesktopDevicesByOwner(t.Context(), "42"); err != nil || len(devices) != 0 {
		t.Fatal("revoked device listed", err)
	}
}

func TestRouteTicketCacheIsBoundedAndReclaimsExpiredEntries(t *testing.T) {
	s, _ := newDesktopTestServer(t)
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	for range routeTicketCapacity {
		if _, err := s.createRouteTicket(routeTicket{Context: routeContext{ExpiresAt: now.Add(time.Second)}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.createRouteTicket(routeTicket{Context: routeContext{ExpiresAt: now.Add(time.Second)}}); err == nil {
		t.Fatal("unbounded ticket cache")
	}
	now = now.Add(time.Second)
	if _, err := s.createRouteTicket(routeTicket{Context: routeContext{ExpiresAt: now.Add(time.Second)}}); err != nil {
		t.Fatal("expired capacity not reclaimed", err)
	}
}
