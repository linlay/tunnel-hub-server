package desktop

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"example.invalid/tunnel-hub-server/internal/devicelink"
	"example.invalid/tunnel-hub-server/internal/proxy"
	"example.invalid/tunnel-hub-server/internal/store"
)

type fakeDesktopPresenceManager struct {
	active map[proxy.ConnectionKey]proxy.ActiveTunnelMetric
}

func (m fakeDesktopPresenceManager) ActiveFor(key proxy.ConnectionKey) (proxy.ActiveTunnelMetric, bool) {
	active, ok := m.active[key]
	return active, ok
}

func deviceLinkRequest(t *testing.T, server *Server, method, path, token string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	var body *bytes.Reader
	if payload == nil {
		body = bytes.NewReader(nil)
	} else {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		body = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, body)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, req)
	return recorder
}

func accountDeviceJWT(t *testing.T, accountID, deviceID, deviceKind, trustLevel string) string {
	t.Helper()
	return signTestSSOJWT(t, defaultDesktopPrivateKey, testSSOJWTClaims{
		Issuer: "https://official.example.test", Audience: "tunnel-hub-server", UserID: "alice",
		AccountID: accountID, DeviceID: deviceID, DeviceKind: deviceKind, TrustLevel: trustLevel,
		Scope: "app tunnel", Expires: time.Now().Add(time.Hour),
	})
}

func allowAccountDeviceValidation(server *Server) {
	server.SetAccountDeviceValidator(devicelink.AccountDeviceValidatorFunc(func(_ context.Context, _, accountID, sourceDeviceID, deviceID string) (devicelink.DeviceValidation, error) {
		if deviceID == "" {
			deviceID = sourceDeviceID
		}
		return devicelink.DeviceValidation{
			AccountID: accountID, SourceDeviceID: sourceDeviceID, SourceTrustLevel: "trusted",
			ValidatedDeviceID: deviceID, ValidatedTrustLevel: "trusted", StateVersion: 1,
		}, nil
	}))
}

func TestAccountDesktopRegistrationRequiresTokenDeviceBinding(t *testing.T) {
	server, db := newDesktopTestServer(t)
	allowAccountDeviceValidation(server)
	desktopID := "22222222-2222-4222-8222-222222222222"
	otherID := "33333333-3333-4333-8333-333333333333"
	desktopToken := accountDeviceJWT(t, "account-alice", desktopID, "desktop", "trusted")

	mismatch := performRegister(t, server, desktopRegisterBody(otherID, "", false), desktopToken)
	if mismatch.Code != http.StatusForbidden {
		t.Fatalf("mismatched registration status = %d, want 403 body=%s", mismatch.Code, mismatch.Body.String())
	}
	mobileToken := accountDeviceJWT(t, "account-alice", desktopID, "mobile", "trusted")
	mobile := performRegister(t, server, desktopRegisterBody(desktopID, "", false), mobileToken)
	if mobile.Code != http.StatusForbidden {
		t.Fatalf("mobile registration status = %d, want 403 body=%s", mobile.Code, mobile.Body.String())
	}

	registered := performRegister(t, server, desktopRegisterBody(desktopID, "", false), desktopToken)
	if registered.Code != http.StatusOK {
		t.Fatalf("registration status = %d body=%s", registered.Code, registered.Body.String())
	}
	response := decodeRegisterResponse(t, registered.Body)
	device, err := db.GetDesktopDeviceByPublicHost(context.Background(), response.PublicHost)
	if err != nil {
		t.Fatalf("load registered desktop: %v", err)
	}
	if device.OwnerUserID != "account-alice" || device.DeviceID != desktopID {
		t.Fatalf("unexpected canonical registration: %+v", device)
	}
}

func TestAccountPresenceAndRouteCredentialUseServerTrustedIdentity(t *testing.T) {
	server, db := newDesktopTestServer(t)
	allowAccountDeviceValidation(server)
	targetID := "22222222-2222-4222-8222-222222222222"
	sourceID := "11111111-1111-4111-8111-111111111111"
	registered, err := db.RegisterDesktopDevice(context.Background(), store.RegisterDesktopDeviceInput{
		DeviceID: targetID, DeviceName: "Desktop", OwnerUserID: "account-alice", PublicHost: "desk.m.example.test",
	})
	if err != nil {
		t.Fatalf("register target: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate route key: %v", err)
	}
	signer, err := devicelink.NewRouteCredentialSigner(devicelink.RouteCredentialIssuer, devicelink.RouteCredentialAudience, "route-test", privateKey)
	if err != nil {
		t.Fatalf("new route signer: %v", err)
	}
	server.now = func() time.Time { return now }
	server.SetDeviceLink(fakeDesktopPresenceManager{active: map[proxy.ConnectionKey]proxy.ActiveTunnelMetric{
		proxy.DesktopConnectionKey(registered.Device.DeviceKey): {
			Kind: proxy.ConnectionKindDesktop, ConnectionID: registered.Device.DeviceKey, ConnectedAt: now.Add(-time.Minute),
		},
	}}, signer)
	token := accountDeviceJWT(t, "account-alice", sourceID, "mobile", "trusted")
	validationRR := deviceLinkRequest(t, server, http.MethodPost, accountDeviceValidationPath, token, map[string]string{
		"deviceId": targetID,
	})
	if validationRR.Code != http.StatusOK {
		t.Fatalf("device validation status = %d body=%s", validationRR.Code, validationRR.Body.String())
	}
	var validation struct {
		SchemaVersion int                         `json:"schemaVersion"`
		Validation    devicelink.DeviceValidation `json:"validation"`
	}
	if err := json.NewDecoder(validationRR.Body).Decode(&validation); err != nil {
		t.Fatalf("decode validation response: %v", err)
	}
	if validation.SchemaVersion != 1 || validation.Validation.AccountID != "account-alice" || validation.Validation.SourceDeviceID != sourceID || validation.Validation.ValidatedDeviceID != targetID {
		t.Fatalf("unexpected validation response: %+v", validation)
	}

	presenceRR := deviceLinkRequest(t, server, http.MethodGet, accountPresencePath, token, nil)
	if presenceRR.Code != http.StatusOK {
		t.Fatalf("presence status = %d body=%s", presenceRR.Code, presenceRR.Body.String())
	}
	var presence struct {
		SchemaVersion int `json:"schemaVersion"`
		Items         []struct {
			DeviceID string `json:"deviceId"`
			Online   bool   `json:"online"`
		} `json:"items"`
	}
	if err := json.NewDecoder(presenceRR.Body).Decode(&presence); err != nil {
		t.Fatalf("decode presence: %v", err)
	}
	if presence.SchemaVersion != 1 || len(presence.Items) != 1 || presence.Items[0].DeviceID != targetID || !presence.Items[0].Online {
		t.Fatalf("unexpected presence: %+v", presence)
	}

	requestID := "55555555-5555-4555-8555-555555555555"
	routeRR := deviceLinkRequest(t, server, http.MethodPost, routeCredentialsPath, token, map[string]string{
		"targetDesktopId": targetID, "requestId": requestID,
	})
	if routeRR.Code != http.StatusCreated {
		t.Fatalf("route status = %d body=%s", routeRR.Code, routeRR.Body.String())
	}
	var route struct {
		Credential   string `json:"credential"`
		WebSocketURL string `json:"webSocketUrl"`
		TokenMode    string `json:"tokenMode"`
	}
	if err := json.NewDecoder(routeRR.Body).Decode(&route); err != nil {
		t.Fatalf("decode route response: %v", err)
	}
	if route.WebSocketURL != "wss://desk.m.example.test/ws" || route.TokenMode != "query" {
		t.Fatalf("route endpoint = %#v", route)
	}
	verified, err := devicelink.VerifyRouteCredential(route.Credential, map[string]*rsa.PublicKey{"route-test": &privateKey.PublicKey}, devicelink.RouteValidation{
		Issuer: devicelink.RouteCredentialIssuer, Audience: devicelink.RouteCredentialAudience,
		AccountID: "account-alice", TargetDesktopID: targetID, Now: now,
	})
	if err != nil {
		t.Fatalf("verify route credential: %v", err)
	}
	if verified.SourceDeviceID != sourceID || verified.RequestID != requestID {
		t.Fatalf("route identity was not derived from token: %+v", verified)
	}
	events, err := db.ListEvents(context.Background(), 10)
	if err != nil {
		t.Fatalf("list route audit events: %v", err)
	}
	if len(events) == 0 || events[0].Type != "device_link.route_credential_issued" {
		t.Fatalf("missing route audit event: %+v", events)
	}
	for _, expected := range []string{"account-alice", sourceID, targetID, requestID, verified.JTI} {
		if !bytes.Contains([]byte(events[0].Details), []byte(expected)) {
			t.Fatalf("route audit details missing %q: %s", expected, events[0].Details)
		}
	}
	if bytes.Contains([]byte(events[0].Details), []byte(route.Credential)) {
		t.Fatal("route credential leaked into audit details")
	}

	bobToken := accountDeviceJWT(t, "account-bob", sourceID, "mobile", "trusted")
	crossAccount := deviceLinkRequest(t, server, http.MethodPost, routeCredentialsPath, bobToken, map[string]string{
		"targetDesktopId": targetID, "requestId": requestID,
	})
	if crossAccount.Code != http.StatusForbidden {
		t.Fatalf("cross-account route status = %d, want 403 body=%s", crossAccount.Code, crossAccount.Body.String())
	}
}
