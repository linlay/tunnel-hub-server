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
	"errors"
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

func TestAdminLocalLoginCookieAuth(t *testing.T) {
	server, db := newAdminTestServer(t)
	if _, _, err := db.EnsureAdminUser(context.Background(), "admin", "secret"); err != nil {
		t.Fatalf("ensure admin user: %v", err)
	}

	loginReq := httptest.NewRequest(http.MethodPost, "/api/admin/login", strings.NewReader(`{"username":"admin","password":"secret"}`))
	loginReq.Header.Set("Content-Type", "application/json")
	loginRec := httptest.NewRecorder()
	server.ServeHTTP(loginRec, loginReq)
	if loginRec.Code != http.StatusOK {
		t.Fatalf("login status = %d body = %s", loginRec.Code, loginRec.Body.String())
	}
	cookies := loginRec.Result().Cookies()
	if len(cookies) == 0 || cookies[0].Name != adminSessionCookieName || !cookies[0].HttpOnly {
		t.Fatalf("session cookie not set correctly: %#v", cookies)
	}
	var loginResponse map[string]any
	if err := json.NewDecoder(loginRec.Body).Decode(&loginResponse); err != nil {
		t.Fatalf("decode login response: %v", err)
	}
	if loginResponse["username"] != "admin" || loginResponse["source"] != "local" || loginResponse["role"] != "admin" {
		t.Fatalf("unexpected login response: %#v", loginResponse)
	}

	meReq := httptest.NewRequest(http.MethodGet, "/api/admin/me", nil)
	meReq.AddCookie(cookies[0])
	meRec := httptest.NewRecorder()
	server.ServeHTTP(meRec, meReq)
	if meRec.Code != http.StatusOK {
		t.Fatalf("me status = %d body = %s", meRec.Code, meRec.Body.String())
	}

	routesReq := httptest.NewRequest(http.MethodGet, "/api/admin/routes", nil)
	routesReq.AddCookie(cookies[0])
	routesRec := httptest.NewRecorder()
	server.ServeHTTP(routesRec, routesReq)
	if routesRec.Code != http.StatusOK {
		t.Fatalf("routes status = %d body = %s", routesRec.Code, routesRec.Body.String())
	}

	logoutReq := httptest.NewRequest(http.MethodPost, "/api/admin/logout", nil)
	logoutReq.AddCookie(cookies[0])
	logoutRec := httptest.NewRecorder()
	server.ServeHTTP(logoutRec, logoutReq)
	if logoutRec.Code != http.StatusOK {
		t.Fatalf("logout status = %d body = %s", logoutRec.Code, logoutRec.Body.String())
	}

	routesReq = httptest.NewRequest(http.MethodGet, "/api/admin/routes", nil)
	routesReq.AddCookie(cookies[0])
	routesRec = httptest.NewRecorder()
	server.ServeHTTP(routesRec, routesReq)
	if routesRec.Code != http.StatusUnauthorized {
		t.Fatalf("routes after logout status = %d body = %s", routesRec.Code, routesRec.Body.String())
	}
}

func TestAdminUsersManagement(t *testing.T) {
	server, db := newAdminTestServer(t)
	root, _, err := db.EnsureAdminUser(context.Background(), "admin", "secret")
	if err != nil {
		t.Fatalf("ensure root admin: %v", err)
	}

	createReq := authedAdminRequest(http.MethodPost, "/api/admin/users", `{"username":"operator","password":"secret-2","status":"active"}`)
	createRec := httptest.NewRecorder()
	server.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create status = %d body = %s", createRec.Code, createRec.Body.String())
	}
	var created store.AdminUser
	if err := json.NewDecoder(createRec.Body).Decode(&created); err != nil {
		t.Fatalf("decode created admin: %v", err)
	}
	if created.Username != "operator" || created.Status != "active" {
		t.Fatalf("unexpected created admin: %+v", created)
	}

	listReq := authedAdminRequest(http.MethodGet, "/api/admin/users", "")
	listRec := httptest.NewRecorder()
	server.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list status = %d body = %s", listRec.Code, listRec.Body.String())
	}
	var listResponse struct {
		Items []store.AdminUser `json:"items"`
	}
	if err := json.NewDecoder(listRec.Body).Decode(&listResponse); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listResponse.Items) != 2 {
		t.Fatalf("admin user count = %d, want 2", len(listResponse.Items))
	}

	patchReq := authedAdminRequest(http.MethodPatch, "/api/admin/users/"+created.ID, `{"username":"ops","password":"new-secret","status":"disabled"}`)
	patchRec := httptest.NewRecorder()
	server.ServeHTTP(patchRec, patchReq)
	if patchRec.Code != http.StatusOK {
		t.Fatalf("patch status = %d body = %s", patchRec.Code, patchRec.Body.String())
	}
	var patched store.AdminUser
	if err := json.NewDecoder(patchRec.Body).Decode(&patched); err != nil {
		t.Fatalf("decode patched admin: %v", err)
	}
	if patched.Username != "ops" || patched.Status != "disabled" {
		t.Fatalf("unexpected patched admin: %+v", patched)
	}
	if _, err := db.VerifyAdminLogin(context.Background(), "ops", "new-secret"); !errors.Is(err, store.ErrUserInactive) {
		t.Fatalf("disabled admin should not log in, got %v", err)
	}

	deleteRootReq := authedAdminRequest(http.MethodDelete, "/api/admin/users/"+root.ID, "")
	deleteRootRec := httptest.NewRecorder()
	server.ServeHTTP(deleteRootRec, deleteRootReq)
	if deleteRootRec.Code != http.StatusBadRequest {
		t.Fatalf("last active delete status = %d body = %s", deleteRootRec.Code, deleteRootRec.Body.String())
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

func TestManualTokenCreationDisabledAndDesktopRegistrationStillReturnsAgentToken(t *testing.T) {
	server, db := newAdminTestServer(t)
	req := authedAdminRequest(http.MethodPost, "/api/admin/tokens", `{"name":"manual"}`)
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("manual token status = %d, body = %s", rec.Code, rec.Body.String())
	}
	registration, err := db.RegisterDesktopDevice(context.Background(), store.RegisterDesktopDeviceInput{
		DeviceID:    "mac-mini",
		DeviceName:  "Mac Mini",
		OwnerUserID: "42",
		OwnerEmail:  "desktop@example.test",
		PublicHost:  "desk.m.zenmind.cc",
	})
	if err != nil {
		t.Fatalf("register desktop device: %v", err)
	}
	if registration.AgentToken == "" || registration.Token.ID == "" || !registration.Created {
		t.Fatalf("registration should still issue agent token: %+v", registration)
	}
}

func TestConsoleAggregationEndpoints(t *testing.T) {
	server, db := newAdminTestServer(t)
	ctx := context.Background()
	registration, err := db.RegisterDesktopDevice(ctx, store.RegisterDesktopDeviceInput{
		DeviceID:    "mac-mini",
		DeviceName:  "Mac Mini",
		OwnerUserID: "42",
		OwnerEmail:  "desktop@example.test",
		OwnerName:   "Lin",
		PublicHost:  "desk.m.zenmind.cc",
	})
	if err != nil {
		t.Fatalf("register desktop: %v", err)
	}
	webapp, err := db.RegisterDesktopWebApp(ctx, store.RegisterDesktopWebAppInput{
		OwnerUserID: "42",
		DeviceID:    "mac-mini",
		Name:        "notes",
		PublicHost:  "notes.wa.zenmind.cc",
		TargetURL:   "http://127.0.0.1:5173",
		Active:      true,
	})
	if err != nil {
		t.Fatalf("register webapp: %v", err)
	}
	sessionRecord, err := db.CreateAgentSession(ctx, registration.Token.ID, "127.0.0.1:50000")
	if err != nil {
		t.Fatalf("create agent session: %v", err)
	}
	activeYamux, peer := newAdminTestSession(t)
	defer peer.Close()
	server.Manager.SetActive(&proxy.ActiveAgent{
		SessionID:   sessionRecord.ID,
		TokenID:     registration.Token.ID,
		RemoteAddr:  sessionRecord.RemoteAddr,
		ConnectedAt: sessionRecord.ConnectedAt,
		Yamux:       activeYamux,
	})
	if err := db.RecordTrafficEvent(ctx, store.TrafficEvent{
		ObjectType: "webapp",
		PublicHost: webapp.Route.PublicHost,
		RouteID:    webapp.Route.ID,
		TokenID:    registration.Token.ID,
		SessionID:  sessionRecord.ID,
		Kind:       "http",
		Method:     http.MethodGet,
		Path:       "/hello",
		StatusCode: http.StatusOK,
		BytesIn:    111,
		BytesOut:   222,
		OccurredAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("record traffic event: %v", err)
	}
	if err := db.AddEvent(ctx, "admin_user.created", "Admin user created", "ops"); err != nil {
		t.Fatalf("add event: %v", err)
	}

	overviewReq := authedAdminRequest(http.MethodGet, "/api/admin/overview?range=hour", "")
	overviewRec := httptest.NewRecorder()
	server.ServeHTTP(overviewRec, overviewReq)
	if overviewRec.Code != http.StatusOK {
		t.Fatalf("overview status = %d, body = %s", overviewRec.Code, overviewRec.Body.String())
	}
	var overview overviewResponse
	if err := json.NewDecoder(overviewRec.Body).Decode(&overview); err != nil {
		t.Fatalf("decode overview: %v", err)
	}
	if overview.Range != "hour" || overview.DesktopConnectionCount != 1 || overview.WebAppCount != 1 || overview.TotalTrafficBytes != 333 {
		t.Fatalf("unexpected overview: %+v", overview)
	}
	if overview.Resources.TotalDesktops != 1 || overview.Resources.OnlineDesktops != 1 || overview.Resources.ActiveWebApps != 1 || len(overview.Traffic) == 0 {
		t.Fatalf("unexpected overview resources/traffic: %+v", overview)
	}
	if overview.RecentIdentity != "Lin" || overview.RecentDevice != "Mac Mini" {
		t.Fatalf("unexpected recent identity/device: %+v", overview)
	}

	desktopsReq := authedAdminRequest(http.MethodGet, "/api/admin/desktops", "")
	desktopsRec := httptest.NewRecorder()
	server.ServeHTTP(desktopsRec, desktopsReq)
	if desktopsRec.Code != http.StatusOK {
		t.Fatalf("desktops status = %d, body = %s", desktopsRec.Code, desktopsRec.Body.String())
	}
	var desktops []desktopAdminResponse
	if err := json.NewDecoder(desktopsRec.Body).Decode(&desktops); err != nil {
		t.Fatalf("decode desktops: %v", err)
	}
	if len(desktops) != 1 || desktops[0].PublicHost != "desk.m.zenmind.cc" || !desktops[0].Online || desktops[0].SessionID != sessionRecord.ID || desktops[0].Traffic.BytesOut != 222 {
		t.Fatalf("unexpected desktops: %+v", desktops)
	}

	webappsReq := authedAdminRequest(http.MethodGet, "/api/admin/webapps", "")
	webappsRec := httptest.NewRecorder()
	server.ServeHTTP(webappsRec, webappsReq)
	if webappsRec.Code != http.StatusOK {
		t.Fatalf("webapps status = %d, body = %s", webappsRec.Code, webappsRec.Body.String())
	}
	var webapps []webAppAdminResponse
	if err := json.NewDecoder(webappsRec.Body).Decode(&webapps); err != nil {
		t.Fatalf("decode webapps: %v", err)
	}
	if len(webapps) != 1 || webapps[0].PublicHost != "notes.wa.zenmind.cc" || !webapps[0].Online || webapps[0].Traffic.BytesIn != 111 || webapps[0].Route.ID != webapp.Route.ID {
		t.Fatalf("unexpected webapps: %+v", webapps)
	}

	activityReq := authedAdminRequest(http.MethodGet, "/api/admin/activity?objectType=webapp&q=notes", "")
	activityRec := httptest.NewRecorder()
	server.ServeHTTP(activityRec, activityReq)
	if activityRec.Code != http.StatusOK {
		t.Fatalf("activity status = %d, body = %s", activityRec.Code, activityRec.Body.String())
	}
	var activity activityResponse
	if err := json.NewDecoder(activityRec.Body).Decode(&activity); err != nil {
		t.Fatalf("decode activity: %v", err)
	}
	if len(activity.Items) != 1 || activity.Items[0].ObjectType != "webapp" || activity.Items[0].BytesOut != 222 {
		t.Fatalf("unexpected activity: %+v", activity)
	}
}

func TestConsoleAggregationEndpointsEmptyData(t *testing.T) {
	server, _ := newAdminTestServer(t)
	req := authedAdminRequest(http.MethodGet, "/api/admin/overview?range=month", "")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var overview overviewResponse
	if err := json.NewDecoder(rec.Body).Decode(&overview); err != nil {
		t.Fatalf("decode overview: %v", err)
	}
	if overview.Range != "month" || overview.DesktopConnectionCount != 0 || overview.WebAppCount != 0 || overview.TotalTrafficBytes != 0 || len(overview.Traffic) == 0 {
		t.Fatalf("unexpected empty overview: %+v", overview)
	}
}

func TestSessionCloseEndpoint(t *testing.T) {
	server, db := newAdminTestServer(t)
	ctx := context.Background()
	token := createAdminTestToken(t, db, "mac-mini")

	missingReq := authedAdminRequest(http.MethodPost, "/api/admin/sessions/session_missing/close", "")
	missingRec := httptest.NewRecorder()
	server.ServeHTTP(missingRec, missingReq)
	if missingRec.Code != http.StatusNotFound {
		t.Fatalf("missing status = %d, body = %s", missingRec.Code, missingRec.Body.String())
	}

	disconnected, err := db.CreateAgentSession(ctx, token.ID, "127.0.0.1:50000")
	if err != nil {
		t.Fatalf("create disconnected session: %v", err)
	}
	if err := db.EndAgentSession(ctx, disconnected.ID); err != nil {
		t.Fatalf("end disconnected session: %v", err)
	}
	disconnectedReq := authedAdminRequest(http.MethodPost, "/api/admin/sessions/"+disconnected.ID+"/close", "")
	disconnectedRec := httptest.NewRecorder()
	server.ServeHTTP(disconnectedRec, disconnectedReq)
	if disconnectedRec.Code != http.StatusConflict {
		t.Fatalf("disconnected status = %d, body = %s", disconnectedRec.Code, disconnectedRec.Body.String())
	}

	inactive, err := db.CreateAgentSession(ctx, token.ID, "127.0.0.1:50001")
	if err != nil {
		t.Fatalf("create inactive session: %v", err)
	}
	inactiveReq := authedAdminRequest(http.MethodPost, "/api/admin/sessions/"+inactive.ID+"/close", "")
	inactiveRec := httptest.NewRecorder()
	server.ServeHTTP(inactiveRec, inactiveReq)
	if inactiveRec.Code != http.StatusConflict {
		t.Fatalf("inactive status = %d, body = %s", inactiveRec.Code, inactiveRec.Body.String())
	}

	activeRecord, err := db.CreateAgentSession(ctx, token.ID, "127.0.0.1:50002")
	if err != nil {
		t.Fatalf("create active session: %v", err)
	}
	activeYamux, peer := newAdminTestSession(t)
	defer peer.Close()
	server.Manager.SetActive(&proxy.ActiveAgent{
		SessionID:   activeRecord.ID,
		TokenID:     token.ID,
		RemoteAddr:  activeRecord.RemoteAddr,
		ConnectedAt: activeRecord.ConnectedAt,
		Yamux:       activeYamux,
	})
	activeReq := authedAdminRequest(http.MethodPost, "/api/admin/sessions/"+activeRecord.ID+"/close", "")
	activeRec := httptest.NewRecorder()
	server.ServeHTTP(activeRec, activeReq)
	if activeRec.Code != http.StatusOK {
		t.Fatalf("active status = %d, body = %s", activeRec.Code, activeRec.Body.String())
	}
	if !activeYamux.IsClosed() {
		t.Fatal("active yamux session was not closed")
	}
	events, err := db.ListEvents(ctx, 10)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) == 0 || events[0].Type != "agent.closed" || events[0].Details != activeRecord.ID {
		t.Fatalf("missing agent.closed event: %+v", events)
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
