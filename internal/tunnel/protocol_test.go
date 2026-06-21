package tunnel

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestJSONRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	want := StreamRequest{
		V:     ProtocolVersion,
		NS:    NamespaceWebApp,
		Frame: FrameRequest,
		Type:  TypeWebAppHTTPRequest,
		ID:    "req_1",
		Payload: &StreamPayload{
			Public: &PublicRequest{
				Method:  http.MethodPost,
				Path:    "/api/ping?x=1",
				Host:    "app.example.com",
				Headers: http.Header{"X-Test": []string{"ok"}},
			},
			Upstream: &UpstreamTarget{
				Scheme: "http",
				Host:   "127.0.0.1",
				Port:   8080,
			},
			BodyLength: Int64Ptr(4),
		},
	}
	if err := WriteJSON(&buf, want); err != nil {
		t.Fatalf("write: %v", err)
	}
	var got StreamRequest
	if err := ReadJSON(&buf, &got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Payload == nil || got.Payload.Public == nil || got.Payload.Public.Path != want.Payload.Public.Path || got.Payload.Public.Headers.Get("X-Test") != "ok" {
		t.Fatalf("round trip mismatch: %#v", got)
	}
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var topLevel map[string]json.RawMessage
	if err := json.Unmarshal(raw, &topLevel); err != nil {
		t.Fatalf("unmarshal top-level: %v", err)
	}
	for _, field := range []string{"public", "upstream", "route", "bodyLength"} {
		if _, ok := topLevel[field]; ok {
			t.Fatalf("standard request should not include top-level %q: %s", field, string(raw))
		}
	}
}

func TestStandardResponseRoundTrip(t *testing.T) {
	bodyLength := int64(123)
	want := NewSuccessResponse(NamespaceWebApp, TypeWebAppHTTPRequest, "req_1", &StreamResponseData{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"X-Test": []string{"ok"}},
		BodyLength: &bodyLength,
	})
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"code":0`) {
		t.Fatalf("success response should include code 0: %s", string(raw))
	}

	var buf bytes.Buffer
	if err := WriteJSON(&buf, want); err != nil {
		t.Fatalf("write: %v", err)
	}
	var got StreamResponse
	if err := ReadJSON(&buf, &got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Frame != FrameResponse || got.Code != 0 || got.Msg != "success" || got.Data == nil || got.Data.StatusCode != http.StatusOK || StreamResponseBodyLength(got) != 123 {
		t.Fatalf("response mismatch: %#v", got)
	}
}

func TestStandardErrorRoundTrip(t *testing.T) {
	want := NewErrorResponse(NamespaceDesktop, TypeTunnelOpen, "tun_1", http.StatusUnauthorized, "invalid agent token")
	var buf bytes.Buffer
	if err := WriteJSON(&buf, want); err != nil {
		t.Fatalf("write: %v", err)
	}
	var got StreamResponse
	if err := ReadJSON(&buf, &got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Frame != FrameError || got.Type != TypeTunnelOpen || got.Code != http.StatusUnauthorized || got.Msg != "invalid agent token" {
		t.Fatalf("error mismatch: %#v", got)
	}
}

func TestParseUpstreamTarget(t *testing.T) {
	got, err := ParseUpstreamTarget("http://127.0.0.1:3000/base", true)
	if err != nil {
		t.Fatalf("parse upstream: %v", err)
	}
	if got.Scheme != "ws" || got.Host != "127.0.0.1" || got.Port != 3000 || got.BasePath != "/base" {
		t.Fatalf("unexpected upstream: %#v", got)
	}
}

func TestBuildTargetURL(t *testing.T) {
	got, err := BuildTargetURL("http://127.0.0.1:3000/base", "/socket?q=1", true)
	if err != nil {
		t.Fatalf("build target: %v", err)
	}
	want := "ws://127.0.0.1:3000/base/socket?q=1"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
