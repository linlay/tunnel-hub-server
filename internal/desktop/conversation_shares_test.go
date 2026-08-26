package desktop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

const validConversationHTML = "<!doctype html><html><head><title>发布计划</title><style>main{color:#123}</style></head><body><main>你好，对话分享</main><script>globalThis.__ready=true</script></body></html>"

func TestConversationShareAPICreateReadExpireAndRevoke(t *testing.T) {
	cfg := desktopTestConfig(t)
	cfg.SharePublicBaseURL = "https://share.example.test"
	server, _ := newDesktopTestServerWithConfig(t, cfg)
	now := time.Date(2026, time.August, 17, 1, 2, 3, 0, time.UTC)
	server.now = func() time.Time { return now }
	unauthorized := performConversationShareRequest(server, http.MethodPost, conversationSharesPath, []byte(validConversationHTML), "")
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d body=%s", unauthorized.Code, unauthorized.Body.String())
	}
	created := performConversationShareRequest(server, http.MethodPost, conversationSharesPath, []byte(validConversationHTML), defaultDesktopJWT)
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
	if result.CreatedAt != "2026-08-17T01:02:03.000Z" || result.ExpiresAt == nil || *result.ExpiresAt != "2026-09-16T01:02:03.000Z" {
		t.Fatalf("unexpected timestamps: %#v", result)
	}
	if result.LastAccessedAt != nil {
		t.Fatalf("new share lastAccessedAt=%v", result.LastAccessedAt)
	}
	if result.SingleUse {
		t.Fatal("30-day share must not be single-use")
	}
	listed := performConversationShareRequest(server, http.MethodGet, conversationSharesPath+"?conversationId=chat-test", nil, defaultDesktopJWT)
	assertConversationShareList(t, listed, result.ID, nil)
	public := performConversationShareRequest(server, http.MethodGet, publicConversationSharePagePath+result.ID, nil, "")
	if public.Code != http.StatusOK || public.Body.String() != validConversationHTML {
		t.Fatalf("public document mismatch status=%d body=%q", public.Code, public.Body.String())
	}
	wantHeaders := map[string]string{
		"Content-Type":           "text/html; charset=utf-8",
		"Content-Length":         strconv.Itoa(len(validConversationHTML)),
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
		t.Fatalf("stored document CSP must remain controlled by its generated HTML, got=%q", got)
	}
	listed = performConversationShareRequest(server, http.MethodGet, conversationSharesPath+"?conversationId=chat-test", nil, defaultDesktopJWT)
	assertConversationShareList(t, listed, result.ID, &result.CreatedAt)
	now = now.Add(30 * 24 * time.Hour)
	expired := performConversationShareRequest(server, http.MethodGet, publicConversationSharePagePath+result.ID, nil, "")
	if expired.Code != http.StatusNotFound {
		t.Fatalf("expired public status=%d body=%s", expired.Code, expired.Body.String())
	}
	listed = performConversationShareRequest(server, http.MethodGet, conversationSharesPath+"?conversationId=chat-test", nil, defaultDesktopJWT)
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
			created := performConversationShareRequestWithExpiration(server, tc.value, []byte(validConversationHTML))
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
		created := performConversationShareRequestWithExpiration(server, "permanent", []byte(validConversationHTML))
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
		created := performConversationShareRequestWithExpiration(server, "once", []byte(validConversationHTML))
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
				bytes.Repeat([]byte("x"), int(maxConversationShareBytes)+1),
			)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestConversationShareSingleUseGETConsumesAtomically(t *testing.T) {
	cfg := desktopTestConfig(t)
	cfg.SharePublicBaseURL = "https://share.example.test"
	server, _ := newDesktopTestServerWithConfig(t, cfg)
	created := performConversationShareRequestWithExpiration(server, "once", []byte(validConversationHTML))
	var result conversationShareRecordResponse
	if err := json.Unmarshal(created.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	listed := performConversationShareRequest(server, http.MethodGet, conversationSharesPath+"?conversationId=chat-test", nil, defaultDesktopJWT)
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
			if response.Code == http.StatusOK && response.Body.String() != validConversationHTML {
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
	listed = performConversationShareRequest(server, http.MethodGet, conversationSharesPath+"?conversationId=chat-test", nil, defaultDesktopJWT)
	assertConversationShareList(t, listed, "", nil)
}

func TestConversationShareSingleUseHEADDoesNotConsume(t *testing.T) {
	cfg := desktopTestConfig(t)
	cfg.SharePublicBaseURL = "https://share.example.test"
	server, _ := newDesktopTestServerWithConfig(t, cfg)
	created := performConversationShareRequestWithExpiration(server, "once", []byte(validConversationHTML))
	var result conversationShareRecordResponse
	if err := json.Unmarshal(created.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	head := performConversationShareRequest(server, http.MethodHead, publicConversationSharePagePath+result.ID, nil, "")
	if head.Code != http.StatusMethodNotAllowed || head.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("HEAD status=%d allow=%q", head.Code, head.Header().Get("Allow"))
	}
	get := performConversationShareRequest(server, http.MethodGet, publicConversationSharePagePath+result.ID, nil, "")
	if get.Code != http.StatusOK || get.Body.String() != validConversationHTML {
		t.Fatalf("GET after HEAD status=%d body=%q", get.Code, get.Body.String())
	}
}

func TestConversationShareSingleUseReadAndRevokeRaceHasOneWinner(t *testing.T) {
	cfg := desktopTestConfig(t)
	cfg.SharePublicBaseURL = "https://share.example.test"
	server, _ := newDesktopTestServerWithConfig(t, cfg)
	created := performConversationShareRequestWithExpiration(server, "once", []byte(validConversationHTML))
	var result conversationShareRecordResponse
	if err := json.Unmarshal(created.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	codes := make(chan int, 2)
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		codes <- performConversationShareRequest(server, http.MethodGet, publicConversationSharePagePath+result.ID, nil, "").Code
	}()
	go func() {
		defer wait.Done()
		codes <- performConversationShareRequest(server, http.MethodDelete, conversationSharesPath+"/"+result.ID, nil, defaultDesktopJWT).Code
	}()
	wait.Wait()
	close(codes)
	winners := 0
	notFound := 0
	for code := range codes {
		switch code {
		case http.StatusOK, http.StatusNoContent:
			winners++
		case http.StatusNotFound:
			notFound++
		default:
			t.Fatalf("unexpected race status=%d", code)
		}
	}
	if winners != 1 || notFound != 1 {
		t.Fatalf("winners=%d notFound=%d", winners, notFound)
	}
}

func TestConversationShareSingleUseSupportsMaximumDocumentSize(t *testing.T) {
	cfg := desktopTestConfig(t)
	cfg.SharePublicBaseURL = "https://share.example.test"
	server, _ := newDesktopTestServerWithConfig(t, cfg)
	body := bytes.Repeat([]byte("x"), int(maxConversationShareBytes))
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

func TestConversationShareAPIRejectsInvalidExpirationBeforeHTMLBody(t *testing.T) {
	cfg := desktopTestConfig(t)
	cfg.SharePublicBaseURL = "https://share.example.test"
	server, _ := newDesktopTestServerWithConfig(t, cfg)
	response := performConversationShareRequestWithExpiration(
		server,
		"90d",
		bytes.Repeat([]byte("x"), int(maxConversationShareBytes)+1),
	)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestConversationShareAPIRequiresHeadersBeforeReadingHTML(t *testing.T) {
	cfg := desktopTestConfig(t)
	cfg.SharePublicBaseURL = "https://share.example.test"
	server, _ := newDesktopTestServerWithConfig(t, cfg)
	for _, header := range []string{
		conversationDocumentVersionHeader,
		conversationShareConversationIDHeader,
		conversationShareExpirationHeader,
	} {
		t.Run(header, func(t *testing.T) {
			req := httptest.NewRequest(
				http.MethodPost,
				conversationSharesPath,
				bytes.NewReader(bytes.Repeat([]byte("x"), int(maxConversationShareBytes)+1)),
			)
			req.Header.Set("Content-Type", "text/html; charset=utf-8")
			req.Header.Set(conversationDocumentVersionHeader, conversationDocumentVersion)
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

func TestDecodeConversationHTMLAcceptsExactSizeLimit(t *testing.T) {
	req := httptest.NewRequest(
		http.MethodPost,
		conversationSharesPath,
		bytes.NewReader(bytes.Repeat([]byte("x"), int(maxConversationShareBytes))),
	)
	req.Header.Set("Content-Type", "text/html; charset=utf-8")
	req.Header.Set(conversationDocumentVersionHeader, conversationDocumentVersion)
	html, err := decodeConversationHTML(req)
	if err != nil || int64(len(html)) != maxConversationShareBytes {
		t.Fatalf("bytes=%d err=%v", len(html), err)
	}
}

func TestDecodeConversationHTMLReportsObservedSizeWithoutContentLength(t *testing.T) {
	req := httptest.NewRequest(
		http.MethodPost,
		conversationSharesPath,
		bytes.NewReader(bytes.Repeat([]byte("x"), int(maxConversationShareBytes)+1)),
	)
	req.ContentLength = -1
	req.Header.Set("Content-Type", "text/html; charset=utf-8")
	req.Header.Set(conversationDocumentVersionHeader, conversationDocumentVersion)
	_, err := decodeConversationHTML(req)
	var sizeErr *conversationShareSizeError
	if !errors.As(err, &sizeErr) || sizeErr.actual != maxConversationShareBytes+1 {
		t.Fatalf("size error=%v", err)
	}
}

func TestConversationShareAccessWriteFailureDoesNotBreakPublicPage(t *testing.T) {
	cfg := desktopTestConfig(t)
	cfg.SharePublicBaseURL = "https://share.example.test"
	server, _ := newDesktopTestServerWithConfig(t, cfg)
	created := performConversationShareRequest(server, http.MethodPost, conversationSharesPath, []byte(validConversationHTML), defaultDesktopJWT)
	var result conversationShareRecordResponse
	if err := json.Unmarshal(created.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	server.recordConversationShareAccess = func(context.Context, string, time.Time) error {
		return errors.New("access write failed")
	}
	public := performConversationShareRequest(server, http.MethodGet, publicConversationSharePagePath+result.ID, nil, "")
	if public.Code != http.StatusOK || public.Body.String() != validConversationHTML {
		t.Fatalf("public status=%d body=%q", public.Code, public.Body.String())
	}
}

func TestConversationShareAPIValidatesOnlyTheHTMLTransportContract(t *testing.T) {
	cfg := desktopTestConfig(t)
	cfg.SharePublicBaseURL = "https://share.example.test"
	server, _ := newDesktopTestServerWithConfig(t, cfg)
	accepted := performConversationShareRequest(server, http.MethodPost, conversationSharesPath, []byte("plain UTF-8, not parsed as HTML"), defaultDesktopJWT)
	if accepted.Code != http.StatusCreated {
		t.Fatalf("opaque HTML payload status=%d body=%s", accepted.Code, accepted.Body.String())
	}
	for _, tc := range []struct {
		name        string
		body        []byte
		contentType string
		version     string
		wantStatus  int
	}{
		{name: "empty", contentType: "text/html", version: "1", wantStatus: http.StatusBadRequest},
		{name: "invalid utf8", body: []byte{0xff}, contentType: "text/html", version: "1", wantStatus: http.StatusBadRequest},
		{name: "wrong media type", body: []byte("x"), contentType: "application/json", version: "1", wantStatus: http.StatusBadRequest},
		{name: "wrong charset", body: []byte("x"), contentType: "text/html; charset=gbk", version: "1", wantStatus: http.StatusBadRequest},
		{name: "extra parameter", body: []byte("x"), contentType: "text/html; profile=live", version: "1", wantStatus: http.StatusBadRequest},
		{name: "wrong version", body: []byte("x"), contentType: "text/html", version: "2", wantStatus: http.StatusBadRequest},
		{name: "oversized", body: bytes.Repeat([]byte("x"), int(maxConversationShareBytes)+1), contentType: "text/html", version: "1", wantStatus: http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := performConversationShareRequestWithHeaders(server, http.MethodPost, conversationSharesPath, tc.body, defaultDesktopJWT, tc.contentType, tc.version)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}

	chunked := httptest.NewRequest(
		http.MethodPost,
		conversationSharesPath,
		bytes.NewReader(bytes.Repeat([]byte("x"), int(maxConversationShareBytes)+1)),
	)
	chunked.ContentLength = -1
	chunked.TransferEncoding = []string{"chunked"}
	chunked.Header.Set("Content-Type", "text/html")
	chunked.Header.Set(conversationDocumentVersionHeader, "1")
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
	if method.Code != http.StatusMethodNotAllowed || method.Header().Get("Allow") != http.MethodGet {
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
	return performConversationShareRequestWithHeaders(server, method, path, body, token, "text/html; charset=utf-8", "1")
}

func performConversationShareRequestWithExpiration(server *Server, expiration string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, conversationSharesPath, bytes.NewReader(body))
	req.Header.Set("Content-Type", "text/html; charset=utf-8")
	req.Header.Set(conversationDocumentVersionHeader, "1")
	req.Header.Set(conversationShareExpirationHeader, expiration)
	req.Header.Set(conversationShareConversationIDHeader, "chat-test")
	req.Header.Set("Authorization", "Bearer "+defaultDesktopJWT)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	return rec
}

func performConversationShareRequestWithHeaders(server *Server, method, path string, body []byte, token, contentType, version string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if body != nil {
		req.Header.Set("Content-Type", contentType)
		req.Header.Set(conversationDocumentVersionHeader, version)
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
