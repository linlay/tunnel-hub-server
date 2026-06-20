package desktop

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/linlay/zenmind-tunnel-server/internal/config"
	"github.com/linlay/zenmind-tunnel-server/internal/proxy"
	"github.com/linlay/zenmind-tunnel-server/internal/store"
)

var (
	defaultDesktopJWT        string
	defaultDesktopPrivateKey *rsa.PrivateKey
)

func TestRegisterRequiresOfficialJWT(t *testing.T) {
	server, _ := newDesktopTestServer(t)
	rec := performRegister(t, server, desktopRegisterBody("mac-mini", "http://127.0.0.1:7082", false), "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing JWT status = %d, body = %s", rec.Code, rec.Body.String())
	}

	rec = performRegister(t, server, desktopRegisterBody("mac-mini", "http://127.0.0.1:7082", false), "register-secret")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("legacy secret status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestRegisterDesktopDeviceCreatesTokenAndRoute(t *testing.T) {
	server, db := newDesktopTestServer(t)
	rec := performRegister(t, server, desktopRegisterBody("mac-mini", "http://127.0.0.1:7082", false), defaultDesktopJWT)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	response := decodeRegisterResponse(t, rec.Body)
	if !response.Created || response.Rotated {
		t.Fatalf("unexpected create flags: %+v", response)
	}
	assertDesktopPublicHost(t, response.PublicHost, "mac-mini")
	if response.WebSocketURL != "wss://"+response.PublicHost+"/ws" {
		t.Fatalf("webSocketUrl = %q", response.WebSocketURL)
	}
	if response.RelayURL != "wss://tunnel-hub.zenmind.cc/tunnel" {
		t.Fatalf("relayUrl = %q", response.RelayURL)
	}
	if !strings.HasPrefix(response.AgentToken, "zt_") || response.TokenID == "" {
		t.Fatalf("missing token fields: %+v", response)
	}

	route, err := db.GetRouteByHost(context.Background(), response.PublicHost)
	if err != nil {
		t.Fatalf("get route: %v", err)
	}
	if route.TargetURL != "http://127.0.0.1:7082" || route.TokenID != response.TokenID || !route.Active {
		t.Fatalf("unexpected route: %+v", route)
	}
	token, err := db.FindActiveTokenBySecret(context.Background(), response.AgentToken)
	if err != nil {
		t.Fatalf("find active token: %v", err)
	}
	if token.ID != response.TokenID {
		t.Fatalf("token id = %q, want %q", token.ID, response.TokenID)
	}
}

func TestRegisterDesktopDeviceAcceptsSSOJWT(t *testing.T) {
	privateKey, publicKeyPEM := testSSOJWTKey(t)
	server, db := newDesktopTestServerWithConfig(t, config.RelayConfig{
		PublicBaseDomain:        "tunnel-hub.zenmind.cc",
		DesktopPublicBaseDomain: "m.zenmind.cc",
		SSOJWTIssuer:            "https://official.example.test",
		SSOJWTPublicKeyPEM:      publicKeyPEM,
		SSOJWTAudience:          "zenmind-tunnel-hub-server",
	})
	token := signTestSSOJWT(t, privateKey, testSSOJWTClaims{
		Issuer:   "https://official.example.test",
		Audience: "zenmind-tunnel-hub-server",
		UserID:   "42",
		Email:    "desktop@example.test",
		Role:     "user",
		Scope:    "profile tunnel",
		Expires:  time.Now().Add(time.Hour),
	})

	rec := performRegister(t, server, desktopRegisterBody("jwt-device", "http://127.0.0.1:7082", false), token)
	if rec.Code != http.StatusOK {
		t.Fatalf("JWT registration status = %d body = %s", rec.Code, rec.Body.String())
	}
	response := decodeRegisterResponse(t, rec.Body)
	assertDesktopPublicHost(t, response.PublicHost, "jwt-device")
	if response.AgentToken == "" {
		t.Fatalf("unexpected JWT registration response: %+v", response)
	}
	if _, err := db.GetRouteByHost(context.Background(), response.PublicHost); err != nil {
		t.Fatalf("get JWT registered route: %v", err)
	}

	wrongAudienceToken := signTestSSOJWT(t, privateKey, testSSOJWTClaims{
		Issuer:   "https://official.example.test",
		Audience: "zenmind-market-server",
		UserID:   "42",
		Email:    "desktop@example.test",
		Role:     "user",
		Scope:    "profile tunnel",
		Expires:  time.Now().Add(time.Hour),
	})
	rec = performRegister(t, server, desktopRegisterBody("jwt-denied", "http://127.0.0.1:7082", false), wrongAudienceToken)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong audience status = %d body = %s", rec.Code, rec.Body.String())
	}

	noScopeToken := signTestSSOJWT(t, privateKey, testSSOJWTClaims{
		Issuer:   "https://official.example.test",
		Audience: "zenmind-tunnel-hub-server",
		UserID:   "42",
		Email:    "desktop@example.test",
		Role:     "user",
		Scope:    "profile market",
		Expires:  time.Now().Add(time.Hour),
	})
	rec = performRegister(t, server, desktopRegisterBody("jwt-no-scope", "http://127.0.0.1:7082", false), noScopeToken)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing scope status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func TestRegisterDesktopDeviceReusesExistingDevice(t *testing.T) {
	server, db := newDesktopTestServer(t)
	first := decodeRegisterResponse(t, performRegister(t, server, desktopRegisterBody("mac-mini", "http://127.0.0.1:7082", false), defaultDesktopJWT).Body)

	rec := performRegister(t, server, desktopRegisterBody("mac-mini", "http://127.0.0.1:7083", false), defaultDesktopJWT)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	second := decodeRegisterResponse(t, rec.Body)
	if second.Created || second.Rotated || second.AgentToken != "" {
		t.Fatalf("unexpected reuse response: %+v", second)
	}
	if second.TokenID != first.TokenID {
		t.Fatalf("token changed without rotate: %q -> %q", first.TokenID, second.TokenID)
	}
	route, err := db.GetRouteByHost(context.Background(), second.PublicHost)
	if err != nil {
		t.Fatalf("get route: %v", err)
	}
	if route.TargetURL != "http://127.0.0.1:7083" || route.TokenID != first.TokenID {
		t.Fatalf("route was not reused and updated: %+v", route)
	}
}

func TestRegisterDesktopDeviceIgnoresLegacyDeviceSecret(t *testing.T) {
	server, db := newDesktopTestServer(t)
	firstBody := `{"deviceId":"mac-mini","deviceSecret":"old-secret","targetUrl":"http://127.0.0.1:7082","rotateToken":false}`
	first := decodeRegisterResponse(t, performRegister(t, server, firstBody, defaultDesktopJWT).Body)

	secondBody := `{"deviceId":"mac-mini","deviceSecret":"different-old-secret","targetUrl":"http://127.0.0.1:7083","rotateToken":false}`
	rec := performRegister(t, server, secondBody, defaultDesktopJWT)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	second := decodeRegisterResponse(t, rec.Body)
	if second.TokenID != first.TokenID || second.AgentToken != "" {
		t.Fatalf("legacy deviceSecret affected registration: %+v", second)
	}
	route, err := db.GetRouteByHost(context.Background(), second.PublicHost)
	if err != nil {
		t.Fatalf("get route: %v", err)
	}
	if route.TargetURL != "http://127.0.0.1:7083" {
		t.Fatalf("legacy deviceSecret prevented update: %+v", route)
	}
}

func TestRegisterDesktopDeviceAllowsSameDeviceIDForDifferentOwners(t *testing.T) {
	server, db := newDesktopTestServer(t)
	first := decodeRegisterResponse(t, performRegister(t, server, desktopRegisterBody("mac-mini", "http://127.0.0.1:7082", false), defaultDesktopJWT).Body)
	otherOwnerJWT := signTestSSOJWT(t, defaultDesktopPrivateKey, testSSOJWTClaims{
		Issuer:   "https://official.example.test",
		Audience: "zenmind-tunnel-hub-server",
		UserID:   "43",
		Email:    "other@example.test",
		Role:     "user",
		Scope:    "profile tunnel",
		Expires:  time.Now().Add(time.Hour),
	})

	rec := performRegister(t, server, desktopRegisterBody("mac-mini", "http://127.0.0.1:7083", false), otherOwnerJWT)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	second := decodeRegisterResponse(t, rec.Body)
	if !second.Created || second.PublicHost == first.PublicHost || second.TokenID == first.TokenID {
		t.Fatalf("different owner should get an independent registration: first=%+v second=%+v", first, second)
	}
	assertDesktopPublicHost(t, second.PublicHost, "mac-mini")
	firstRoute, err := db.GetRouteByHost(context.Background(), first.PublicHost)
	if err != nil {
		t.Fatalf("get route: %v", err)
	}
	if firstRoute.TargetURL != "http://127.0.0.1:7082" || firstRoute.TokenID != first.TokenID {
		t.Fatalf("different owner changed first route: %+v", firstRoute)
	}
	secondRoute, err := db.GetRouteByHost(context.Background(), second.PublicHost)
	if err != nil {
		t.Fatalf("get second route: %v", err)
	}
	if secondRoute.TargetURL != "http://127.0.0.1:7083" || secondRoute.TokenID != second.TokenID {
		t.Fatalf("different owner route mismatch: %+v", secondRoute)
	}
}

func TestRegisterDesktopDeviceRotatesToken(t *testing.T) {
	server, db := newDesktopTestServer(t)
	first := decodeRegisterResponse(t, performRegister(t, server, desktopRegisterBody("mac-mini", "http://127.0.0.1:7082", false), defaultDesktopJWT).Body)

	rec := performRegister(t, server, desktopRegisterBody("mac-mini", "http://127.0.0.1:7082", true), defaultDesktopJWT)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	second := decodeRegisterResponse(t, rec.Body)
	if second.Created || !second.Rotated || second.AgentToken == "" {
		t.Fatalf("unexpected rotate response: %+v", second)
	}
	if second.TokenID == first.TokenID {
		t.Fatalf("token did not rotate: %q", second.TokenID)
	}
	if _, err := db.FindActiveTokenBySecret(context.Background(), first.AgentToken); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("old token should be inactive, got %v", err)
	}
	token, err := db.FindActiveTokenBySecret(context.Background(), second.AgentToken)
	if err != nil {
		t.Fatalf("new token should be active: %v", err)
	}
	if token.ID != second.TokenID {
		t.Fatalf("new token id = %q, want %q", token.ID, second.TokenID)
	}
	route, err := db.GetRouteByHost(context.Background(), second.PublicHost)
	if err != nil {
		t.Fatalf("get route: %v", err)
	}
	if route.TokenID != second.TokenID {
		t.Fatalf("route token id = %q, want %q", route.TokenID, second.TokenID)
	}
}

func TestDesktopRegistrationTunnelWebSocketIntegration(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	desktopWS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ws" {
			t.Fatalf("unexpected upstream path: %s", r.URL.Path)
		}
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade desktop ws: %v", err)
		}
		defer ws.Close()
		messageType, payload, err := ws.ReadMessage()
		if err != nil {
			t.Fatalf("read desktop ws: %v", err)
		}
		if err := ws.WriteMessage(messageType, []byte("desktop:"+string(payload))); err != nil {
			t.Fatalf("write desktop ws: %v", err)
		}
	}))
	defer desktopWS.Close()

	db := openDesktopTestDB(t)
	manager := proxy.NewManager()
	cfg := desktopTestConfig(t)
	relay := proxy.NewRelay(db, manager, nil, 64<<20)
	desktopServer, err := NewServer(db, cfg, nil)
	if err != nil {
		t.Fatalf("new desktop server: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/tunnel":
			relay.HandleTunnel(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/desktop"):
			desktopServer.ServeHTTP(w, r)
		default:
			relay.HandlePublic(w, r)
		}
	}))
	defer server.Close()

	registration := postRegisterHTTP(t, server.URL, desktopRegisterBody("mac-mini", desktopWS.URL, false))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = proxy.RunAgent(ctx, config.AgentConfig{
			RelayURL:          "ws" + strings.TrimPrefix(server.URL, "http") + "/tunnel",
			Token:             registration.AgentToken,
			ReconnectInterval: 50 * time.Millisecond,
		}, nil)
	}()
	waitForDesktopAgentToken(t, manager, registration.TokenID)

	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse server url: %v", err)
	}
	publicWSURL := "ws://" + registration.PublicHost + ":" + serverURL.Port() + "/ws"
	dialer := websocket.Dialer{NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, serverURL.Host)
	}}
	client, _, err := dialer.DialContext(ctx, publicWSURL, nil)
	if err != nil {
		t.Fatalf("dial public desktop ws: %v", err)
	}
	defer client.Close()
	if err := client.WriteMessage(websocket.TextMessage, []byte("ping")); err != nil {
		t.Fatalf("write public desktop ws: %v", err)
	}
	_, payload, err := client.ReadMessage()
	if err != nil {
		t.Fatalf("read public desktop ws: %v", err)
	}
	if string(payload) != "desktop:ping" {
		t.Fatalf("payload = %q", payload)
	}
}

func newDesktopTestServer(t *testing.T) (*Server, *store.DB) {
	t.Helper()
	return newDesktopTestServerWithConfig(t, desktopTestConfig(t))
}

func newDesktopTestServerWithConfig(t *testing.T, cfg config.RelayConfig) (*Server, *store.DB) {
	t.Helper()
	db := openDesktopTestDB(t)
	server, err := NewServer(db, cfg, nil)
	if err != nil {
		t.Fatalf("new desktop server: %v", err)
	}
	return server, db
}

func openDesktopTestDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func desktopTestConfig(t *testing.T) config.RelayConfig {
	t.Helper()
	privateKey, publicKeyPEM := testSSOJWTKey(t)
	defaultDesktopPrivateKey = privateKey
	defaultDesktopJWT = signTestSSOJWT(t, privateKey, testSSOJWTClaims{
		Issuer:   "https://official.example.test",
		Audience: "zenmind-tunnel-hub-server",
		UserID:   "42",
		Email:    "desktop@example.test",
		Role:     "user",
		Scope:    "profile tunnel",
		Expires:  time.Now().Add(time.Hour),
	})
	return config.RelayConfig{
		PublicBaseDomain:        "tunnel-hub.zenmind.cc",
		DesktopPublicBaseDomain: "m.zenmind.cc",
		SSOJWTIssuer:            "https://official.example.test",
		SSOJWTPublicKeyPEM:      publicKeyPEM,
		SSOJWTAudience:          "zenmind-tunnel-hub-server",
	}
}

func assertDesktopPublicHost(t *testing.T, publicHost, deviceID string) {
	t.Helper()
	if !strings.HasSuffix(publicHost, ".m.zenmind.cc") {
		t.Fatalf("publicHost = %q, want *.m.zenmind.cc", publicHost)
	}
	if strings.Contains(publicHost, deviceID) {
		t.Fatalf("publicHost %q should not contain device id %q", publicHost, deviceID)
	}
	label := strings.TrimSuffix(publicHost, ".m.zenmind.cc")
	if !strings.HasPrefix(label, "zm") || len(label) < 12 {
		t.Fatalf("publicHost label = %q, want generated zm label", label)
	}
}

func performRegister(t *testing.T, server *Server, body string, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, registerPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	return rec
}

type testSSOJWTClaims struct {
	Issuer   string
	Audience string
	UserID   string
	Email    string
	Role     string
	Scope    string
	Expires  time.Time
}

func testSSOJWTKey(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	publicKeyDER, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	publicKeyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "PUBLIC KEY",
		Bytes: publicKeyDER,
	})
	return privateKey, string(publicKeyPEM)
}

func signTestSSOJWT(t *testing.T, privateKey *rsa.PrivateKey, claims testSSOJWTClaims) string {
	t.Helper()
	headerJSON, _ := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "test-key"})
	claimsJSON, _ := json.Marshal(map[string]any{
		"iss":     claims.Issuer,
		"sub":     "user:" + claims.UserID,
		"aud":     claims.Audience,
		"iat":     time.Now().Unix(),
		"exp":     claims.Expires.Unix(),
		"jti":     "test-jti",
		"user_id": claims.UserID,
		"email":   claims.Email,
		"name":    "Desktop User",
		"role":    claims.Role,
		"scope":   claims.Scope,
	})
	headerPart := base64.RawURLEncoding.EncodeToString(headerJSON)
	payloadPart := base64.RawURLEncoding.EncodeToString(claimsJSON)
	signedValue := headerPart + "." + payloadPart
	digest := sha256.Sum256([]byte(signedValue))
	signature, err := rsa.SignPKCS1v15(rand.Reader, privateKey, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign JWT: %v", err)
	}
	return signedValue + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func postRegisterHTTP(t *testing.T, baseURL, body string) registerResponse {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL+registerPath, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+defaultDesktopJWT)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post register: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("register status = %d, body = %s", resp.StatusCode, string(payload))
	}
	return decodeRegisterResponse(t, resp.Body)
}

func decodeRegisterResponse(t *testing.T, body io.Reader) registerResponse {
	t.Helper()
	var response registerResponse
	if err := json.NewDecoder(body).Decode(&response); err != nil {
		t.Fatalf("decode register response: %v", err)
	}
	return response
}

func desktopRegisterBody(deviceID, targetURL string, rotateToken bool) string {
	rotate := "false"
	if rotateToken {
		rotate = "true"
	}
	return `{"deviceId":"` + deviceID + `","targetUrl":"` + targetURL + `","rotateToken":` + rotate + `}`
}

func waitForDesktopAgentToken(t *testing.T, manager *proxy.Manager, tokenID string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, agent := range manager.ActiveAgents() {
			if agent.TokenID == tokenID {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("agent token %s did not connect", tokenID)
}
