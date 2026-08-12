package desktop

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linlay/zenmind-tunnel-server/internal/config"
)

const validConversationShareSnapshot = `{"schemaVersion":1,"title":"Release plan","createdAt":1700000000000,"updatedAt":1700000001000,"entries":[{"type":"message","role":"user","content":"hello","createdAt":1700000000000},{"type":"reasoning","content":"compare options","label":"分析问题","durationMs":900,"createdAt":1700000000500},{"type":"message","role":"assistant","content":"hi","createdAt":1700000001000}]}`

func TestConversationShareAPIRequiresAuthAndSupportsRevoke(t *testing.T) {
	cfg := desktopTestConfig(t)
	cfg.SharePublicBaseURL = "https://share.example.test/"
	server, _ := newDesktopTestServerWithConfig(t, cfg)

	unauthorized := performConversationShareRequest(server, http.MethodPost, conversationSharesPath, validConversationShareSnapshot, "")
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d body=%s", unauthorized.Code, unauthorized.Body.String())
	}

	created := performConversationShareRequest(server, http.MethodPost, conversationSharesPath, validConversationShareSnapshot, defaultDesktopJWT)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	var result conversationShareCreateResponse
	if err := json.Unmarshal(created.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if !strings.HasPrefix(result.ID, "share_") || result.URL != "https://share.example.test/share/"+result.ID {
		t.Fatalf("unexpected create response: %#v", result)
	}

	public := performConversationShareRequest(server, http.MethodGet, publicConversationSharesPath+result.ID, "", "")
	if public.Code != http.StatusOK || !strings.Contains(public.Body.String(), `"title":"Release plan"`) {
		t.Fatalf("public status=%d body=%s", public.Code, public.Body.String())
	}
	if public.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("cache-control=%q", public.Header().Get("Cache-Control"))
	}

	revoked := performConversationShareRequest(server, http.MethodDelete, conversationSharesPath+"/"+result.ID, "", defaultDesktopJWT)
	if revoked.Code != http.StatusNoContent {
		t.Fatalf("revoke status=%d body=%s", revoked.Code, revoked.Body.String())
	}
	missing := performConversationShareRequest(server, http.MethodGet, publicConversationSharesPath+result.ID, "", "")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("revoked public status=%d body=%s", missing.Code, missing.Body.String())
	}
}

func TestConversationShareAPIRejectsUnsafeSnapshot(t *testing.T) {
	server, _ := newDesktopTestServer(t)
	for _, body := range []string{
		`{"schemaVersion":2,"title":"Bad","createdAt":1700000000000,"updatedAt":1700000001000,"entries":[{"type":"message","role":"user","content":"hello"}]}`,
		`{"schemaVersion":1,"title":"Bad","createdAt":1700000000000,"updatedAt":1700000001000,"entries":[{"type":"message","role":"system","content":"secret"}]}`,
		`{"schemaVersion":1,"title":"Bad","createdAt":1700000000000,"updatedAt":1700000001000,"entries":[{"type":"message","role":"user","content":"hello","agentKey":"secret"}]}`,
		`{"schemaVersion":1,"title":"Bad","createdAt":1700000000000,"updatedAt":1700000001000,"entries":[{"type":"reasoning","role":"assistant","content":"secret"}]}`,
		`{"schemaVersion":1,"title":"Bad","createdAt":1700000000000,"updatedAt":1700000001000,"entries":[{"type":"message","role":"user","content":"hello","label":"not allowed"}]}`,
		`{"schemaVersion":1,"title":"Bad","createdAt":1700000000000,"updatedAt":1700000001000,"entries":[{"type":"message","role":"user","content":"hello","durationMs":10}]}`,
		`{"schemaVersion":1,"title":"Bad","createdAt":1700000000000,"updatedAt":1700000001000,"entries":[{"type":"reasoning","content":"secret","durationMs":-1}]}`,
	} {
		rec := performConversationShareRequest(server, http.MethodPost, conversationSharesPath, body, defaultDesktopJWT)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("unsafe snapshot status=%d body=%s", rec.Code, rec.Body.String())
		}
	}
}

func TestConversationShareURLRequiresExplicitPublicEnvironment(t *testing.T) {
	server := &Server{Config: config.RelayConfig{}}
	if _, err := server.conversationShareURL("share_test"); err == nil {
		t.Fatal("expected missing public share URL to fail")
	}
	server.Config.SharePublicBaseURL = "https://share.example.test"
	if got, err := server.conversationShareURL("share_test"); err != nil || got != "https://share.example.test/share/share_test" {
		t.Fatalf("url=%q err=%v", got, err)
	}
}

func performConversationShareRequest(server *Server, method, path, body, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	return rec
}
