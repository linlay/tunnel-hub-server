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

var defaultAdminJWT string

func TestLegacyAdminAPIKeyBearerAuthRejected(t *testing.T) {
	server, _ := newAdminTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/routes", nil)
	req.Header.Set("Authorization", "Bearer za_legacy-admin-api-key")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestAdminSSOJWTBearerAuth(t *testing.T) {
	privateKey, publicKeyPEM := testSSOJWTKey(t)
	server, _ := newAdminTestServerWithConfig(t, config.RelayConfig{
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
		Scope:    "profile tunnel",
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
		Scope:    "profile tunnel",
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

func TestAdminJWTRejectsMissingOrInvalidClaims(t *testing.T) {
	privateKey, publicKeyPEM := testSSOJWTKey(t)
	server, _ := newAdminTestServerWithConfig(t, config.RelayConfig{
		PublicBaseDomain:   "tunnel-hub.zenmind.cc",
		SSOJWTIssuer:       "https://official.example.test",
		SSOJWTPublicKeyPEM: publicKeyPEM,
		SSOJWTAudience:     "zenmind-tunnel-hub-server",
	})
	validClaims := testSSOJWTClaims{
		Issuer:   "https://official.example.test",
		Audience: "zenmind-tunnel-hub-server",
		UserID:   "1",
		Email:    "admin@example.test",
		Role:     "admin",
		Scope:    "profile tunnel",
		Expires:  time.Now().Add(time.Hour),
	}
	cases := []struct {
		name       string
		token      string
		wantStatus int
	}{
		{name: "missing", wantStatus: http.StatusUnauthorized},
		{name: "user role", token: signTestSSOJWT(t, privateKey, withClaims(validClaims, func(claims *testSSOJWTClaims) { claims.Role = "user" })), wantStatus: http.StatusForbidden},
		{name: "missing tunnel scope", token: signTestSSOJWT(t, privateKey, withClaims(validClaims, func(claims *testSSOJWTClaims) { claims.Scope = "profile market" })), wantStatus: http.StatusForbidden},
		{name: "wrong issuer", token: signTestSSOJWT(t, privateKey, withClaims(validClaims, func(claims *testSSOJWTClaims) { claims.Issuer = "https://other.example.test" })), wantStatus: http.StatusUnauthorized},
		{name: "expired", token: signTestSSOJWT(t, privateKey, withClaims(validClaims, func(claims *testSSOJWTClaims) { claims.Expires = time.Now().Add(-time.Minute) })), wantStatus: http.StatusUnauthorized},
		{name: "non RS256", token: signTestSSOJWTWithAlg(t, privateKey, validClaims, "HS256"), wantStatus: http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/admin/routes", nil)
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			rec := httptest.NewRecorder()
			server.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d, body = %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}
}

func TestAdminAPIKeyEndpointRemoved(t *testing.T) {
	server, _ := newAdminTestServer(t)
	req := authedAdminRequest(http.MethodPost, "/api/admin/api-keys", `{"name":"deploy-bot"}`)
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
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

func TestComponentsEndpointIsPublicAndRedactsSensitiveFields(t *testing.T) {
	server, db := newAdminTestServer(t)
	token := createAdminTestToken(t, db, "mac-mini")
	if _, err := db.CreateRoute(context.Background(), "app.example.com", "http://127.0.0.1:3000", true, token.ID); err != nil {
		t.Fatalf("create route: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/components", nil)
	rec := httptest.NewRecorder()

	server.ServeComponents(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, forbidden := range []string{"targetUrl", "tokenId", "secret", "route_"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("component response leaked %q: %s", forbidden, body)
		}
	}
	var components []componentResponse
	if err := json.Unmarshal([]byte(body), &components); err != nil {
		t.Fatalf("decode components: %v", err)
	}
	if len(components) != 1 || components[0].PublicHost != "app.example.com" || components[0].PublicURL != "https://app.example.com" {
		t.Fatalf("unexpected components: %+v", components)
	}
}

func newAdminTestServer(t *testing.T) (*Server, *store.DB) {
	t.Helper()
	privateKey, publicKeyPEM := testSSOJWTKey(t)
	defaultAdminJWT = signTestSSOJWT(t, privateKey, testSSOJWTClaims{
		Issuer:   "https://official.example.test",
		Audience: "zenmind-tunnel-hub-server",
		UserID:   "1",
		Email:    "admin@example.test",
		Role:     "admin",
		Scope:    "profile tunnel",
		Expires:  time.Now().Add(time.Hour),
	})
	return newAdminTestServerWithConfig(t, config.RelayConfig{
		PublicBaseDomain:   "tunnel-hub.zenmind.cc",
		SSOJWTIssuer:       "https://official.example.test",
		SSOJWTPublicKeyPEM: publicKeyPEM,
		SSOJWTAudience:     "zenmind-tunnel-hub-server",
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
	server, err := NewServer(db, proxy.NewManager(), cfg, nil)
	if err != nil {
		t.Fatalf("new admin server: %v", err)
	}
	return server, db
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
	req.Header.Set("Authorization", "Bearer "+defaultAdminJWT)
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
	return signTestSSOJWTWithAlg(t, privateKey, claims, "RS256")
}

func signTestSSOJWTWithAlg(t *testing.T, privateKey *rsa.PrivateKey, claims testSSOJWTClaims, alg string) string {
	t.Helper()
	headerJSON, _ := json.Marshal(map[string]any{"alg": alg, "typ": "JWT", "kid": "test-key"})
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

func withClaims(claims testSSOJWTClaims, mutate func(*testSSOJWTClaims)) testSSOJWTClaims {
	mutate(&claims)
	return claims
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
