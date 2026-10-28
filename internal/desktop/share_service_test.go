package desktop

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"example.invalid/tunnel-hub-server/internal/proxy"
	"example.invalid/tunnel-hub-server/internal/sharecontract"
	"example.invalid/tunnel-hub-server/internal/store"
	"example.invalid/tunnel-hub-server/internal/tunnel"
	"github.com/hashicorp/yamux"
)

func servicePublication(t *testing.T, s *Server, deviceID, name string) store.RegisterDesktopWebAppResult {
	t.Helper()
	if response := performRegister(t, s, desktopRegisterBody(deviceID, "", false), defaultDesktopJWT); response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	publication, err := s.DB.RegisterDesktopWebApp(t.Context(), store.RegisterDesktopWebAppInput{OwnerUserID: "42", DeviceID: deviceID, Name: name, PublicHost: "abcdefghijk23-wa.example.test", TargetURL: "http://127.0.0.1:4001", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	return publication
}
func serviceShare(t *testing.T, s *Server, id string) (sharecontract.Record, []byte, string) {
	t.Helper()
	manifest := genericShareExample()
	manifest.Kind = "webapp-service"
	manifest.Title = "Frozen introduction v1"
	manifest.Desktop = nil
	manifest.Service = &sharecontract.Binding{Type: "webapp", WebAppID: id}
	body := []byte("Frozen overview version 1")
	hash := sha256.Sum256(body)
	resourceID := "000000000000000000000001"
	manifest.Resources = []sharecontract.Resource{{ID: resourceID, Name: "overview.txt", MIMEType: "text/plain", Size: int64(len(body)), SHA256: hex.EncodeToString(hash[:])}}
	return decodeGenericRecord(t, postGenericShare(t, s, manifest, map[string][]byte{resourceID: body})), body, resourceID
}
func TestShareServiceUsesOwnedExistingPublicationAndOriginalCurrentRoute(t *testing.T) {
	cfg := desktopTestConfig(t)
	cfg.SharePublicBaseURL = "https://share.example.test"
	cfg.ShareLimits.MaxSnapshotBytes = 8192
	s, db := newDesktopTestServerWithConfig(t, cfg)
	publication := servicePublication(t, s, "service-desktop", "notes")
	read := accountAPI(t, s, "GET", "/api/desktop/devices/service-desktop/webapps/notes", defaultDesktopJWT, "")
	var response webAppResponse
	if read.Code != 200 || json.Unmarshal(read.Body.Bytes(), &response) != nil || response.WebAppID != publication.WebApp.ID || response.RouteID != publication.Route.ID {
		t.Fatal(read.Code, read.Body.String())
	}
	first, frozen, resourceID := serviceShare(t, s, publication.WebApp.ID)
	second, _, _ := serviceShare(t, s, publication.WebApp.ID)
	if first.ID == second.ID {
		t.Fatal("shared creation reused an ID")
	}
	if head := performConversationShareRequest(s, "HEAD", "/share/"+first.ID+"?action=service", nil, ""); head.Code != http.StatusSeeOther {
		t.Fatal("HEAD service selection failed", head.Code)
	}
	listed, err := db.ListShares(t.Context(), "42")
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range listed {
		if item.LastAccessedAt != nil {
			t.Fatal("HEAD recorded public access")
		}
	}
	page := performConversationShareRequest(s, "GET", "/share/"+first.ID, nil, "")
	if page.Code != 200 || !strings.Contains(page.Body.String(), "?action=service") {
		t.Fatal(page.Code, page.Body.String())
	}
	resolve := performConversationShareRequest(s, "GET", "/share/"+first.ID+"?action=resolve", nil, "")
	if !strings.Contains(resolve.Body.String(), `"service"`) {
		t.Fatal(resolve.Body.String())
	}
	if strings.Contains(resolve.Body.String(), "ownerUserId") || strings.Contains(resolve.Body.String(), "targetUrl") || strings.Contains(resolve.Body.String(), publication.WebApp.ID) {
		t.Fatal("public resolution exposed publication internals")
	}
	service := performConversationShareRequest(s, "GET", "/share/"+first.ID+"?action=service", nil, "")
	if service.Code != 303 || service.Header().Get("Location") != "https://"+publication.WebApp.PublicHost+"/" || service.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatal(service.Code, service.Header())
	}
	manager := proxy.NewManager()
	relay := proxy.NewRelay(db, manager, s.Logger, cfg.BrandID, s.desktopPublicBaseDomain(), s.webAppPublicBaseDomain(), 0)
	publicRequest := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", service.Header().Get("Location"), nil)
		out := httptest.NewRecorder()
		relay.HandlePublic(out, req)
		return out
	}
	if out := publicRequest(); out.Code != http.StatusBadGateway || !strings.Contains(out.Body.String(), "offline") {
		t.Fatal("original offline semantics changed", out.Code, out.Body.String())
	}
	updated, err := db.RegisterDesktopWebApp(t.Context(), store.RegisterDesktopWebAppInput{OwnerUserID: "42", DeviceID: "service-desktop", Name: "notes", PublicHost: publication.WebApp.PublicHost, TargetURL: "http://127.0.0.1:4002", Active: true})
	if err != nil || updated.WebApp.ID != publication.WebApp.ID || updated.Route.ID != publication.Route.ID {
		t.Fatal(err, "binding changed")
	}
	a, b := net.Pipe()
	client, err := yamux.Client(a)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := yamux.Server(b)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	defer peer.Close()
	manager.SetActive(&proxy.ActiveTunnel{SessionID: "synthetic-current", Key: proxy.DesktopConnectionKey(publication.Device.DeviceKey), OwnerUserID: "42", DesktopID: "service-desktop", Yamux: client, ConnectedAt: time.Now()})
	result := make(chan error, 1)
	go func() {
		stream, err := peer.AcceptStream()
		if err != nil {
			result <- err
			return
		}
		defer stream.Close()
		var request tunnel.StreamRequest
		if err = tunnel.ReadJSON(stream, &request); err != nil {
			result <- err
			return
		}
		if request.Payload.Upstream.Port != 4002 || request.Payload.AuthToken != "" || request.Payload.Public.Headers.Get("Authorization") != "" {
			result <- io.ErrUnexpectedEOF
			return
		}
		body := []byte("current deployment v2")
		err = tunnel.WriteJSON(stream, tunnel.NewSuccessResponse(tunnel.NamespaceWebApp, tunnel.TypeWebAppHTTPRequest, request.ID, &tunnel.StreamResponseData{StatusCode: 200, BodyLength: tunnel.Int64Ptr(int64(len(body)))}))
		if err == nil {
			_, err = stream.Write(body)
		}
		result <- err
	}()
	if out := publicRequest(); out.Code != 200 || out.Body.String() != "current deployment v2" {
		t.Fatal(out.Code, out.Body.String())
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	download := func(id string) *httptest.ResponseRecorder {
		return performConversationShareRequest(s, "GET", "/share/"+id+"?action=download&resourceId="+resourceID, nil, "")
	}
	if out := download(first.ID); out.Code != 200 || !bytes.Equal(out.Body.Bytes(), frozen) {
		t.Fatal("frozen resource changed")
	}
	if out := performConversationShareRequest(s, "DELETE", sharecontract.ManagementPath+"/"+first.ID, nil, defaultDesktopJWT); out.Code != 204 {
		t.Fatal(out.Code)
	}
	for _, action := range []string{"preview", "service", "resolve", "download&resourceId=" + resourceID} {
		if out := performConversationShareRequest(s, "GET", "/share/"+first.ID+"?action="+action, nil, ""); out.Code != 404 {
			t.Fatal(action, out.Code)
		}
	}
	if out := performConversationShareRequest(s, "GET", "/share/"+second.ID+"?action=service", nil, ""); out.Code != 303 {
		t.Fatal("revoking one share changed another", out.Code)
	}
	if _, err := db.GetActiveDesktopWebAppRouteByID(t.Context(), publication.WebApp.ID); err != nil {
		t.Fatal("share revoke stopped publication", err)
	}
}

func TestShareServiceRejectsForeignUnpublishedAndInjectedTargets(t *testing.T) {
	cfg := desktopTestConfig(t)
	cfg.SharePublicBaseURL = "https://share.example.test"
	cfg.ShareLimits.MaxSnapshotBytes = 8192
	s, db := newDesktopTestServerWithConfig(t, cfg)
	publication := servicePublication(t, s, "service-desktop", "notes")
	for _, test := range []struct {
		token  string
		status int
	}{{"", 401}, {signTestSSOJWT(t, defaultDesktopPrivateKey, testSSOJWTClaims{Issuer: cfg.SSOJWTIssuer, Audience: cfg.SSOJWTAudience, UserID: "foreign", Scope: "tunnel", Expires: time.Now().Add(time.Hour)}), 404}} {
		if out := accountAPI(t, s, "GET", "/api/desktop/devices/service-desktop/webapps/notes", test.token, ""); out.Code != test.status {
			t.Fatal("foreign lookup accepted", out.Code)
		}
	}
	if _, err := db.RegisterDesktopDevice(t.Context(), store.RegisterDesktopDeviceInput{OwnerUserID: "foreign", DeviceID: "other-device", PublicHost: "other.m.example.test"}); err != nil {
		t.Fatal(err)
	}
	foreign, err := db.RegisterDesktopWebApp(t.Context(), store.RegisterDesktopWebAppInput{OwnerUserID: "foreign", DeviceID: "other-device", Name: "notes", PublicHost: "bbbbbbbbbbb23-wa.example.test", TargetURL: "http://127.0.0.1:4444", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	record, _, _ := serviceShare(t, s, publication.WebApp.ID)
	manifest := genericShareExample()
	manifest.Service = &sharecontract.Binding{Type: "webapp", WebAppID: foreign.WebApp.ID}
	if out := postGenericShare(t, s, manifest, nil); out.Code != 422 {
		t.Fatal("foreign publication accepted", out.Code)
	}
	manifest.Service = &sharecontract.Binding{Type: "webapp", WebAppID: "not-published"}
	if out := postGenericShare(t, s, manifest, nil); out.Code != 422 {
		t.Fatal(out.Code)
	}
	for _, query := range []string{"action=service&url=https://attacker.invalid", "action=service&webAppId=other", "action=service&deviceId=other"} {
		if out := performConversationShareRequest(s, "GET", "/share/"+record.ID+"?"+query, nil, ""); out.Code != 400 {
			t.Fatal(query, out.Code)
		}
	}
	_, err = db.RegisterDesktopWebApp(t.Context(), store.RegisterDesktopWebAppInput{OwnerUserID: "42", DeviceID: "service-desktop", Name: "notes", PublicHost: publication.WebApp.PublicHost, TargetURL: publication.WebApp.TargetURL, Active: false})
	if err != nil {
		t.Fatal(err)
	}
	if out := performConversationShareRequest(s, "GET", "/share/"+record.ID+"?action=service", nil, ""); out.Code != 404 || !strings.Contains(out.Body.String(), "?action=preview") {
		t.Fatal("unpublished result unclear", out.Code, out.Body.String())
	}
	if out := performConversationShareRequest(s, "GET", "/share/"+record.ID, nil, ""); out.Code != 200 {
		t.Fatal("unpublish removed frozen share", out.Code)
	}
	if out := accountAPI(t, s, "GET", "/api/desktop/devices/service-desktop/webapps/notes", defaultDesktopJWT, ""); out.Code != 404 {
		t.Fatal("read activated publication", out.Code)
	}
}
