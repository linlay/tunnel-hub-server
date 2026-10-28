package desktop

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"example.invalid/tunnel-hub-server/internal/sharecontract"
)

func genericShareExample() sharecontract.Envelope {
	return sharecontract.Envelope{ProtocolVersion: 1, Kind: "market-resource", Title: "Frozen connector",
		Content: sharecontract.Content{Version: 3, Data: json.RawMessage(`{"description":"frozen","resourceVersion":"1.2.3"}`)}, Resources: []sharecontract.Resource{},
		Desktop: &sharecontract.Handoff{ReceiverID: "builtin.market", ContextVersion: 2, Context: json.RawMessage(`{"itemId":"connector-1","resourceVersion":"1.2.3"}`)}}
}

func postGenericShare(t *testing.T, server *Server, manifest any, bodies map[string][]byte) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var envelope sharecontract.Envelope
	_ = json.Unmarshal(data, &envelope)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="manifest"; filename="manifest.json"`)
	header.Set("Content-Type", "application/json")
	part, err := writer.CreatePart(header)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write(data)
	for _, resource := range envelope.Resources {
		content, ok := bodies[resource.ID]
		if !ok {
			continue
		}
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="resource:%s"; filename=%q`, resource.ID, resource.Name))
		header.Set("Content-Type", resource.MIMEType)
		part, err := writer.CreatePart(header)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = part.Write(content)
	}
	_ = writer.Close()
	request := httptest.NewRequest(http.MethodPost, sharecontract.ManagementPath, &body)
	request.Header.Set("Authorization", "Bearer "+defaultDesktopJWT)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	return response
}

func decodeGenericRecord(t *testing.T, response *httptest.ResponseRecorder) sharecontract.Record {
	t.Helper()
	if response.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", response.Code, response.Body.String())
	}
	var record sharecontract.Record
	if err := json.Unmarshal(response.Body.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	return record
}

func TestShareV2AndLegacyUseOneLifecycleAndSeparateProjections(t *testing.T) {
	cfg := desktopTestConfig(t)
	cfg.SharePublicBaseURL = "https://share.example.test"
	cfg.ShareLimits.MaxSnapshotBytes = 8192
	server, db := newDesktopTestServerWithConfig(t, cfg)
	market := genericShareExample()
	first := decodeGenericRecord(t, postGenericShare(t, server, market, nil))
	second := decodeGenericRecord(t, postGenericShare(t, server, market, nil))
	if first.ID == second.ID || first.Kind != "market-resource" || first.Title != market.Title {
		t.Fatal("publication identity was reused")
	}
	stored, err := db.AccessPublicConversationShare(httptest.NewRequest(http.MethodGet, "/", nil).Context(), first.ID)
	if err != nil || stored.ConversationID != "" || stored.SnapshotVersion != 3 || !json.Valid(stored.SnapshotJSON) {
		t.Fatal("generic content was converted into a conversation", err)
	}
	if bytes.Contains(stored.DescriptionJSON, []byte(`"description":"frozen"`)) {
		t.Fatal("content duplicated into description BLOB")
	}
	for _, id := range []string{first.ID, second.ID} {
		response := performConversationShareRequest(server, http.MethodGet, sharecontract.PublicPath+id+"?action=resolve", nil, "")
		var resolved sharecontract.Resolution
		if err := json.Unmarshal(response.Body.Bytes(), &resolved); err != nil || response.Code != http.StatusOK {
			t.Fatal("resolve", err, response.Code)
		}
		if resolved.Desktop == nil || resolved.Desktop.ContextVersion != 2 || resolved.Desktop.ReceiverID != "builtin.market" || !bytes.Contains(resolved.Desktop.Context, []byte(`"resourceVersion":"1.2.3"`)) {
			t.Fatal("business versions were changed")
		}
		if !reflect.DeepEqual(resolved.Actions, []sharecontract.Action{sharecontract.Preview, sharecontract.Desktop}) {
			t.Fatal("incorrect action declaration", resolved.Actions)
		}
		if bytes.Contains(response.Body.Bytes(), []byte(`owner`)) || bytes.Contains(response.Body.Bytes(), []byte(`"content"`)) {
			t.Fatal("resolve leaked management identity/content")
		}
	}
	old := performConversationShareRequest(server, http.MethodPost, conversationSharesPath, []byte(validConversationSnapshot), defaultDesktopJWT)
	if old.Code != http.StatusCreated {
		t.Fatal(old.Code, old.Body.String())
	}
	var oldFields map[string]json.RawMessage
	_ = json.Unmarshal(old.Body.Bytes(), &oldFields)
	keys := make([]string, 0, len(oldFields))
	for key := range oldFields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if !reflect.DeepEqual(keys, []string{"conversationId", "createdAt", "expiresAt", "id", "lastAccessedAt", "singleUse", "url"}) || string(oldFields["expiresAt"]) != "null" || string(oldFields["singleUse"]) != "false" {
		t.Fatal("legacy strict wire contract changed", keys)
	}
	listed := performConversationShareRequest(server, http.MethodGet, conversationSharesPath, nil, defaultDesktopJWT)
	var legacy conversationShareListResponse
	_ = json.Unmarshal(listed.Body.Bytes(), &legacy)
	if len(legacy.Items) != 1 || legacy.Items[0].ConversationID != "chat-test" {
		t.Fatal("legacy list included generic records")
	}
	listed = performConversationShareRequest(server, http.MethodGet, sharecontract.ManagementPath, nil, defaultDesktopJWT)
	var all sharecontract.List
	_ = json.Unmarshal(listed.Body.Bytes(), &all)
	if listed.Code != http.StatusOK || len(all.Items) != 3 {
		t.Fatal("generic metadata list", listed.Code, listed.Body.String())
	}
	for _, query := range []string{"action=desktop&receiverId=evil", "action=desktop&contextVersion=1", "action=desktop&resourceVersion=latest", "action=resolve&action=preview", "action=install"} {
		if response := performConversationShareRequest(server, http.MethodGet, sharecontract.PublicPath+first.ID+"?"+query, nil, ""); response.Code != http.StatusBadRequest {
			t.Fatal("query override accepted", query, response.Code)
		}
	}
	if response := performConversationShareRequest(server, http.MethodGet, sharecontract.PublicPath+first.ID+"?action=service", nil, ""); response.Code != http.StatusUnprocessableEntity {
		t.Fatal("undeclared service action accepted")
	}
	if response := performConversationShareRequest(server, http.MethodDelete, sharecontract.ManagementPath+"/"+first.ID, nil, defaultDesktopJWT); response.Code != http.StatusNoContent {
		t.Fatal("revoke failed", response.Code)
	}
	for _, action := range []string{"preview", "resolve", "desktop", "download&resourceId=0123456789abcdef01234567", "service"} {
		if response := performConversationShareRequest(server, http.MethodGet, sharecontract.PublicPath+first.ID+"?action="+action, nil, ""); response.Code != http.StatusNotFound {
			t.Fatal("revoked behavior readable", action, response.Code)
		}
	}
	if response := performConversationShareRequest(server, http.MethodGet, sharecontract.PublicPath+second.ID, nil, ""); response.Code != http.StatusOK {
		t.Fatal("independent share affected")
	}
}

func TestShareV2ResourcesFreezeIntegrityAndBelongToOneShare(t *testing.T) {
	cfg := desktopTestConfig(t)
	cfg.SharePublicBaseURL = "https://share.example.test"
	cfg.ShareLimits.MaxSnapshotBytes = 8192
	server, _ := newDesktopTestServerWithConfig(t, cfg)
	manifest := genericShareExample()
	manifest.Kind = "static-report"
	manifest.Desktop = nil
	const id = "0123456789abcdef01234567"
	body := []byte("frozen bytes")
	digest := sha256.Sum256(body)
	manifest.Resources = []sharecontract.Resource{{ID: id, Name: "report.txt", MIMEType: "text/plain", Size: int64(len(body)), SHA256: hex.EncodeToString(digest[:])}}
	manifest.Content = sharecontract.Content{Version: 3, ResourceID: id}
	first := decodeGenericRecord(t, postGenericShare(t, server, manifest, map[string][]byte{id: body}))
	second := decodeGenericRecord(t, postGenericShare(t, server, manifest, map[string][]byte{id: body}))
	path := sharecontract.PublicPath + first.ID + "?action=download&resourceId=" + id
	response := performConversationShareRequest(server, http.MethodGet, path, nil, "")
	if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), body) || !strings.Contains(response.Header().Get("Content-Disposition"), "attachment") {
		t.Fatal("frozen download failed", response.Code)
	}
	if response := performConversationShareRequest(server, http.MethodGet, sharecontract.PublicPath+first.ID+"?action=download&resourceId=ffffffffffffffffffffffff", nil, ""); response.Code != http.StatusNotFound {
		t.Fatal("undeclared resource readable")
	}
	file := filepath.Join(cfg.ConversationShareResourceDir, "shares", first.ID, "resources", id)
	if err := os.WriteFile(file, bytes.Repeat([]byte("x"), len(body)), 0600); err != nil {
		t.Fatal(err)
	}
	if response := performConversationShareRequest(server, http.MethodGet, path, nil, ""); response.Code != http.StatusInternalServerError {
		t.Fatal("corrupt resource hash was not checked", response.Code)
	}
	if response := performConversationShareRequest(server, http.MethodGet, sharecontract.PublicPath+second.ID+"?action=download&resourceId="+id, nil, ""); response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), body) {
		t.Fatal("private bytes were shared across IDs")
	}
	if response := performConversationShareRequest(server, http.MethodDelete, sharecontract.ManagementPath+"/"+first.ID, nil, defaultDesktopJWT); response.Code != http.StatusNoContent {
		t.Fatal(response.Code)
	}
	if response := performConversationShareRequest(server, http.MethodGet, path, nil, ""); response.Code != http.StatusNotFound {
		t.Fatal("revoked resource still readable")
	}
}

func TestShareV2RejectsBadManifestQuotaAndUnvalidatedService(t *testing.T) {
	cfg := desktopTestConfig(t)
	cfg.SharePublicBaseURL = "https://share.example.test"
	cfg.ShareLimits.MaxSnapshotBytes = 8192
	server, _ := newDesktopTestServerWithConfig(t, cfg)
	for name, mutate := range map[string]func(map[string]any){
		"owner":          func(m map[string]any) { m["userId"] = "other" },
		"retired policy": func(m map[string]any) { m["singleUse"] = false },
		"outer version":  func(m map[string]any) { m["protocolVersion"] = 2 },
		"path escape": func(m map[string]any) {
			m["resources"] = []any{map[string]any{"id": "0123456789abcdef01234567", "name": "file", "mimeType": "text/plain", "size": 0, "sha256": strings.Repeat("a", 64), "path": "../file"}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			data, _ := json.Marshal(genericShareExample())
			var manifest map[string]any
			_ = json.Unmarshal(data, &manifest)
			mutate(manifest)
			if response := postGenericShare(t, server, manifest, nil); response.Code != http.StatusBadRequest {
				t.Fatal(response.Code, response.Body.String())
			}
		})
	}
	manifest := genericShareExample()
	manifest.Service = &sharecontract.Binding{Type: "webapp", WebAppID: "unverified"}
	if response := postGenericShare(t, server, manifest, nil); response.Code != http.StatusUnprocessableEntity {
		t.Fatal("unvalidated binding accepted", response.Code)
	}
	manifest = genericShareExample()
	manifest.Content.Data = json.RawMessage(`{"text":"` + strings.Repeat("x", 8192) + `"}`)
	response := postGenericShare(t, server, manifest, nil)
	var tooLarge conversationShareSizeError
	if response.Code != http.StatusRequestEntityTooLarge || json.Unmarshal(response.Body.Bytes(), &tooLarge) != nil || tooLarge.Code != "snapshot_too_large" {
		t.Fatal("manifest quota weakened", response.Code, response.Body.String())
	}
	limits := performConversationShareRequest(server, http.MethodGet, sharecontract.ManagementPath+"/limits", nil, defaultDesktopJWT)
	var got sharecontract.Limits
	if limits.Code != http.StatusOK || json.Unmarshal(limits.Body.Bytes(), &got) != nil || got.MaxManifestBytes != 8192 || got.MaxResourceBytes != cfg.ShareLimits.MaxResourceBytes {
		t.Fatal("v2 limits route matched wrong prefix")
	}
}

func TestShareV2DatabaseFailureCompensatesCommittedFiles(t *testing.T) {
	cfg := desktopTestConfig(t)
	cfg.SharePublicBaseURL = "https://share.example.test"
	cfg.ShareLimits.MaxSnapshotBytes = 8192
	server, db := newDesktopTestServerWithConfig(t, cfg)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if response := postGenericShare(t, server, genericShareExample(), nil); response.Code != http.StatusInternalServerError {
		t.Fatal(response.Code)
	}
	entries, err := os.ReadDir(filepath.Join(cfg.ConversationShareResourceDir, "shares"))
	if err != nil || len(entries) != 0 {
		t.Fatal("failed share resources not compensated", err)
	}
	entries, err = os.ReadDir(filepath.Join(cfg.ConversationShareResourceDir, ".staging"))
	if err != nil || len(entries) != 0 {
		t.Fatal("staging not aborted", err)
	}
}

func TestShareV2ManagementKeepsLimitsAndLegacyHeadersAtTheirOwnEdges(t *testing.T) {
	cfg := desktopTestConfig(t)
	cfg.SharePublicBaseURL = "https://share.example.test"
	server, _ := newDesktopTestServerWithConfig(t, cfg)
	for _, path := range []string{sharecontract.ManagementPath, sharecontract.ManagementPath + "/limits"} {
		if response := performConversationShareRequest(server, http.MethodGet, path, nil, ""); response.Code != http.StatusUnauthorized {
			t.Fatal("management read was anonymous", path)
		}
	}
	for _, path := range []string{sharecontract.ManagementPath + "/limits", conversationSharesPath + "/limits"} {
		if response := performConversationShareRequest(server, http.MethodDelete, path, nil, defaultDesktopJWT); response.Code != http.StatusMethodNotAllowed {
			t.Fatal("limits was routed as an item", path, response.Code)
		}
	}
	request := httptest.NewRequest(http.MethodPost, sharecontract.ManagementPath, strings.NewReader("invalid body"))
	request.Header.Set("Authorization", "Bearer "+defaultDesktopJWT)
	request.Header.Set(conversationShareExpirationHeader, "once")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatal("v2 silently ignored old non-permanent request", response.Code)
	}
}
