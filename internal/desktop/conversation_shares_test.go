package desktop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"example.invalid/tunnel-hub-server/internal/config"
)

const validConversationSnapshot = `{"version":1,"title":"发布计划","createdAt":1786928523000,"capturedAt":1786928523000,"turns":[]}`

func TestConversationShareAPICreateReadExpireAndRevoke(t *testing.T) {
	cfg := desktopTestConfig(t)
	cfg.SharePublicBaseURL = "https://share.example.test"
	server, _ := newDesktopTestServerWithConfig(t, cfg)
	now := time.Date(2026, time.August, 17, 1, 2, 3, 0, time.UTC)
	server.now = func() time.Time { return now }
	unauthorized := performConversationShareRequest(server, http.MethodPost, conversationSharesPath, []byte(validConversationSnapshot), "")
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d body=%s", unauthorized.Code, unauthorized.Body.String())
	}
	created := performConversationShareRequest(server, http.MethodPost, conversationSharesPath, []byte(validConversationSnapshot), defaultDesktopJWT)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	var result conversationShareRecordResponse
	if err := json.Unmarshal(created.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if !strings.HasPrefix(result.ID, "share_") || result.URL != "https://share.example.test/share/"+result.ID {
		t.Fatalf("unexpected create response: %#v", result)
	}
	if result.ConversationID != "chat-test" {
		t.Fatalf("conversationId=%q", result.ConversationID)
	}
	if result.CreatedAt != "2026-08-17T01:02:03.000Z" || result.ExpiresAt == nil || *result.ExpiresAt != "2026-09-16T01:02:03.000Z" {
		t.Fatalf("unexpected timestamps: %#v", result)
	}
	if result.LastAccessedAt != nil {
		t.Fatalf("new share lastAccessedAt=%v", result.LastAccessedAt)
	}
	if result.SingleUse {
		t.Fatal("30-day share must not be single-use")
	}
	listed := performConversationShareRequest(server, http.MethodGet, conversationSharesPath, nil, defaultDesktopJWT)
	assertConversationShareList(t, listed, result.ID, nil)
	public := performConversationShareRequest(server, http.MethodGet, publicConversationSharePagePath+result.ID, nil, "")
	if public.Code != http.StatusOK || public.Body.String() != validConversationSnapshot {
		t.Fatalf("public document mismatch status=%d body=%q", public.Code, public.Body.String())
	}
	wantHeaders := map[string]string{
		"Content-Type":           "text/html; charset=utf-8",
		"Content-Length":         strconv.Itoa(len(validConversationSnapshot)),
		"Cache-Control":          "no-store",
		"X-Content-Type-Options": "nosniff",
		"X-Robots-Tag":           "noindex, nofollow, noarchive",
		"Referrer-Policy":        "no-referrer",
	}
	for name, want := range wantHeaders {
		if got := public.Header().Get(name); got != want {
			t.Fatalf("%s=%q want=%q", name, got, want)
		}
	}
	if got := public.Header().Get("Content-Security-Policy"); got != "" {
		t.Fatalf("generated page CSP must remain controlled by its template, got=%q", got)
	}
	listed = performConversationShareRequest(server, http.MethodGet, conversationSharesPath, nil, defaultDesktopJWT)
	assertConversationShareList(t, listed, result.ID, &result.CreatedAt)
	now = now.Add(30 * 24 * time.Hour)
	expired := performConversationShareRequest(server, http.MethodGet, publicConversationSharePagePath+result.ID, nil, "")
	if expired.Code != http.StatusNotFound {
		t.Fatalf("expired public status=%d body=%s", expired.Code, expired.Body.String())
	}
	listed = performConversationShareRequest(server, http.MethodGet, conversationSharesPath, nil, defaultDesktopJWT)
	assertConversationShareList(t, listed, "", nil)
	now = now.Add(-30 * 24 * time.Hour)
	revoked := performConversationShareRequest(server, http.MethodDelete, conversationSharesPath+"/"+result.ID, nil, defaultDesktopJWT)
	if revoked.Code != http.StatusNoContent {
		t.Fatalf("revoke status=%d body=%s", revoked.Code, revoked.Body.String())
	}
	missing := performConversationShareRequest(server, http.MethodGet, publicConversationSharePagePath+result.ID, nil, "")
	if missing.Code != http.StatusNotFound || missing.Body.String() != expired.Body.String() {
		t.Fatalf("revoked response differs from expired response: status=%d body=%q", missing.Code, missing.Body.String())
	}
}

func TestConversationShareListRejectsLegacyConversationQuery(t *testing.T) {
	cfg := desktopTestConfig(t)
	cfg.SharePublicBaseURL = "https://share.example.test"
	server, _ := newDesktopTestServerWithConfig(t, cfg)
	response := performConversationShareRequest(
		server,
		http.MethodGet,
		conversationSharesPath+"?conversationId=chat-test",
		nil,
		defaultDesktopJWT,
	)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestConversationShareAPIListsAllConversationsNewestFirst(t *testing.T) {
	cfg := desktopTestConfig(t)
	cfg.SharePublicBaseURL = "https://share.example.test"
	server, _ := newDesktopTestServerWithConfig(t, cfg)
	now := time.Date(2026, time.August, 17, 1, 2, 3, 0, time.UTC)
	server.now = func() time.Time { return now }

	first := performConversationShareRequestWithExpirationAndConversationID(
		server, "30d", "chat-first", []byte(validConversationSnapshot),
	)
	now = now.Add(time.Minute)
	second := performConversationShareRequestWithExpirationAndConversationID(
		server, "30d", "chat-second", []byte(validConversationSnapshot),
	)
	if first.Code != http.StatusCreated || second.Code != http.StatusCreated {
		t.Fatalf("create statuses=%d,%d", first.Code, second.Code)
	}

	listed := performConversationShareRequest(server, http.MethodGet, conversationSharesPath, nil, defaultDesktopJWT)
	if listed.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", listed.Code, listed.Body.String())
	}
	var result conversationShareListResponse
	if err := json.Unmarshal(listed.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(result.Items) != 2 || result.Items[0].ConversationID != "chat-second" || result.Items[1].ConversationID != "chat-first" {
		t.Fatalf("items=%#v", result.Items)
	}
}

func TestConversationShareAPIExpirationOptions(t *testing.T) {
	cfg := desktopTestConfig(t)
	cfg.SharePublicBaseURL = "https://share.example.test"
	server, _ := newDesktopTestServerWithConfig(t, cfg)
	now := time.Date(2026, time.August, 17, 1, 2, 3, 0, time.UTC)
	server.now = func() time.Time { return now }

	for _, tc := range []struct {
		value    string
		duration time.Duration
	}{
		{value: "3h", duration: 3 * time.Hour},
		{value: "1d", duration: 24 * time.Hour},
		{value: "7d", duration: 7 * 24 * time.Hour},
		{value: "30d", duration: 30 * 24 * time.Hour},
	} {
		t.Run(tc.value, func(t *testing.T) {
			created := performConversationShareRequestWithExpiration(server, tc.value, []byte(validConversationSnapshot))
			if created.Code != http.StatusCreated {
				t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
			}
			var result conversationShareRecordResponse
			if err := json.Unmarshal(created.Body.Bytes(), &result); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			want := now.Add(tc.duration).Format("2006-01-02T15:04:05.000Z07:00")
			if result.ExpiresAt == nil || *result.ExpiresAt != want {
				t.Fatalf("expiresAt=%v want=%q", result.ExpiresAt, want)
			}
		})
	}

	t.Run("permanent", func(t *testing.T) {
		created := performConversationShareRequestWithExpiration(server, "permanent", []byte(validConversationSnapshot))
		if created.Code != http.StatusCreated {
			t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
		}
		var result conversationShareRecordResponse
		if err := json.Unmarshal(created.Body.Bytes(), &result); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if result.ExpiresAt != nil {
			t.Fatalf("permanent expiresAt=%v", result.ExpiresAt)
		}
		if result.SingleUse {
			t.Fatal("permanent share must not be single-use")
		}
		now = now.Add(100 * 365 * 24 * time.Hour)
		public := performConversationShareRequest(server, http.MethodGet, publicConversationSharePagePath+result.ID, nil, "")
		if public.Code != http.StatusOK {
			t.Fatalf("permanent public status=%d body=%s", public.Code, public.Body.String())
		}
	})

	t.Run("once", func(t *testing.T) {
		created := performConversationShareRequestWithExpiration(server, "once", []byte(validConversationSnapshot))
		if created.Code != http.StatusCreated {
			t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
		}
		var result conversationShareRecordResponse
		if err := json.Unmarshal(created.Body.Bytes(), &result); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if !result.SingleUse || result.ExpiresAt != nil || result.LastAccessedAt != nil {
			t.Fatalf("unexpected single-use response: %#v", result)
		}
	})
}

func TestConversationShareAPIRejectsRemovedExpirationOptions(t *testing.T) {
	cfg := desktopTestConfig(t)
	cfg.SharePublicBaseURL = "https://share.example.test"
	server, _ := newDesktopTestServerWithConfig(t, cfg)
	for _, expiration := range []string{"5m", "30m", "1h", "5d", "15d"} {
		t.Run(expiration, func(t *testing.T) {
			response := performConversationShareRequestWithExpiration(
				server,
				expiration,
				bytes.Repeat([]byte("x"), int(maxConversationSnapshotBytes)+1),
			)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestConversationShareSingleUseGETClaimsOneBrowserSession(t *testing.T) {
	cfg := desktopTestConfig(t)
	cfg.SharePublicBaseURL = "https://share.example.test"
	server, _ := newDesktopTestServerWithConfig(t, cfg)
	created := performConversationShareRequestWithExpiration(server, "once", []byte(validConversationSnapshot))
	var result conversationShareRecordResponse
	if err := json.Unmarshal(created.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	listed := performConversationShareRequest(server, http.MethodGet, conversationSharesPath, nil, defaultDesktopJWT)
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), result.ID) {
		t.Fatalf("single-use share not listed: status=%d body=%s", listed.Code, listed.Body.String())
	}

	var accessWrites atomic.Int64
	server.recordConversationShareAccess = func(context.Context, string, time.Time) error {
		accessWrites.Add(1)
		return nil
	}
	const readers = 12
	codes := make(chan int, readers)
	var wait sync.WaitGroup
	wait.Add(readers)
	for range readers {
		go func() {
			defer wait.Done()
			response := performConversationShareRequest(server, http.MethodGet, publicConversationSharePagePath+result.ID, nil, "")
			if response.Code == http.StatusOK && response.Body.String() != validConversationSnapshot {
				codes <- 0
				return
			}
			codes <- response.Code
		}()
	}
	wait.Wait()
	close(codes)
	okCount := 0
	notFoundCount := 0
	for code := range codes {
		switch code {
		case http.StatusOK:
			okCount++
		case http.StatusNotFound:
			notFoundCount++
		default:
			t.Fatalf("unexpected concurrent status=%d", code)
		}
	}
	if okCount != 1 || notFoundCount != readers-1 {
		t.Fatalf("ok=%d notFound=%d", okCount, notFoundCount)
	}
	if accessWrites.Load() != 0 {
		t.Fatalf("single-use share wrote access metadata %d times", accessWrites.Load())
	}
	listed = performConversationShareRequest(server, http.MethodGet, conversationSharesPath, nil, defaultDesktopJWT)
	assertConversationShareList(t, listed, result.ID, nil)
}

func TestConversationShareSingleUseHEADDoesNotConsume(t *testing.T) {
	cfg := desktopTestConfig(t)
	cfg.SharePublicBaseURL = "https://share.example.test"
	server, _ := newDesktopTestServerWithConfig(t, cfg)
	created := performConversationShareRequestWithExpiration(server, "once", []byte(validConversationSnapshot))
	var result conversationShareRecordResponse
	if err := json.Unmarshal(created.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	head := performConversationShareRequest(server, http.MethodHead, publicConversationSharePagePath+result.ID, nil, "")
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Fatalf("HEAD status=%d allow=%q", head.Code, head.Header().Get("Allow"))
	}
	get := performConversationShareRequest(server, http.MethodGet, publicConversationSharePagePath+result.ID, nil, "")
	if get.Code != http.StatusOK || get.Body.String() != validConversationSnapshot {
		t.Fatalf("GET after HEAD status=%d body=%q", get.Code, get.Body.String())
	}
}

func TestConversationShareSingleUseReadAndRevokeRaceEndsRevoked(t *testing.T) {
	cfg := desktopTestConfig(t)
	cfg.SharePublicBaseURL = "https://share.example.test"
	server, _ := newDesktopTestServerWithConfig(t, cfg)
	created := performConversationShareRequestWithExpiration(server, "once", []byte(validConversationSnapshot))
	var result conversationShareRecordResponse
	if err := json.Unmarshal(created.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	getCode := make(chan int, 1)
	deleteCode := make(chan int, 1)
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		getCode <- performConversationShareRequest(server, http.MethodGet, publicConversationSharePagePath+result.ID, nil, "").Code
	}()
	go func() {
		defer wait.Done()
		deleteCode <- performConversationShareRequest(server, http.MethodDelete, conversationSharesPath+"/"+result.ID, nil, defaultDesktopJWT).Code
	}()
	wait.Wait()
	if code := <-deleteCode; code != http.StatusNoContent {
		t.Fatalf("revoke status=%d", code)
	}
	if code := <-getCode; code != http.StatusOK && code != http.StatusNotFound {
		t.Fatalf("read status=%d", code)
	}
	if response := performConversationShareRequest(server, http.MethodGet, publicConversationSharePagePath+result.ID, nil, ""); response.Code != http.StatusNotFound {
		t.Fatalf("revoked share status=%d", response.Code)
	}
}

func TestConversationShareSingleUseSupportsMaximumSnapshotSize(t *testing.T) {
	cfg := desktopTestConfig(t)
	cfg.SharePublicBaseURL = "https://share.example.test"
	server, _ := newDesktopTestServerWithConfig(t, cfg)
	body := conversationSnapshotOfSize(t, int(maxConversationSnapshotBytes))
	created := performConversationShareRequestWithExpiration(server, "once", body)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	var result conversationShareRecordResponse
	if err := json.Unmarshal(created.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	public := performConversationShareRequest(server, http.MethodGet, publicConversationSharePagePath+result.ID, nil, "")
	if public.Code != http.StatusOK || public.Body.Len() != len(body) {
		t.Fatalf("public status=%d bytes=%d", public.Code, public.Body.Len())
	}
	second := performConversationShareRequest(server, http.MethodGet, publicConversationSharePagePath+result.ID, nil, "")
	if second.Code != http.StatusNotFound {
		t.Fatalf("second status=%d", second.Code)
	}
}

func TestConversationShareAPIRejectsInvalidExpirationBeforeSnapshotBody(t *testing.T) {
	cfg := desktopTestConfig(t)
	cfg.SharePublicBaseURL = "https://share.example.test"
	server, _ := newDesktopTestServerWithConfig(t, cfg)
	response := performConversationShareRequestWithExpiration(
		server,
		"90d",
		bytes.Repeat([]byte("x"), int(maxConversationSnapshotBytes)+1),
	)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestConversationShareAPIRequiresHeadersBeforeReadingSnapshot(t *testing.T) {
	cfg := desktopTestConfig(t)
	cfg.SharePublicBaseURL = "https://share.example.test"
	server, _ := newDesktopTestServerWithConfig(t, cfg)
	for _, header := range []string{
		conversationSnapshotVersionHeader,
		conversationShareConversationIDHeader,
		conversationShareExpirationHeader,
	} {
		t.Run(header, func(t *testing.T) {
			req := httptest.NewRequest(
				http.MethodPost,
				conversationSharesPath,
				bytes.NewReader(bytes.Repeat([]byte("x"), int(maxConversationSnapshotBytes)+1)),
			)
			req.Header.Set("Content-Type", "application/json; charset=utf-8")
			req.Header.Set(conversationSnapshotVersionHeader, conversationSnapshotVersion)
			req.Header.Set(conversationShareConversationIDHeader, "chat-test")
			req.Header.Set(conversationShareExpirationHeader, "30d")
			req.Header.Set("Authorization", "Bearer "+defaultDesktopJWT)
			req.Header.Del(header)
			rec := httptest.NewRecorder()
			server.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestConversationShareAccessWriteFailureDoesNotBreakPublicPage(t *testing.T) {
	cfg := desktopTestConfig(t)
	cfg.SharePublicBaseURL = "https://share.example.test"
	server, _ := newDesktopTestServerWithConfig(t, cfg)
	created := performConversationShareRequest(server, http.MethodPost, conversationSharesPath, []byte(validConversationSnapshot), defaultDesktopJWT)
	var result conversationShareRecordResponse
	if err := json.Unmarshal(created.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	server.recordConversationShareAccess = func(context.Context, string, time.Time) error {
		return errors.New("access write failed")
	}
	public := performConversationShareRequest(server, http.MethodGet, publicConversationSharePagePath+result.ID, nil, "")
	if public.Code != http.StatusOK || public.Body.String() != validConversationSnapshot {
		t.Fatalf("public status=%d body=%q", public.Code, public.Body.String())
	}
}

func TestConversationShareAPIValidatesTheSnapshotTransportContract(t *testing.T) {
	cfg := desktopTestConfig(t)
	cfg.SharePublicBaseURL = "https://share.example.test"
	server, _ := newDesktopTestServerWithConfig(t, cfg)
	accepted := performConversationShareRequest(server, http.MethodPost, conversationSharesPath, []byte(validConversationSnapshot), defaultDesktopJWT)
	if accepted.Code != http.StatusCreated {
		t.Fatalf("valid Snapshot payload status=%d body=%s", accepted.Code, accepted.Body.String())
	}
	for _, tc := range []struct {
		name        string
		body        []byte
		contentType string
		version     string
		wantStatus  int
	}{
		{name: "empty", contentType: "multipart", version: "1", wantStatus: http.StatusBadRequest},
		{name: "invalid utf8", body: []byte{0xff}, contentType: "multipart", version: "1", wantStatus: http.StatusBadRequest},
		{name: "invalid json", body: []byte("x"), contentType: "multipart", version: "1", wantStatus: http.StatusBadRequest},
		{name: "wrong media type", body: []byte(validConversationSnapshot), contentType: "text/html", version: "1", wantStatus: http.StatusBadRequest},
		{name: "wrong header version", body: []byte(validConversationSnapshot), contentType: "multipart", version: "9", wantStatus: http.StatusBadRequest},
		{name: "wrong payload version", body: []byte(`{"version":9}`), contentType: "multipart", version: "1", wantStatus: http.StatusBadRequest},
		{name: "oversized", body: bytes.Repeat([]byte("x"), int(maxConversationSnapshotBytes)+1), contentType: "multipart", version: "1", wantStatus: http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, contentType := tc.body, tc.contentType
			if contentType == "multipart" {
				body, contentType = conversationShareMultipart(tc.body)
			}
			rec := performConversationShareRequestWithHeaders(server, http.MethodPost, conversationSharesPath, body, defaultDesktopJWT, contentType, tc.version)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}

	chunkedBody, chunkedContentType := conversationShareMultipart(bytes.Repeat([]byte("x"), int(maxConversationSnapshotBytes)+1))
	chunked := httptest.NewRequest(
		http.MethodPost,
		conversationSharesPath,
		bytes.NewReader(chunkedBody),
	)
	chunked.ContentLength = -1
	chunked.TransferEncoding = []string{"chunked"}
	chunked.Header.Set("Content-Type", chunkedContentType)
	chunked.Header.Set(conversationSnapshotVersionHeader, "1")
	chunked.Header.Set(conversationShareConversationIDHeader, "chat-test")
	chunked.Header.Set(conversationShareExpirationHeader, "30d")
	chunked.Header.Set("Authorization", "Bearer "+defaultDesktopJWT)
	chunkedResponse := httptest.NewRecorder()
	server.ServeHTTP(chunkedResponse, chunked)
	if chunkedResponse.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("chunked oversized status=%d body=%s", chunkedResponse.Code, chunkedResponse.Body.String())
	}
}

func TestConversationShareIDIsOpaque(t *testing.T) {
	if id, ok := conversationShareIDFromPath("/share/opaque-abc_123", publicConversationSharePagePath); !ok || id != "opaque-abc_123" {
		t.Fatalf("prefixless id=%q ok=%t", id, ok)
	}
	for _, path := range []string{"/share/", "/share/a/b", "/share/bad.id", "/share/" + strings.Repeat("a", 81)} {
		if _, ok := conversationShareIDFromPath(path, publicConversationSharePagePath); ok {
			t.Fatalf("invalid path accepted: %q", path)
		}
	}
}

func TestConversationSharePageErrorsUseStandaloneResponsiveHTML(t *testing.T) {
	server, db := newDesktopTestServer(t)

	invalid := performConversationShareRequest(server, http.MethodGet, publicConversationSharePagePath+"bad.id", nil, "")
	missing := performConversationShareRequest(server, http.MethodGet, publicConversationSharePagePath+"share_missing", nil, "")
	if invalid.Code != http.StatusNotFound || missing.Code != http.StatusNotFound || invalid.Body.String() != missing.Body.String() {
		t.Fatalf("404 responses differ: invalid=%d %q missing=%d %q", invalid.Code, invalid.Body.String(), missing.Code, missing.Body.String())
	}
	assertPublicConversationShareHeaders(t, invalid)
	for _, marker := range []string{
		`<meta name="viewport"`,
		`class="share-error-card"`,
		`@media(prefers-color-scheme:dark)`,
		"分享不可用",
		"请向分享者确认链接是否仍然有效",
	} {
		if !strings.Contains(invalid.Body.String(), marker) {
			t.Fatalf("404 page missing %q", marker)
		}
	}
	if invalid.Body.Len() > 8*1024 || strings.Contains(invalid.Body.String(), "<script") {
		t.Fatalf("404 page must stay lightweight and script-free: bytes=%d", invalid.Body.Len())
	}

	method := performConversationShareRequest(server, http.MethodPost, publicConversationSharePagePath+"share_missing", nil, "")
	if method.Code != http.StatusMethodNotAllowed || method.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("method response status=%d allow=%q", method.Code, method.Header().Get("Allow"))
	}
	if !strings.Contains(method.Body.String(), "无法打开此页面") {
		t.Fatalf("method response body=%q", method.Body.String())
	}
	assertPublicConversationShareHeaders(t, method)

	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	failed := performConversationShareRequest(server, http.MethodGet, publicConversationSharePagePath+"share_missing", nil, "")
	if failed.Code != http.StatusInternalServerError || strings.Contains(failed.Body.String(), "database") {
		t.Fatalf("internal response status=%d body=%q", failed.Code, failed.Body.String())
	}
	if !strings.Contains(failed.Body.String(), "暂时无法打开分享") {
		t.Fatalf("internal response body=%q", failed.Body.String())
	}
	assertPublicConversationShareHeaders(t, failed)
}

func assertPublicConversationShareHeaders(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	want := map[string]string{
		"Content-Type":            "text/html; charset=utf-8",
		"Content-Language":        "zh-CN",
		"Content-Length":          strconv.Itoa(response.Body.Len()),
		"Cache-Control":           "no-store",
		"Content-Security-Policy": "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'",
		"X-Content-Type-Options":  "nosniff",
		"X-Robots-Tag":            "noindex, nofollow, noarchive",
		"Referrer-Policy":         "no-referrer",
	}
	for name, value := range want {
		if got := response.Header().Get(name); got != value {
			t.Fatalf("%s=%q want=%q", name, got, value)
		}
	}
}

func TestConversationShareURLUsesNormalizedPublicEnvironment(t *testing.T) {
	server := &Server{Config: config.RelayConfig{}}
	if _, err := server.conversationShareBaseURL(); err == nil {
		t.Fatal("expected missing public share URL to fail")
	}
	server.Config.SharePublicBaseURL = "https://share.example.test"
	if got, err := server.conversationShareBaseURL(); err != nil || got != "https://share.example.test/share" {
		t.Fatalf("url=%q err=%v", got, err)
	}
}

func performConversationShareRequest(server *Server, method, path string, body []byte, token string) *httptest.ResponseRecorder {
	if method == http.MethodPost && path == conversationSharesPath && body != nil {
		body, contentType := conversationShareMultipart(body)
		return performConversationShareRequestWithHeaders(server, method, path, body, token, contentType, conversationSnapshotVersion)
	}
	return performConversationShareRequestWithHeaders(server, method, path, body, token, "", "")
}

func performConversationShareRequestWithExpiration(server *Server, expiration string, body []byte) *httptest.ResponseRecorder {
	return performConversationShareRequestWithExpirationAndConversationID(server, expiration, "chat-test", body)
}

func performConversationShareRequestWithExpirationAndConversationID(server *Server, expiration, conversationID string, body []byte) *httptest.ResponseRecorder {
	body, contentType := conversationShareMultipart(body)
	req := httptest.NewRequest(http.MethodPost, conversationSharesPath, bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	req.Header.Set(conversationSnapshotVersionHeader, "1")
	req.Header.Set(conversationShareExpirationHeader, expiration)
	req.Header.Set(conversationShareConversationIDHeader, conversationID)
	req.Header.Set("Authorization", "Bearer "+defaultDesktopJWT)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	return rec
}

func conversationShareMultipart(snapshot []byte) ([]byte, string) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, _ := writer.CreateFormField("snapshot")
	_, _ = part.Write(snapshot)
	_ = writer.Close()
	return body.Bytes(), writer.FormDataContentType()
}

func performConversationShareRequestWithHeaders(server *Server, method, path string, body []byte, token, contentType, version string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if body != nil {
		req.Header.Set("Content-Type", contentType)
		req.Header.Set(conversationSnapshotVersionHeader, version)
		req.Header.Set(conversationShareConversationIDHeader, "chat-test")
		req.Header.Set(conversationShareExpirationHeader, "30d")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	return rec
}

func conversationSnapshotOfSize(t *testing.T, size int) []byte {
	t.Helper()
	prefix := []byte(`{"version":1,"padding":"`)
	suffix := []byte(`"}`)
	if size < len(prefix)+len(suffix) {
		t.Fatalf("snapshot size %d is too small", size)
	}
	result := make([]byte, 0, size)
	result = append(result, prefix...)
	result = append(result, bytes.Repeat([]byte("x"), size-len(prefix)-len(suffix))...)
	result = append(result, suffix...)
	return result
}

func assertConversationShareList(
	t *testing.T,
	response *httptest.ResponseRecorder,
	wantID string,
	wantLastAccessedAt *string,
) {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", response.Code, response.Body.String())
	}
	var result conversationShareListResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if wantID == "" {
		if len(result.Items) != 0 {
			t.Fatalf("list items=%#v", result.Items)
		}
		return
	}
	if len(result.Items) != 1 || result.Items[0].ID != wantID {
		t.Fatalf("list items=%#v want=%q", result.Items, wantID)
	}
	if result.Items[0].ConversationID != "chat-test" {
		t.Fatalf("conversationId=%q want=%q", result.Items[0].ConversationID, "chat-test")
	}
	if result.Items[0].SingleUse {
		t.Fatalf("reusable share listed as single-use: %#v", result.Items[0])
	}
	if wantLastAccessedAt == nil {
		if result.Items[0].LastAccessedAt != nil {
			t.Fatalf("lastAccessedAt=%v", result.Items[0].LastAccessedAt)
		}
	} else if result.Items[0].LastAccessedAt == nil || *result.Items[0].LastAccessedAt != *wantLastAccessedAt {
		t.Fatalf("lastAccessedAt=%v want=%q", result.Items[0].LastAccessedAt, *wantLastAccessedAt)
	}
}
