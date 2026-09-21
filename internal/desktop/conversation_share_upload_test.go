package desktop

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"example.invalid/tunnel-hub-server/internal/store"
)

func TestConversationShareUploadRequiresEveryFrozenAttachment(t *testing.T) {
	body := []byte("<html><script>alert(1)</script><p>报告</p></html>")
	hash := sha256.Sum256(body)
	const id = "0123456789abcdef01234567"
	snapshot, _ := json.Marshal(map[string]any{"version": 1, "attachments": []any{
		map[string]any{"id": id, "name": "报告.html", "mimeType": "text/html", "sourceRef": "artifacts/run-1/report.html", "size": len(body), "sha256": hex.EncodeToString(hash[:])},
	}})
	makeRequest := func(include, changed bool) *http.Request {
		var buffer bytes.Buffer
		writer := multipart.NewWriter(&buffer)
		part, _ := writer.CreateFormField("snapshot")
		_, _ = part.Write(snapshot)
		if include {
			part, _ = writer.CreateFormFile("attachment:"+id, "报告.html")
			content := body
			if changed {
				content = []byte("changed")
			}
			_, _ = part.Write(content)
		}
		_ = writer.Close()
		request := httptest.NewRequest(http.MethodPost, "/api/desktop/shares", &buffer)
		request.Header.Set("Content-Type", writer.FormDataContentType())
		return request
	}
	uploaded, attachments, err := decodeConversationShare(httptest.NewRecorder(), makeRequest(true, false))
	if err != nil || len(uploaded) == 0 || len(attachments) != 1 || attachments[0].Name != "报告.html" {
		t.Fatalf("upload = %d %+v %v", len(uploaded), attachments, err)
	}
	for _, request := range []*http.Request{makeRequest(false, false), makeRequest(true, true)} {
		if _, _, err := decodeConversationShare(httptest.NewRecorder(), request); err == nil {
			t.Fatal("incomplete or changed attachment was accepted")
		}
	}
}

func TestSanitizeSharedHTMLRemovesActiveContent(t *testing.T) {
	source := []byte(`<html><head><style>p{color:red}</style></head><body><script>alert(1)</script><p style="color:blue">safe</p><img src="https://bad.example/x.png"><img src="data:image/png;base64,AAAA"><iframe src="https://bad.example"></iframe></body></html>`)
	result := string(sanitizeSharedHTML(source))
	for _, fragment := range []string{"alert(1)", "<script", "<iframe", "bad.example"} {
		if strings.Contains(result, fragment) {
			t.Fatalf("unsafe fragment %q in %s", fragment, result)
		}
	}
	if !strings.Contains(result, "safe") || !strings.Contains(result, "data:image/png;base64,AAAA") {
		t.Fatalf("safe content lost: %s", result)
	}
}

func TestConversationShareOnceSessionReadsFrozenAttachment(t *testing.T) {
	cfg := desktopTestConfig(t)
	cfg.SharePublicBaseURL = "https://share.example.test"
	server, db := newDesktopTestServerWithConfig(t, cfg)
	now := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	server.now = func() time.Time { return now }
	const id = "0123456789abcdef01234567"
	body := []byte("<h1>报告</h1><script>alert(1)</script>")
	hash := sha256.Sum256(body)
	snapshot, _ := json.Marshal(map[string]any{"version": 1, "attachments": []any{
		map[string]any{"id": id, "name": "报告.html", "mimeType": "text/html",
			"sourceRef": "artifacts/report.html", "size": len(body), "sha256": hex.EncodeToString(hash[:])},
	}})
	var payload bytes.Buffer
	writer := multipart.NewWriter(&payload)
	part, _ := writer.CreateFormField("snapshot")
	_, _ = part.Write(snapshot)
	part, _ = writer.CreateFormFile("attachment:"+id, "报告.html")
	_, _ = part.Write(body)
	_ = writer.Close()
	request := httptest.NewRequest(http.MethodPost, conversationSharesPath, &payload)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set(conversationSnapshotVersionHeader, conversationSnapshotVersion)
	request.Header.Set(conversationShareExpirationHeader, "once")
	request.Header.Set(conversationShareConversationIDHeader, "chat-test")
	request.Header.Set("Authorization", "Bearer "+defaultDesktopJWT)
	created := httptest.NewRecorder()
	server.ServeHTTP(created, request)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	var share conversationShareRecordResponse
	if err := json.Unmarshal(created.Body.Bytes(), &share); err != nil {
		t.Fatal(err)
	}
	pagePath := publicConversationSharePagePath + share.ID
	attachmentPath := pagePath + "/attachments/" + id + "/preview"
	if head := performConversationShareRequest(server, http.MethodHead, pagePath, nil, ""); head.Code != http.StatusOK {
		t.Fatalf("HEAD status=%d", head.Code)
	}
	first := performConversationShareRequest(server, http.MethodGet, pagePath, nil, "")
	if first.Code != http.StatusOK || len(first.Result().Cookies()) != 1 {
		t.Fatalf("first access status=%d cookies=%v", first.Code, first.Result().Cookies())
	}
	if second := performConversationShareRequest(server, http.MethodGet, pagePath, nil, ""); second.Code != http.StatusNotFound {
		t.Fatalf("second browser status=%d", second.Code)
	}
	read := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(first.Result().Cookies()[0])
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)
		return rec
	}
	preview := read(attachmentPath)
	if preview.Code != http.StatusOK || !strings.Contains(preview.Body.String(), "报告") || strings.Contains(preview.Body.String(), "alert(1)") {
		t.Fatalf("preview status=%d body=%s", preview.Code, preview.Body.String())
	}
	const otherAttachmentID = "ffffffffffffffffffffffff"
	_, err := db.CreateConversationShareWithAttachments(request.Context(), "owner", "other-chat", store.ConversationSnapshotVersion,
		[]byte(`{"version":1}`), []store.ConversationShareAttachment{{ID: otherAttachmentID,
			Name: "other.html", MIMEType: "text/html", Size: int64(len(body)), SHA256: hex.EncodeToString(hash[:]), Body: body}},
		now, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if cross := read(pagePath + "/attachments/" + otherAttachmentID + "/preview"); cross.Code != http.StatusNotFound {
		t.Fatalf("cross attachment status=%d", cross.Code)
	}
	download := read(pagePath + "/attachments/" + id + "/download")
	if download.Code != http.StatusOK || !bytes.Equal(download.Body.Bytes(), body) {
		t.Fatalf("download status=%d body=%s", download.Code, download.Body.String())
	}
	now = now.Add(31 * time.Minute)
	if expired := read(attachmentPath); expired.Code != http.StatusNotFound {
		t.Fatalf("expired attachment status=%d", expired.Code)
	}
}
