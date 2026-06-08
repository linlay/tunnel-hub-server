package tunnel

import (
	"bytes"
	"net/http"
	"testing"
)

func TestJSONRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	want := StreamRequest{
		Kind:       KindHTTP,
		RequestID:  "req_1",
		Method:     http.MethodPost,
		Path:       "/api/ping?x=1",
		Host:       "app.example.com",
		Target:     "http://127.0.0.1:8080",
		Header:     http.Header{"X-Test": []string{"ok"}},
		BodyLength: 4,
	}
	if err := WriteJSON(&buf, want); err != nil {
		t.Fatalf("write: %v", err)
	}
	var got StreamRequest
	if err := ReadJSON(&buf, &got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Path != want.Path || got.Header.Get("X-Test") != "ok" {
		t.Fatalf("round trip mismatch: %#v", got)
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
