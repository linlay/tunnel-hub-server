package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"example.invalid/tunnel-hub-server/internal/sharecontract"
	"example.invalid/tunnel-hub-server/internal/store"
)

func TestTemplateUsageRecordsLegacyAndV2PagesOnly(t *testing.T) {
	cfg := desktopTestConfig(t)
	cfg.SharePublicBaseURL = "https://share.example.test"
	server, _ := newDesktopTestServerWithConfig(t, cfg)
	var recorded []string
	server.SetConversationShareRenderObserver(func(_ context.Context, version, id string) {
		if version != "fixture-version" {
			t.Error("wrong version", version)
		}
		recorded = append(recorded, id)
	})
	created := performConversationShareRequestWithExpiration(server, "permanent", []byte(validConversationSnapshot))
	var legacy conversationShareRecordResponse
	if err := json.Unmarshal(created.Body.Bytes(), &legacy); err != nil || created.Code != 201 {
		t.Fatalf("legacy=%s", created.Body.String())
	}
	manifest := sharecontract.Envelope{ProtocolVersion: 1, Kind: "conversation", Title: "Conversation", Content: sharecontract.Content{Version: 1, Data: json.RawMessage(validConversationSnapshot)}, Resources: []sharecontract.Resource{}, Preview: &sharecontract.PreviewSpec{Type: "builtin", Viewer: "conversation"}}
	v2 := decodeGenericRecord(t, postGenericShare(t, server, manifest, nil))
	for _, id := range []string{legacy.ID, v2.ID} {
		head := performConversationShareRequest(server, http.MethodHead, "/share/"+id, nil, "")
		if head.Code != 200 {
			t.Fatal("HEAD", head.Code)
		}
	}
	if len(recorded) != 0 {
		t.Fatal("HEAD counted as rendering")
	}
	for _, id := range []string{legacy.ID, v2.ID} {
		page := performConversationShareRequest(server, http.MethodGet, "/share/"+id, nil, "")
		if page.Code != 200 {
			t.Fatal("GET", page.Code, page.Body.String())
		}
	}
	if len(recorded) != 2 || recorded[0] != legacy.ID || recorded[1] != v2.ID {
		t.Fatal("render usage", recorded)
	}
	metadata := decodeGenericRecord(t, postGenericShare(t, server, genericShareExample(), nil))
	performConversationShareRequest(server, http.MethodGet, "/share/"+metadata.ID, nil, "")
	performConversationShareRequest(server, http.MethodGet, "/share/"+v2.ID+"?action=resolve", nil, "")
	performConversationShareRequest(server, http.MethodDelete, sharecontract.ManagementPath+"/"+v2.ID, nil, defaultDesktopJWT)
	performConversationShareRequest(server, http.MethodGet, "/share/"+v2.ID, nil, "")
	server.SetConversationShareRenderer(&failedUsageRenderer{})
	failed := performConversationShareRequest(server, http.MethodGet, "/share/"+legacy.ID, nil, "")
	if failed.Code != 500 || len(recorded) != 2 {
		t.Fatal("failed/metadata/revoked response counted", recorded)
	}
}

type failedUsageRenderer struct {
	capturingConversationShareRenderer
}

func (*failedUsageRenderer) RenderVersion([]byte, string, string, string, string, bool) ([]byte, string, error) {
	return nil, "", errors.New("render failed")
}

type switchingUsageRenderer struct {
	testConversationShareRenderer
	current string
}

func (r *switchingUsageRenderer) RenderVersion(_ []byte, _, _, _, _ string, _ bool) ([]byte, string, error) {
	fixed := r.current
	r.current = "B"
	return []byte("rendered " + fixed), fixed, nil
}
func TestUsageUsesRenderedVersionAfterCurrentChanges(t *testing.T) {
	renderer := &switchingUsageRenderer{current: "A"}
	server := &Server{conversationShareRenderer: renderer}
	var observed string
	server.SetConversationShareRenderObserver(func(_ context.Context, version, id string) { observed = version })
	w := httptest.NewRecorder()
	server.writeConversationSharePage(w, httptest.NewRequest("GET", "/share/example", nil), store.ConversationShare{ID: "example"})
	if w.Code != 200 || w.Body.String() != "rendered A" || observed != "A" || renderer.current != "B" {
		t.Fatal(w.Code, w.Body.String(), observed)
	}
}
