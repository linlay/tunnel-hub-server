package desktop

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/linlay/zenmind-tunnel-server/internal/config"
)

const validConversationShareEventStream = `event: message
data: {"seq":1,"type":"chat.start","shareVersion":1,"chatName":"Release plan","timestamp":1700000000000}

event: message
data: {"seq":2,"type":"request.query","message":"hello","timestamp":1700000000000}

event: message
data: {"seq":3,"type":"run.start","timestamp":1700000000100}

event: message
data: {"seq":4,"type":"reasoning.snapshot","text":"compare options","reasoningLabel":"分析问题","timestamp":1700000000500}

event: message
data: {"seq":5,"type":"content.snapshot","text":"hi","timestamp":1700000000900}

event: message
data: {"seq":6,"type":"run.complete","timestamp":1700000001000}

event: message
data: [DONE]

`

func TestConversationShareAPIRequiresAuthAndPreservesEventStreamBytes(t *testing.T) {
	cfg := desktopTestConfig(t)
	cfg.SharePublicBaseURL = "https://share.example.test/"
	server, _ := newDesktopTestServerWithConfig(t, cfg)

	unauthorized := performConversationShareRequest(server, http.MethodPost, conversationSharesPath, validConversationShareEventStream, "")
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d body=%s", unauthorized.Code, unauthorized.Body.String())
	}

	created := performConversationShareRequest(server, http.MethodPost, conversationSharesPath, validConversationShareEventStream, defaultDesktopJWT)
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
	if public.Code != http.StatusOK || public.Body.String() != validConversationShareEventStream {
		t.Fatalf("public event stream mismatch status=%d body=%q", public.Code, public.Body.String())
	}
	if public.Header().Get("Cache-Control") != "no-store" || public.Header().Get("Content-Type") != "text/event-stream; charset=utf-8" {
		t.Fatalf("unexpected public headers: %#v", public.Header())
	}
	if public.Header().Get("Content-Length") != strconv.Itoa(len(validConversationShareEventStream)) {
		t.Fatalf("content length=%q", public.Header().Get("Content-Length"))
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

func TestConversationShareAPIRejectsInvalidEventStreams(t *testing.T) {
	server, _ := newDesktopTestServer(t)
	cases := []struct {
		name        string
		body        string
		contentType string
	}{
		{name: "old jsonl transcript", body: `{"type":"metadata"}` + "\n", contentType: "application/x-ndjson"},
		{name: "wrong media type", body: validConversationShareEventStream, contentType: "application/json"},
		{name: "unsupported version", body: strings.Replace(validConversationShareEventStream, `"shareVersion":1`, `"shareVersion":2`, 1), contentType: "text/event-stream"},
		{name: "oversized title", body: strings.Replace(validConversationShareEventStream, "Release plan", strings.Repeat("t", maxConversationShareTitleBytes+1), 1), contentType: "text/event-stream"},
		{name: "unknown field", body: strings.Replace(validConversationShareEventStream, `"chatName":"Release plan"`, `"chatName":"Release plan","agentKey":"secret"`, 1), contentType: "text/event-stream"},
		{name: "oversized message", body: strings.Replace(validConversationShareEventStream, "hello", strings.Repeat("m", maxConversationShareContentBytes+1), 1), contentType: "text/event-stream"},
		{name: "oversized label", body: strings.Replace(validConversationShareEventStream, "分析问题", strings.Repeat("l", maxConversationShareLabelBytes+1), 1), contentType: "text/event-stream"},
		{name: "bad sequence", body: strings.Replace(validConversationShareEventStream, `"seq":4`, `"seq":9`, 1), contentType: "text/event-stream"},
		{name: "fractional time", body: strings.Replace(validConversationShareEventStream, `"timestamp":1700000000500`, `"timestamp":1700000000500.5`, 1), contentType: "text/event-stream"},
		{name: "missing done", body: strings.Replace(validConversationShareEventStream, "event: message\ndata: [DONE]\n\n", "", 1), contentType: "text/event-stream"},
		{name: "trailing frame", body: validConversationShareEventStream + "event: message\ndata: [DONE]\n\n", contentType: "text/event-stream"},
		{name: "broad sse syntax", body: strings.Replace(validConversationShareEventStream, "event: message\ndata:", "id: 1\nevent: message\ndata:", 1), contentType: "text/event-stream"},
		{name: "unsupported media parameter", body: validConversationShareEventStream, contentType: "text/event-stream; profile=live"},
		{name: "unsupported event", body: strings.Replace(validConversationShareEventStream, `"type":"content.snapshot"`, `"type":"tool.result"`, 1), contentType: "text/event-stream"},
		{name: "internal id", body: strings.Replace(validConversationShareEventStream, `"message":"hello"`, `"message":"hello","runId":"secret"`, 1), contentType: "text/event-stream"},
		{name: "second query without terminal", body: strings.Replace(validConversationShareEventStream, `"type":"content.snapshot","text":"hi"`, `"type":"request.query","message":"again"`, 1), contentType: "text/event-stream"},
		{name: "terminal before run start", body: strings.Replace(validConversationShareEventStream, `"timestamp":1700000001000}`, `"timestamp":1700000000001}`, 1), contentType: "text/event-stream"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := performConversationShareRequestWithContentType(server, http.MethodPost, conversationSharesPath, tc.body, defaultDesktopJWT, tc.contentType)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("invalid event stream status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestConversationShareAPIAllowsAnIncompleteLastTurnAndMissingReasoningLabel(t *testing.T) {
	cfg := desktopTestConfig(t)
	cfg.SharePublicBaseURL = "https://share.example.test"
	server, _ := newDesktopTestServerWithConfig(t, cfg)
	stream := shareEventStream(
		`{"seq":1,"type":"chat.start","shareVersion":1,"chatName":"Incomplete","timestamp":1700000000000}`,
		`{"seq":2,"type":"request.query","message":"hello","timestamp":1700000000000}`,
		`{"seq":3,"type":"reasoning.snapshot","text":"thinking","timestamp":1700000000100}`,
	)
	rec := performConversationShareRequest(server, http.MethodPost, conversationSharesPath, stream, defaultDesktopJWT)
	if rec.Code != http.StatusCreated {
		t.Fatalf("incomplete turn status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestConversationShareAPIRejectsTooManyEvents(t *testing.T) {
	server, _ := newDesktopTestServer(t)
	events := []string{
		`{"seq":1,"type":"chat.start","shareVersion":1,"chatName":"Many","timestamp":1700000000000}`,
		`{"seq":2,"type":"request.query","message":"hello","timestamp":1700000000000}`,
	}
	for seq := 3; seq <= maxConversationShareEvents+2; seq++ {
		events = append(events, fmt.Sprintf(`{"seq":%d,"type":"content.snapshot","text":"x","timestamp":1700000000001}`, seq))
	}
	rec := performConversationShareRequest(server, http.MethodPost, conversationSharesPath, shareEventStream(events...), defaultDesktopJWT)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("too many events status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestConversationShareAPIRejectsOversizedEventStream(t *testing.T) {
	server, _ := newDesktopTestServer(t)
	rec := performConversationShareRequest(
		server,
		http.MethodPost,
		conversationSharesPath,
		strings.Repeat("x", int(maxConversationShareBytes)+1),
		defaultDesktopJWT,
	)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized event stream status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestConversationShareIDIsOpaque(t *testing.T) {
	if id, ok := conversationShareIDFromPath("/api/public/shares/opaque-abc_123", publicConversationSharesPath); !ok || id != "opaque-abc_123" {
		t.Fatalf("prefixless id=%q ok=%t", id, ok)
	}
	for _, path := range []string{"/api/public/shares/", "/api/public/shares/a/b", "/api/public/shares/bad.id", "/api/public/shares/" + strings.Repeat("a", 81)} {
		if _, ok := conversationShareIDFromPath(path, publicConversationSharesPath); ok {
			t.Fatalf("invalid path accepted: %q", path)
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

func shareEventStream(events ...string) string {
	frames := make([]string, 0, len(events)+2)
	for _, event := range events {
		frames = append(frames, "event: message\ndata: "+event)
	}
	frames = append(frames, "event: message\ndata: [DONE]", "")
	return strings.Join(frames, "\n\n")
}

func performConversationShareRequest(server *Server, method, path, body, token string) *httptest.ResponseRecorder {
	return performConversationShareRequestWithContentType(server, method, path, body, token, "text/event-stream")
}

func performConversationShareRequestWithContentType(server *Server, method, path, body, token, contentType string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	return rec
}
