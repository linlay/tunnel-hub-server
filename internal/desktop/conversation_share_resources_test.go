package desktop

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"example.invalid/tunnel-hub-server/internal/store"
)

func TestConversationSnapshotResourceGraph(t *testing.T) {
	const htmlID = "111111111111111111111111"
	const imageID = "222222222222222222222222"
	const missingID = "333333333333333333333333"
	base := `{"version":1,"attachments":[{"id":"` + htmlID + `","sourceRef":"artifacts/run/report.html","name":"report.html","mimeType":"text/html","size":0,"sha256":"` + strings.Repeat("a", 64) + `","mediaRefs":{"image.png":"` + imageID + `","missing.png":"` + missingID + `"}},{"id":"` + imageID + `","sourceRef":"image.png","name":"image.png","mimeType":"image/png","size":0,"sha256":"` + strings.Repeat("b", 64) + `"}],"omittedResources":[{"id":"` + missingID + `","sourceRef":"missing.png","name":"missing.png","reason":"not_found"}],"turns":[{"nodes":[{"attachmentIds":["` + imageID + `","` + missingID + `"]}]}]}`
	for name, snapshot := range map[string]string{"legacy": `{"version":1}`, "graph": base} {
		if _, err := parseConversationSnapshotResources([]byte(snapshot)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	for name, change := range map[string]func(map[string]any){
		"success omission collision": func(v map[string]any) { v["omittedResources"].([]any)[0].(map[string]any)["id"] = imageID },
		"unknown node target": func(v map[string]any) {
			v["turns"].([]any)[0].(map[string]any)["nodes"].([]any)[0].(map[string]any)["attachmentIds"] = []string{strings.Repeat("f", 24)}
		},
		"null node targets": func(v map[string]any) {
			v["turns"].([]any)[0].(map[string]any)["nodes"].([]any)[0].(map[string]any)["attachmentIds"] = nil
		},
		"duplicate node target": func(v map[string]any) {
			v["turns"].([]any)[0].(map[string]any)["nodes"].([]any)[0].(map[string]any)["attachmentIds"] = []string{imageID, imageID}
		},
		"null omissions":     func(v map[string]any) { v["omittedResources"] = nil },
		"missing final size": func(v map[string]any) { delete(v["attachments"].([]any)[0].(map[string]any), "size") },
		"null media map":     func(v map[string]any) { v["attachments"].([]any)[0].(map[string]any)["mediaRefs"] = nil },
		"non-media target":   func(v map[string]any) { v["attachments"].([]any)[1].(map[string]any)["mimeType"] = "application/pdf" },
		"unknown HTML target": func(v map[string]any) {
			v["attachments"].([]any)[0].(map[string]any)["mediaRefs"] = map[string]string{"image.png": strings.Repeat("f", 24)}
		},
	} {
		t.Run(name, func(t *testing.T) {
			var v map[string]any
			if err := json.Unmarshal([]byte(base), &v); err != nil {
				t.Fatal(err)
			}
			change(v)
			data, _ := json.Marshal(v)
			if _, err := parseConversationSnapshotResources(data); err == nil {
				t.Fatal("invalid graph accepted")
			}
		})
	}
}

func TestConversationShareNativeMediaRangeAndAuthorization(t *testing.T) {
	cfg := desktopTestConfig(t)
	cfg.SharePublicBaseURL = "https://share.example.test"
	server, db := newDesktopTestServerWithConfig(t, cfg)
	now := time.Now().UTC()
	server.now = func() time.Time { return now }
	for _, mediaType := range []string{"image/svg+xml", "audio/wav", "video/webm"} {
		t.Run(mediaType, func(t *testing.T) {
			id := strings.Repeat("a", 24)
			shareID, err := store.NewConversationShareID()
			if err != nil {
				t.Fatal(err)
			}
			body := []byte("frozen-media-bytes")
			stage, err := server.conversationShareResources.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer stage.Abort()
			written, err := stage.Write(id, bytes.NewReader(body), 1024)
			if err != nil {
				t.Fatal(err)
			}
			if err := stage.Commit(shareID); err != nil {
				t.Fatal(err)
			}
			ctx := httptest.NewRequest(http.MethodGet, "/", nil).Context()
			_, err = db.CreateConversationShareWithResources(ctx, shareID, "owner", "chat", 1, []byte(`{"version":1}`), []store.ConversationShareResource{{ID: id, Name: "media", MIMEType: mediaType, Size: written.Size, SHA256: written.SHA256}}, now, nil, true)
			if err != nil {
				t.Fatal(err)
			}
			page := publicConversationSharePagePath + shareID
			preview := page + "/attachments/" + id + "/preview"
			read := func(method, path, rangeValue string, cookie *http.Cookie) *httptest.ResponseRecorder {
				req := httptest.NewRequest(method, path, nil)
				req.Header.Set("Range", rangeValue)
				if cookie != nil {
					req.AddCookie(cookie)
				}
				rec := httptest.NewRecorder()
				server.ServeHTTP(rec, req)
				return rec
			}
			for _, method := range []string{http.MethodHead, http.MethodGet} {
				if rec := read(method, preview, "bytes=0-2", nil); rec.Code != http.StatusNotFound {
					t.Fatalf("unclaimed %s: %d", method, rec.Code)
				}
			}
			if rec := read(http.MethodHead, page, "", nil); rec.Code != http.StatusOK || rec.Body.Len() != 0 || len(rec.Result().Cookies()) != 0 {
				t.Fatalf("HEAD claimed share: %d", rec.Code)
			}
			claim := read(http.MethodGet, page, "", nil)
			if claim.Code != http.StatusOK || len(claim.Result().Cookies()) != 1 {
				t.Fatalf("claim: %d", claim.Code)
			}
			cookie := claim.Result().Cookies()[0]
			for _, path := range []string{preview, preview + "?embed=1", page + "/attachments/" + id + "/download"} {
				if rec := read(http.MethodGet, path, "bytes=0-2", nil); rec.Code != http.StatusNotFound {
					t.Fatalf("cookie bypass: %d", rec.Code)
				}
				rec := read(http.MethodGet, path, "bytes=0-2", cookie)
				if rec.Code != http.StatusPartialContent || rec.Body.String() != "fro" || rec.Header().Get("Content-Type") != mediaType || rec.Header().Get("Content-Range") != "bytes 0-2/18" {
					t.Fatalf("range: %d %v %q", rec.Code, rec.Header(), rec.Body.String())
				}
				if rec := read(http.MethodHead, path, "", cookie); rec.Code != http.StatusOK || rec.Body.Len() != 0 || rec.Header().Get("Content-Length") != "18" {
					t.Fatalf("HEAD: %d %v", rec.Code, rec.Header())
				}
				if rec := read(http.MethodGet, path, "bytes=999-", cookie); rec.Code != http.StatusRequestedRangeNotSatisfiable {
					t.Fatalf("invalid range: %d", rec.Code)
				}
			}
			now = now.Add(31 * time.Minute)
			if rec := read(http.MethodGet, preview+"?embed=1", "bytes=0-2", cookie); rec.Code != http.StatusNotFound {
				t.Fatalf("expired session: %d", rec.Code)
			}
		})
	}
}
