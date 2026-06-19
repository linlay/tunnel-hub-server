package admin

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
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/linlay/zenmind-tunnel-server/internal/auth"
	"github.com/linlay/zenmind-tunnel-server/internal/config"
	"github.com/linlay/zenmind-tunnel-server/internal/proxy"
	"github.com/linlay/zenmind-tunnel-server/internal/store"
)

func TestAdminAPIKeyBearerAuth(t *testing.T) {
	server, db := newAdminTestServer(t)
	rawKey, err := auth.NewAdminAPIKey()
	if err != nil {
		t.Fatalf("new api key: %v", err)
	}
	key, err := db.CreateAdminAPIKey(context.Background(), "automation", rawKey)
	if err != nil {
		t.Fatalf("create api key: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/admin/routes", nil)
	req.Header.Set("Authorization", "Bearer "+rawKey)
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	keys, err := db.ListAdminAPIKeys(context.Background())
	if err != nil {
		t.Fatalf("list api keys: %v", err)
	}
	if len(keys) != 1 || keys[0].ID != key.ID || keys[0].LastUsedAt == nil {
		t.Fatalf("api key was not touched after bearer auth: %+v", keys)
	}
}

func TestAdminSSOJWTBearerAuth(t *testing.T) {
	privateKey, publicKeyPEM := testSSOJWTKey(t)
	server, _ := newAdminTestServerWithConfig(t, config.RelayConfig{
		CookieSecret:       "test-cookie-secret",
		PublicBaseDomain:   "tunnel-hub.zenmind.cc",
		SSOJWTIssuer:       "https://official.example.test",
		SSOJWTPublicKeyPEM: publicKeyPEM,
		SSOJWTAudience:     "zenmind-tunnel-hub-server",
	})

	adminToken := signTestSSOJWT(t, privateKey, testSSOJWTClaims{
		Issuer:   "https://official.example.test",
		Audience: "zenmind-tunnel-hub-server",
		UserID:   "1",
		Email:    "admin@example.test",
		Role:     "admin",
		Expires:  time.Now().Add(time.Hour),
	})
	req := httptest.NewRequest(http.MethodGet, "/api/admin/routes", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin JWT status = %d body = %s", rec.Code, rec.Body.String())
	}

	wrongAudienceToken := signTestSSOJWT(t, privateKey, testSSOJWTClaims{
		Issuer:   "https://official.example.test",
		Audience: "zenmind-market-server",
		UserID:   "1",
		Email:    "admin@example.test",
		Role:     "admin",
		Expires:  time.Now().Add(time.Hour),
	})
	req = httptest.NewRequest(http.MethodGet, "/api/admin/routes", nil)
	req.Header.Set("Authorization", "Bearer "+wrongAudienceToken)
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong audience status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func TestCreateAdminAPIKeyEndpoint(t *testing.T) {
	server, _ := newAdminTestServer(t)
	req := authedAdminRequest(http.MethodPost, "/api/admin/api-keys", `{"name":"deploy-bot"}`)
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var response struct {
		Secret string            `json:"secret"`
		APIKey store.AdminAPIKey `json:"apiKey"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !strings.HasPrefix(response.Secret, "za_") {
		t.Fatalf("secret prefix = %q", response.Secret)
	}
	if response.APIKey.Name != "deploy-bot" || response.APIKey.KeyPrefix == "" {
		t.Fatalf("unexpected api key response: %+v", response.APIKey)
	}
	if response.APIKey.KeyHash != "" {
		t.Fatal("api key hash should not be exposed in json")
	}
}

func TestServicePublishUpsertsManagedRoute(t *testing.T) {
	server, db := newAdminTestServer(t)
	token := createAdminTestToken(t, db, "mac-mini")
	req := authedAdminRequest(http.MethodPut, "/api/admin/services/auditor", fmt.Sprintf(`{"targetUrl":"http://127.0.0.1:3000","active":true,"tokenId":%q}`, token.ID))
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	created := decodeServiceResponse(t, rec)
	if created.PublicHost != "auditor.tunnel-hub.zenmind.cc" {
		t.Fatalf("publicHost = %q", created.PublicHost)
	}
	if created.PublicURL != "https://auditor.tunnel-hub.zenmind.cc" {
		t.Fatalf("publicUrl = %q", created.PublicURL)
	}
	if created.Route.TargetURL != "http://127.0.0.1:3000" || !created.Route.Active {
		t.Fatalf("unexpected created route: %+v", created.Route)
	}
	if created.Route.TokenID != token.ID {
		t.Fatalf("route token id = %q", created.Route.TokenID)
	}

	req = authedAdminRequest(http.MethodPut, "/api/admin/services/auditor", fmt.Sprintf(`{"targetUrl":"http://127.0.0.1:4000","active":false,"tokenId":%q}`, token.ID))
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	updated := decodeServiceResponse(t, rec)
	if updated.Route.ID != created.Route.ID {
		t.Fatalf("expected route upsert, got new id %s", updated.Route.ID)
	}
	if updated.Route.TargetURL != "http://127.0.0.1:4000" || updated.Route.Active {
		t.Fatalf("unexpected updated route: %+v", updated.Route)
	}
	route, err := db.GetRouteByHost(context.Background(), "auditor.tunnel-hub.zenmind.cc")
	if err != nil {
		t.Fatalf("get route by host: %v", err)
	}
	if route.ID != created.Route.ID {
		t.Fatalf("stored route id = %s", route.ID)
	}
}

func TestServiceGetAndDeleteManagedRoute(t *testing.T) {
	server, db := newAdminTestServer(t)
	token := createAdminTestToken(t, db, "mac-mini")
	req := authedAdminRequest(http.MethodPut, "/api/admin/services/auditor", fmt.Sprintf(`{"targetUrl":"http://127.0.0.1:3000","tokenId":%q}`, token.ID))
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("put status = %d, body = %s", rec.Code, rec.Body.String())
	}

	req = authedAdminRequest(http.MethodGet, "/api/admin/services/auditor", "")
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, body = %s", rec.Code, rec.Body.String())
	}

	req = authedAdminRequest(http.MethodDelete, "/api/admin/services/auditor", "")
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, body = %s", rec.Code, rec.Body.String())
	}

	req = authedAdminRequest(http.MethodGet, "/api/admin/services/auditor", "")
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get deleted status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestServicePublishValidation(t *testing.T) {
	server, db := newAdminTestServer(t)
	token := createAdminTestToken(t, db, "mac-mini")
	cases := []struct {
		name string
		path string
		body string
	}{
		{name: "uppercase", path: "/api/admin/services/Auditor", body: fmt.Sprintf(`{"targetUrl":"http://127.0.0.1:3000","tokenId":%q}`, token.ID)},
		{name: "dot", path: "/api/admin/services/auditor.dev", body: fmt.Sprintf(`{"targetUrl":"http://127.0.0.1:3000","tokenId":%q}`, token.ID)},
		{name: "reserved", path: "/api/admin/services/admin", body: fmt.Sprintf(`{"targetUrl":"http://127.0.0.1:3000","tokenId":%q}`, token.ID)},
		{name: "bad target", path: "/api/admin/services/auditor", body: fmt.Sprintf(`{"targetUrl":"ftp://127.0.0.1:3000","tokenId":%q}`, token.ID)},
		{name: "missing host", path: "/api/admin/services/auditor", body: fmt.Sprintf(`{"targetUrl":"http:///missing","tokenId":%q}`, token.ID)},
		{name: "missing token", path: "/api/admin/services/auditor", body: `{"targetUrl":"http://127.0.0.1:3000"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := authedAdminRequest(http.MethodPut, tc.path, tc.body)
			rec := httptest.NewRecorder()
			server.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestCreateRouteRequiresActiveToken(t *testing.T) {
	server, db := newAdminTestServer(t)
	token := createAdminTestToken(t, db, "mac-mini")
	req := authedAdminRequest(http.MethodPost, "/api/admin/routes", fmt.Sprintf(`{"publicHost":"app.example.com","targetUrl":"http://127.0.0.1:3000","active":true,"tokenId":%q}`, token.ID))
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var route store.Route
	if err := json.NewDecoder(rec.Body).Decode(&route); err != nil {
		t.Fatalf("decode route: %v", err)
	}
	if route.TokenID != token.ID {
		t.Fatalf("route token id = %q", route.TokenID)
	}

	req = authedAdminRequest(http.MethodPost, "/api/admin/routes", `{"publicHost":"bad.example.com","targetUrl":"http://127.0.0.1:3000","active":true}`)
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing token status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestAgentsEndpointCombinesTokenConnectionAndRoutes(t *testing.T) {
	server, db := newAdminTestServer(t)
	token := createAdminTestToken(t, db, "mac-mini")
	if _, err := db.CreateRoute(context.Background(), "app.example.com", "http://127.0.0.1:3000", true, token.ID); err != nil {
		t.Fatalf("create route: %v", err)
	}
	session, peer := newAdminTestSession(t)
	connectedAt := time.Now().UTC()
	server.Manager.SetActive(&proxy.ActiveAgent{
		SessionID:   "session_1",
		TokenID:     token.ID,
		RemoteAddr:  "127.0.0.1:50000",
		ConnectedAt: connectedAt,
		Yamux:       session,
	})
	defer peer.Close()

	req := authedAdminRequest(http.MethodGet, "/api/admin/agents", "")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var agents []agentResponse
	if err := json.NewDecoder(rec.Body).Decode(&agents); err != nil {
		t.Fatalf("decode agents: %v", err)
	}
	if len(agents) != 1 || !agents[0].Online || agents[0].RouteCount != 1 || agents[0].Token.ID != token.ID {
		t.Fatalf("unexpected agents response: %+v", agents)
	}
}

func newAdminTestServer(t *testing.T) (*Server, *store.DB) {
	t.Helper()
	return newAdminTestServerWithConfig(t, config.RelayConfig{
		CookieSecret:     "test-cookie-secret",
		PublicBaseDomain: "tunnel-hub.zenmind.cc",
	})
}

func newAdminTestServerWithConfig(t *testing.T, cfg config.RelayConfig) (*Server, *store.DB) {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return NewServer(db, proxy.NewManager(), cfg, nil), db
}

func authedAdminRequest(method, path, body string) *http.Request {
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{
		Name:  sessionCookie,
		Value: auth.SignSession("test-cookie-secret", "admin", time.Hour),
	})
	return req
}

func decodeServiceResponse(t *testing.T, rec *httptest.ResponseRecorder) servicePublishResponse {
	t.Helper()
	var response servicePublishResponse
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatalf("decode service response: %v", err)
	}
	return response
}

func createAdminTestToken(t *testing.T, db *store.DB, name string) store.TunnelToken {
	t.Helper()
	raw, err := auth.NewToken()
	if err != nil {
		t.Fatalf("new token: %v", err)
	}
	token, err := db.CreateToken(context.Background(), name, raw)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	return token
}

type testSSOJWTClaims struct {
	Issuer   string
	Audience string
	UserID   string
	Email    string
	Role     string
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
		"role":    claims.Role,
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

func newAdminTestSession(t *testing.T) (*yamux.Session, *yamux.Session) {
	t.Helper()
	left, right := net.Pipe()
	server, err := yamux.Server(left, yamux.DefaultConfig())
	if err != nil {
		t.Fatalf("start yamux server: %v", err)
	}
	client, err := yamux.Client(right, yamux.DefaultConfig())
	if err != nil {
		t.Fatalf("start yamux client: %v", err)
	}
	t.Cleanup(func() {
		_ = server.Close()
		_ = client.Close()
	})
	return server, client
}
