package desktop

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
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

type identityTestTransport func(*http.Request) (*http.Response, error)

func (f identityTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func accountMobileJWT(t *testing.T, key *rsa.PrivateKey) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256"}`))
	raw, err := json.Marshal(map[string]any{"iss": "https://official.example.test", "aud": []string{"app", "tunnel"}, "sub": "alice", "account_id": "42", "device_id": "11111111-1111-4111-8111-111111111111", "device_kind": "mobile", "scope": "app tunnel", "exp": time.Now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	input := header + "." + base64.RawURLEncoding.EncodeToString(raw)
	digest := sha256.Sum256([]byte(input))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(signature)
}
func accountAPI(t *testing.T, s *Server, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	return rr
}
func TestAccountPresenceUsesLiveManagerAndStableAccountID(t *testing.T) {
	s, db := newDesktopTestServer(t)
	registered := performRegister(t, s, desktopRegisterBody("desktop-a", "", false), defaultDesktopJWT)
	if registered.Code != 200 {
		t.Fatal(registered.Code)
	}
	device, err := db.GetDesktopDeviceByOwnerAndID(t.Context(), "42", "desktop-a")
	if err != nil {
		t.Fatal(err)
	}
	token := accountMobileJWT(t, defaultDesktopPrivateKey)
	s.Config.IdentityAPIBaseURL = "https://identity.example.test"
	manager := proxy.NewManager()
	s.SetPresenceManager(manager)
	previous := identityHTTP
	t.Cleanup(func() { identityHTTP = previous })
	identityHTTP = &http.Client{Transport: identityTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != "GET" || r.URL.Path != "/api/auth/me" || r.Header.Get("Authorization") != "Bearer "+token {
			t.Fatal("incorrect validation boundary")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"schemaVersion":1,"accountId":"42","deviceId":"11111111-1111-4111-8111-111111111111"}`)), Header: make(http.Header)}, nil
	})}
	read := func() bool {
		rr := accountAPI(t, s, "GET", "/api/desktop/account-devices", token, "")
		if rr.Code != 200 {
			t.Fatalf("status=%d %s", rr.Code, rr.Body.String())
		}
		var value struct {
			Items []struct {
				DeviceID string `json:"deviceId"`
				Online   bool   `json:"online"`
			} `json:"items"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &value); err != nil {
			t.Fatal(err)
		}
		if len(value.Items) != 1 || value.Items[0].DeviceID != "desktop-a" {
			t.Fatal("wrong account devices")
		}
		return value.Items[0].Online
	}
	if read() {
		t.Fatal("registration alone appeared online")
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
	manager.SetActive(&proxy.ActiveTunnel{SessionID: "session", Key: proxy.DesktopConnectionKey(device.DeviceKey), Yamux: client, ConnectedAt: time.Now()})
	if !read() {
		t.Fatal("live tunnel not online")
	}
	client.Close()
	if read() {
		t.Fatal("closed tunnel remained online")
	}
}
func TestAccountRoutesFailClosedForOfflineUnknownAndAuthorityFailure(t *testing.T) {
	s, _ := newDesktopTestServer(t)
	registered := performRegister(t, s, desktopRegisterBody("desktop-a", "", false), defaultDesktopJWT)
	if registered.Code != 200 {
		t.Fatal(registered.Code)
	}
	token := accountMobileJWT(t, defaultDesktopPrivateKey)
	s.Config.IdentityAPIBaseURL = "https://identity.example.test"
	s.SetPresenceManager(proxy.NewManager())
	mockAccountSession(t, s, token)
	rr := accountAPI(t, s, "POST", "/api/desktop/route-credentials", token, `{"targetDesktopId":"desktop-a","requestId":"22222222-2222-4222-8222-222222222222"}`)
	if rr.Code != 409 {
		t.Fatalf("offline status=%d", rr.Code)
	}
	rr = accountAPI(t, s, "POST", "/api/desktop/route-credentials", token, `{"targetDesktopId":"other-account-device","requestId":"22222222-2222-4222-8222-222222222222"}`)
	if rr.Code != 403 {
		t.Fatalf("cross-account status=%d", rr.Code)
	}
	previous := identityHTTP
	t.Cleanup(func() { identityHTTP = previous })
	identityHTTP = &http.Client{Transport: identityTestTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader("refused")), Header: make(http.Header)}, nil
	})}
	rr = accountAPI(t, s, "GET", "/api/desktop/account-devices", token, "")
	if rr.Code != 403 {
		t.Fatal("revoked phone still received presence")
	}
	s.Config.IdentityAPIBaseURL = ""
	rr = accountAPI(t, s, "GET", "/api/desktop/account-devices", token, "")
	if rr.Code != 503 {
		t.Fatal("unknown Identity state appeared offline")
	}
}

func TestAccountRegistrationUsesHubOwnerAndExistingDeviceID(t *testing.T) {
	s, db := newDesktopTestServer(t)
	s.Config.IdentityAPIBaseURL = "https://identity.example.test"
	previous := identityHTTP
	t.Cleanup(func() { identityHTTP = previous })
	target := "initial-desktop"
	identityHTTP = &http.Client{Transport: identityTestTransport(func(r *http.Request) (*http.Response, error) {
		t.Fatal("Desktop registration must not call Identity")
		return nil, nil
	})}
	rr := performRegister(t, s, desktopRegisterBody("initial-desktop", "", false), defaultDesktopJWT)
	if rr.Code != 200 {
		t.Fatalf("registration=%d %s", rr.Code, rr.Body.String())
	}
	var response registerResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.AccountID != "42" || response.DeviceID != target {
		t.Fatal("missing authority-bound registration")
	}
	if _, err := db.GetDesktopDeviceByOwnerAndID(t.Context(), "42", target); err != nil {
		t.Fatal(err)
	}
	s.Config.IdentityAPIBaseURL = ""
	rr = performRegister(t, s, desktopRegisterBody("legacy-desktop", "", false), defaultDesktopJWT)
	response = registerResponse{}
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.AccountID != "42" || response.DeviceID != "legacy-desktop" {
		t.Fatal("registration did not preserve Hub ownership and device identity")
	}
}
