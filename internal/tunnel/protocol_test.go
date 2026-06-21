package tunnel

import (
	"bytes"
	"net/http"
	"testing"
)

func TestJSONRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	want := StreamRequest{
		V:    ProtocolVersion,
		NS:   NamespaceWebApp,
		Type: TypeWebAppHTTPRequest,
		ID:   "req_1",
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
		BodyLength: 4,
	}
	if err := WriteJSON(&buf, want); err != nil {
		t.Fatalf("write: %v", err)
	}
	var got StreamRequest
	if err := ReadJSON(&buf, &got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Public == nil || got.Public.Path != want.Public.Path || got.Public.Headers.Get("X-Test") != "ok" {
		t.Fatalf("round trip mismatch: %#v", got)
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
