package desktop

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"example.invalid/tunnel-hub-server/internal/sharefiles"
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
			header := make(textproto.MIMEHeader)
			header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="attachment:%s"; filename="报告.html"`, id))
			header.Set("Content-Type", "text/html")
			part, _ = writer.CreatePart(header)
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
	files, err := sharefiles.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stage, err := files.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer stage.Abort()
	uploaded, attachments, err := decodeConversationShare(httptest.NewRecorder(), makeRequest(true, false), stage)
	if err != nil || len(uploaded) == 0 || len(attachments) != 1 || attachments[0].Name != "报告.html" {
		t.Fatalf("upload = %d %+v %v", len(uploaded), attachments, err)
	}
	for _, request := range []*http.Request{makeRequest(false, false), makeRequest(true, true)} {
		stage, beginErr := files.Begin()
		if beginErr != nil {
			t.Fatal(beginErr)
		}
		if _, _, err := decodeConversationShare(httptest.NewRecorder(), request, stage); err == nil {
			t.Fatal("incomplete or changed attachment was accepted")
		}
		_ = stage.Abort()
	}
}

func TestConversationShareUploadRejectsPartsThatDoNotMatchSnapshot(t *testing.T) {
	body := []byte("data")
	digest := sha256.Sum256(body)
	const id = "0123456789abcdef01234567"
	base := conversationSnapshotResource{
		ID: id, Name: "report.pdf", MIMEType: "application/pdf",
		SourceRef: "artifacts/run-1/report.pdf", Size: int64(len(body)), SHA256: hex.EncodeToString(digest[:]),
	}
	type uploadPart struct {
		id, name, mime string
		body           []byte
	}
	request := func(descriptors []conversationSnapshotResource, parts []uploadPart, snapshotFirst bool) *http.Request {
		snapshot, err := json.Marshal(map[string]any{"version": 1, "attachments": descriptors})
		if err != nil {
			t.Fatal(err)
		}
		var buffer bytes.Buffer
		writer := multipart.NewWriter(&buffer)
		writeSnapshot := func() {
			part, err := writer.CreateFormField("snapshot")
			if err != nil {
				t.Fatal(err)
			}
			_, _ = part.Write(snapshot)
		}
		writeParts := func() {
			for _, item := range parts {
				header := make(textproto.MIMEHeader)
				header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="attachment:%s"; filename="%s"`, item.id, item.name))
				header.Set("Content-Type", item.mime)
				part, err := writer.CreatePart(header)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = part.Write(item.body)
			}
		}
		if snapshotFirst {
			writeSnapshot()
			writeParts()
		} else {
			writeParts()
			writeSnapshot()
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, conversationSharesPath, &buffer)
		req.Header.Set("Content-Type", writer.FormDataContentType())
		return req
	}
	validPart := uploadPart{id: id, name: base.Name, mime: base.MIMEType, body: body}
	cases := []struct {
		name          string
		descriptors   []conversationSnapshotResource
		parts         []uploadPart
		snapshotFirst bool
	}{
		{name: "missing", descriptors: []conversationSnapshotResource{base}, snapshotFirst: true},
		{name: "extra", descriptors: []conversationSnapshotResource{base}, parts: []uploadPart{validPart,
			{id: "abcdef0123456789abcdef01", name: "extra.pdf", mime: "application/pdf", body: body}}, snapshotFirst: true},
		{name: "duplicate part", descriptors: []conversationSnapshotResource{base}, parts: []uploadPart{validPart, validPart}, snapshotFirst: true},
		{name: "duplicate descriptor", descriptors: []conversationSnapshotResource{base, base}, parts: []uploadPart{validPart}, snapshotFirst: true},
		{name: "wrong MIME", descriptors: []conversationSnapshotResource{base}, parts: []uploadPart{{id: id, name: base.Name, mime: "text/html", body: body}}, snapshotFirst: true},
		{name: "wrong filename", descriptors: []conversationSnapshotResource{base}, parts: []uploadPart{{id: id, name: "other.pdf", mime: base.MIMEType, body: body}}, snapshotFirst: true},
		{name: "wrong length", descriptors: []conversationSnapshotResource{{ID: base.ID, Name: base.Name, MIMEType: base.MIMEType, SourceRef: base.SourceRef, Size: base.Size + 1, SHA256: base.SHA256}}, parts: []uploadPart{validPart}, snapshotFirst: true},
		{name: "wrong hash", descriptors: []conversationSnapshotResource{{ID: base.ID, Name: base.Name, MIMEType: base.MIMEType, SourceRef: base.SourceRef, Size: base.Size, SHA256: strings.Repeat("0", 64)}}, parts: []uploadPart{validPart}, snapshotFirst: true},
		{name: "snapshot after resource", descriptors: []conversationSnapshotResource{base}, parts: []uploadPart{validPart}},
	}
	files, err := sharefiles.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stage, err := files.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer stage.Abort()
			if _, _, err := decodeConversationShare(httptest.NewRecorder(), request(tc.descriptors, tc.parts, tc.snapshotFirst), stage); err == nil {
				t.Fatal("mismatched multipart request was accepted")
			}
		})
	}
}

func TestConversationShareUploadRejectsAggregateResourceLimit(t *testing.T) {
	body := bytes.Repeat([]byte("x"), store.MaxConversationResourceBytes/2+1)
	digest := sha256.Sum256(body)
	descriptors := []conversationSnapshotResource{
		{ID: "0123456789abcdef01234567", Name: "first.bin", MIMEType: "application/octet-stream",
			SourceRef: "artifacts/run-1/first.bin", Size: int64(len(body)), SHA256: hex.EncodeToString(digest[:])},
		{ID: "abcdef0123456789abcdef01", Name: "second.bin", MIMEType: "application/octet-stream",
			SourceRef: "artifacts/run-1/second.bin", Size: int64(len(body)), SHA256: hex.EncodeToString(digest[:])},
	}
	snapshot, err := json.Marshal(map[string]any{"version": 1, "attachments": descriptors})
	if err != nil {
		t.Fatal(err)
	}
	var payload bytes.Buffer
	writer := multipart.NewWriter(&payload)
	part, _ := writer.CreateFormField("snapshot")
	_, _ = part.Write(snapshot)
	for _, descriptor := range descriptors {
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="attachment:%s"; filename="%s"`, descriptor.ID, descriptor.Name))
		header.Set("Content-Type", descriptor.MIMEType)
		part, _ = writer.CreatePart(header)
		_, _ = part.Write(body)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, conversationSharesPath, &payload)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	files, err := sharefiles.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stage, err := files.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer stage.Abort()
	if _, _, err := decodeConversationShare(httptest.NewRecorder(), request, stage); !errors.Is(err, errConversationShareTooLarge) {
		t.Fatalf("aggregate limit error=%v", err)
	}
}

func TestCanonicalArtifactSourceRefMatchesPlatformEncoding(t *testing.T) {
	for _, value := range []string{
		"artifacts/run-1/report.html",
		"artifacts/run-1/%E6%8A%A5%E5%91%8A.pdf",
		"artifacts/run-1/report+v1.pdf",
	} {
		if !canonicalArtifactSourceRef(value) {
			t.Fatalf("canonical ref rejected: %q", value)
		}
	}
	for _, value := range []string{
		"report.pdf",
		"artifacts/run-1/nested/report.pdf",
		"artifacts/run-1/报告.pdf",
		"artifacts/run-1/%e6%8a%a5%e5%91%8a.pdf",
		"artifacts/run-1/%2e%2e",
		"artifacts/run-1/%252e%252e",
		"artifacts/run-1/a%2Fb.pdf",
	} {
		if canonicalArtifactSourceRef(value) {
			t.Fatalf("non-canonical ref accepted: %q", value)
		}
	}
}

func TestConversationShareDatabaseFailureRemovesCommittedResourceDirectory(t *testing.T) {
	cfg := desktopTestConfig(t)
	cfg.SharePublicBaseURL = "https://share.example.test"
	server, db := newDesktopTestServerWithConfig(t, cfg)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	response := performConversationShareRequestWithExpiration(server, "30d", []byte(validConversationSnapshot))
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	entries, err := os.ReadDir(filepath.Join(cfg.ConversationShareResourceDir, "shares"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("database failure left resource directories: %v", entries)
	}
}

func TestConversationShareRevokeRemovesFrozenResourceDirectory(t *testing.T) {
	cfg := desktopTestConfig(t)
	cfg.SharePublicBaseURL = "https://share.example.test"
	server, _ := newDesktopTestServerWithConfig(t, cfg)
	body := []byte("%PDF-1.7\nreport")
	digest := sha256.Sum256(body)
	const id = "0123456789abcdef01234567"
	snapshot, err := json.Marshal(map[string]any{"version": 1, "attachments": []any{
		map[string]any{"id": id, "name": "报告.pdf", "mimeType": "application/pdf",
			"sourceRef": "artifacts/run-1/report.pdf", "size": len(body), "sha256": hex.EncodeToString(digest[:])},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var payload bytes.Buffer
	writer := multipart.NewWriter(&payload)
	part, _ := writer.CreateFormField("snapshot")
	_, _ = part.Write(snapshot)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="attachment:%s"; filename="报告.pdf"`, id))
	header.Set("Content-Type", "application/pdf")
	part, _ = writer.CreatePart(header)
	_, _ = part.Write(body)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, conversationSharesPath, &payload)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set(conversationSnapshotVersionHeader, conversationSnapshotVersion)
	request.Header.Set(conversationShareExpirationHeader, "30d")
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
	resourceDir := filepath.Join(cfg.ConversationShareResourceDir, "shares", share.ID)
	if _, err := os.Stat(resourceDir); err != nil {
		t.Fatalf("resource directory missing before revoke: %v", err)
	}
	revoked := performConversationShareRequest(server, http.MethodDelete, conversationSharesPath+"/"+share.ID, nil, defaultDesktopJWT)
	if revoked.Code != http.StatusNoContent {
		t.Fatalf("revoke status=%d body=%s", revoked.Code, revoked.Body.String())
	}
	if _, err := os.Stat(resourceDir); !os.IsNotExist(err) {
		t.Fatalf("revoked resource directory still exists: %v", err)
	}
	attachment := performConversationShareRequest(server, http.MethodGet,
		publicConversationSharePagePath+share.ID+"/attachments/"+id+"/download", nil, "")
	if attachment.Code != http.StatusNotFound {
		t.Fatalf("revoked attachment status=%d", attachment.Code)
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
	const pdfID = "abcdef0123456789abcdef01"
	body := []byte("<h1>报告</h1><script>alert(1)</script>")
	pdf := []byte("%PDF-1.7\nreport")
	hash := sha256.Sum256(body)
	pdfHash := sha256.Sum256(pdf)
	snapshot, _ := json.Marshal(map[string]any{"version": 1, "attachments": []any{
		map[string]any{"id": id, "name": "报告.html", "mimeType": "text/html",
			"sourceRef": "artifacts/run-1/report.html", "size": len(body), "sha256": hex.EncodeToString(hash[:])},
		map[string]any{"id": pdfID, "name": "报告.pdf", "mimeType": "application/pdf",
			"sourceRef": "artifacts/run-1/report.pdf", "size": len(pdf), "sha256": hex.EncodeToString(pdfHash[:])},
	}})
	var payload bytes.Buffer
	writer := multipart.NewWriter(&payload)
	part, _ := writer.CreateFormField("snapshot")
	_, _ = part.Write(snapshot)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="attachment:%s"; filename="报告.html"`, id))
	header.Set("Content-Type", "text/html")
	part, _ = writer.CreatePart(header)
	_, _ = part.Write(body)
	pdfHeader := make(textproto.MIMEHeader)
	pdfHeader.Set("Content-Disposition", fmt.Sprintf(`form-data; name="attachment:%s"; filename="报告.pdf"`, pdfID))
	pdfHeader.Set("Content-Type", "application/pdf")
	part, _ = writer.CreatePart(pdfHeader)
	_, _ = part.Write(pdf)
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
	otherShareID, err := store.NewConversationShareID()
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.CreateConversationShareWithResources(request.Context(), otherShareID, "owner", "other-chat", store.ConversationSnapshotVersion,
		[]byte(`{"version":1}`), []store.ConversationShareResource{{ID: otherAttachmentID,
			Name: "other.html", MIMEType: "text/html", Size: int64(len(body)), SHA256: hex.EncodeToString(hash[:])}},
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
	disposition, parameters, err := mime.ParseMediaType(download.Header().Get("Content-Disposition"))
	if err != nil || disposition != "attachment" || parameters["filename"] != "报告.html" ||
		download.Header().Get("Content-Type") != "text/html" || download.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("download headers=%v disposition=%q parameters=%v error=%v", download.Header(), disposition, parameters, err)
	}
	pdfDownload := read(pagePath + "/attachments/" + pdfID + "/download")
	if pdfDownload.Code != http.StatusOK || pdfDownload.Header().Get("Content-Type") != "application/pdf" || !bytes.Equal(pdfDownload.Body.Bytes(), pdf) {
		t.Fatalf("pdf download status=%d contentType=%q body=%q", pdfDownload.Code, pdfDownload.Header().Get("Content-Type"), pdfDownload.Body.Bytes())
	}
	if pdfPreview := read(pagePath + "/attachments/" + pdfID + "/preview"); pdfPreview.Code != http.StatusNotFound {
		t.Fatalf("pdf preview status=%d", pdfPreview.Code)
	}
	now = now.Add(31 * time.Minute)
	if expired := read(attachmentPath); expired.Code != http.StatusNotFound {
		t.Fatalf("expired attachment status=%d", expired.Code)
	}
	if err := server.CleanupConversationShareResources(request.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cfg.ConversationShareResourceDir, "shares", share.ID)); !os.IsNotExist(err) {
		t.Fatalf("expired resource directory still exists: %v", err)
	}
}
