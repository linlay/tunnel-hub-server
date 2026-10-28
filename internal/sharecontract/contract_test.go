package sharecontract

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func examples(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile("testdata/examples.json")
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(data, &values); err != nil {
		t.Fatal(err)
	}
	return values
}

func TestBusinessExamplesUseOneEnvelope(t *testing.T) {
	values := examples(t)
	if len(values) != 4 {
		t.Fatal("expected conversation, market, preview, service")
	}
	for name, data := range values {
		t.Run(name, func(t *testing.T) {
			envelope, err := Decode(data)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(envelope)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Decode(encoded); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestEnvelopeRejectsPolicyIdentityAndBrokenReferences(t *testing.T) {
	base := examples(t)["market"]
	for _, field := range []string{`"expiresAt":null`, `"singleUse":false`, `"userId":"other"`, `"actions":[]`} {
		data := append([]byte("{"+field+","), base[1:]...)
		if _, err := Decode(data); err == nil {
			t.Fatalf("accepted forbidden field %s", field)
		}
	}
	for name, mutate := range map[string]func(*Envelope){
		"outer version":        func(e *Envelope) { e.ProtocolVersion = 2 },
		"zero content version": func(e *Envelope) { e.Content.Version = 0 },
		"zero context version": func(e *Envelope) { e.Desktop.ContextVersion = 0 },
		"invalid context":      func(e *Envelope) { e.Desktop.Context = json.RawMessage(`[]`) },
		"path escape":          func(e *Envelope) { e.Resources[0].Path = "../index.html" },
		"duplicate id":         func(e *Envelope) { e.Resources = append(e.Resources, e.Resources[0]) },
		"duplicate path": func(e *Envelope) {
			r := e.Resources[0]
			r.ID = "1123456789abcdef01234567"
			e.Resources = append(e.Resources, r)
		},
		"missing entrypoint": func(e *Envelope) { e.Preview.Entrypoint = "missing.html" },
		"entrypoint MIME":    func(e *Envelope) { e.Resources[0].MIMEType = "text/plain" },
		"missing resource":   func(e *Envelope) { e.Content = Content{Version: 1, ResourceID: "missing"} },
		"two content forms":  func(e *Envelope) { e.Content.ResourceID = e.Resources[0].ID },
		"negative size":      func(e *Envelope) { e.Resources[0].Size = -1 },
		"unsafe size":        func(e *Envelope) { e.Resources[0].Size = 1 << 53 },
		"absolute source":    func(e *Envelope) { e.Source.ID = "/private/file" },
		"arbitrary binding":  func(e *Envelope) { e.Service = &Binding{Type: "url", WebAppID: "https://example.test"} },
	} {
		t.Run(name, func(t *testing.T) {
			e, err := Decode(base)
			if err != nil {
				t.Fatal(err)
			}
			mutate(&e)
			if e.Validate() == nil {
				t.Fatal("invalid manifest accepted")
			}
		})
	}
}

func TestContentContextAndResourceVersionsAreIndependent(t *testing.T) {
	e, err := Decode(examples(t)["market"])
	if err != nil {
		t.Fatal(err)
	}
	e.Content.Version = 3
	e.Desktop.ContextVersion = 2
	// Public validation accepts future business protocols. The receiver must
	// later reject 2 if it only registered contextVersion 1, never rewrite it.
	if err := e.Validate(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(e.Desktop.Context), `"resourceVersion": "1.2.3"`) {
		t.Fatal("resource version was changed by envelope validation")
	}
}

func TestPublicQuerySelectsExactlyOneBehavior(t *testing.T) {
	for query, expected := range map[string]Action{
		"": Preview, "action=preview": Preview, "action=desktop": Desktop,
		"action=resolve": Resolve, "action=service": Service,
		"action=download&resourceId=0123456789abcdef01234567": Download,
		"theme=dark&lang=zh": Preview,
	} {
		got, err := ParseQuery(query)
		if err != nil || got.Action != expected {
			t.Fatalf("%q: %v, %v", query, got, err)
		}
	}
	for _, query := range []string{
		"action=", "action=preview&action=desktop", "action=install", "action=download",
		"resourceId=0123456789abcdef01234567", "action=download&resourceId=../file",
		"action=download&resourceId=a&resourceId=b", "action=desktop&receiverId=evil",
		"contextVersion=2", "resourceVersion=latest", "context=%7B%7D", "userId=other",
		"token=secret", "url=https://example.test", "attachment=file", "action=%ZZ",
	} {
		if _, err := ParseQuery(query); err == nil {
			t.Fatalf("accepted %q", query)
		}
	}
}

func TestResourcePathsArePortableAndCanonical(t *testing.T) {
	for _, value := range []string{"index.html", "assets/script.js", "images/plot_1-2.png"} {
		if !ValidResourcePath(value) {
			t.Fatalf("rejected %q", value)
		}
	}
	for _, value := range []string{"", "/a", "../a", "a/./b", "a//b", "a/", `a\b`, "C:/a", "a%2fb", "a?b", "a#b", "a\x00b", strings.Repeat("a", 256)} {
		if ValidResourcePath(value) {
			t.Fatalf("accepted %q", value)
		}
	}
}
