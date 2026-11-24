package desktop

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"example.invalid/tunnel-hub-server/internal/shareassets"
	"example.invalid/tunnel-hub-server/internal/store"
)

func TestInlineHTMLPreviewAccessAndPolicy(t *testing.T) {
	cfg := desktopTestConfig(t)
	cfg.SharePublicBaseURL = "https://share.example.test"
	cfg.BrandID, cfg.ProductName = "test", "Test"
	server, db := newDesktopTestServerWithConfig(t, cfg)
	bundle := shareassets.NewBundle()
	server.SetConversationShareRenderer(bundle)
	now := time.Now().UTC()
	server.now = func() time.Time { return now }
	ctx := context.Background()
	expires := now.Add(time.Hour)
	legacy, err := db.CreateConversationShare(ctx, "owner", "chat", 1, []byte(validConversationSnapshot), now, &expires, false)
	if err != nil {
		t.Fatal(err)
	}
	generic, err := db.CreateShareWithResources(ctx, store.ConversationShare{ID: "share_inline_html", OwnerUserID: "owner", Kind: "conversation", Title: "HTML", SnapshotVersion: 1, SnapshotJSON: []byte(validConversationSnapshot), CreatedAt: now}, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, id, query string) *httptest.ResponseRecorder {
		return performConversationShareRequest(server, method, "/share/"+id+"/html-preview"+query, nil, "")
	}
	query := "?assetSet=" + bundle.CurrentAssetSet()
	for _, id := range []string{legacy.ID, generic.ID} {
		response := request(http.MethodGet, id, query)
		if response.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", id, response.Code, response.Body.String())
		}
		csp := response.Header().Get("Content-Security-Policy")
		script := "script-src https://share.example.test/assets/conversation-export/" + bundle.CurrentAssetSet() + "/html-preview-bootstrap.js;"
		for _, part := range []string{script, "connect-src 'none'", "frame-src 'none'", "sandbox allow-scripts", "base-uri 'none'", "form-action 'none'"} {
			if !strings.Contains(csp, part) {
				t.Fatalf("missing %q: %s", part, csp)
			}
		}
		if strings.Contains(csp, "allow-same-origin") || strings.Contains(csp, "script-src 'unsafe-inline'") || strings.Contains(response.Body.String(), "发布计划") {
			t.Fatal("shell exposes content or permissions")
		}
		if response.Header().Get("Set-Cookie") != "" {
			t.Fatal("preview sets a claim cookie")
		}
		head := request(http.MethodHead, id, query)
		if head.Code != http.StatusOK || head.Body.Len() != 0 || head.Header().Get("Content-Length") != response.Header().Get("Content-Length") {
			t.Fatal("invalid HEAD response")
		}
	}
	for _, query := range []string{"", "?assetSet=dev", "?assetSet=" + bundle.CurrentAssetSet() + "&html=bad", "?assetSet=" + bundle.CurrentAssetSet() + "&assetSet=" + bundle.CurrentAssetSet()} {
		if got := request(http.MethodGet, legacy.ID, query); got.Code != http.StatusBadRequest {
			t.Fatalf("query %q: %d", query, got.Code)
		}
	}
	if got := request(http.MethodGet, legacy.ID, "?assetSet="+strings.Repeat("a", 64)); got.Code != http.StatusServiceUnavailable {
		t.Fatalf("unknown asset set: %d", got.Code)
	}
	if got := request(http.MethodPost, legacy.ID, query); got.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d", got.Code)
	}
	if err := db.RevokeShare(ctx, generic.ID, "owner", now); err != nil {
		t.Fatal(err)
	}
	if got := request(http.MethodGet, generic.ID, query); got.Code != http.StatusNotFound {
		t.Fatalf("revoked: %d", got.Code)
	}
	for _, invalid := range []struct {
		id, kind string
		version  int
	}{{"share_inline_package", "package", 1}, {"share_inline_version", "conversation", 2}} {
		_, err := db.CreateShareWithResources(ctx, store.ConversationShare{ID: invalid.id, OwnerUserID: "owner", Kind: invalid.kind, Title: "Invalid", SnapshotVersion: invalid.version, SnapshotJSON: []byte(validConversationSnapshot), CreatedAt: now}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := request(http.MethodGet, invalid.id, query); got.Code != http.StatusNotFound {
			t.Fatalf("invalid share %s: %d", invalid.id, got.Code)
		}
	}
	now = expires
	if got := request(http.MethodGet, legacy.ID, query); got.Code != http.StatusNotFound {
		t.Fatalf("expired: %d", got.Code)
	}
}

func TestInlineHTMLPreviewCannotClaimSingleUseShare(t *testing.T) {
	cfg := desktopTestConfig(t)
	cfg.SharePublicBaseURL = "https://share.example.test"
	cfg.BrandID, cfg.ProductName = "test", "Test"
	server, db := newDesktopTestServerWithConfig(t, cfg)
	bundle := shareassets.NewBundle()
	server.SetConversationShareRenderer(bundle)
	now := time.Now().UTC()
	server.now = func() time.Time { return now }
	share, err := db.CreateConversationShare(context.Background(), "owner", "chat", 1, []byte(validConversationSnapshot), now, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	path := "/share/" + share.ID + "/html-preview?assetSet=" + bundle.CurrentAssetSet()
	denied := performConversationShareRequest(server, http.MethodGet, path, nil, "")
	if denied.Code != http.StatusNotFound || denied.Header().Get("Set-Cookie") != "" {
		t.Fatal("preview must not claim the share")
	}
	page := performConversationShareRequest(server, http.MethodGet, "/share/"+share.ID, nil, "")
	if page.Code != http.StatusOK || len(page.Result().Cookies()) != 1 {
		t.Fatal("original page cannot claim")
	}
	request := func(cookie *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if cookie != nil {
			req.AddCookie(cookie)
		}
		result := httptest.NewRecorder()
		server.ServeHTTP(result, req)
		return result
	}
	if request(nil).Code != http.StatusNotFound {
		t.Fatal("missing claim accepted")
	}
	cookie := page.Result().Cookies()[0]
	if got := request(cookie); got.Code != http.StatusOK || got.Header().Get("Set-Cookie") != "" {
		t.Fatalf("existing claim: %d", got.Code)
	}
	wrongCookie := *cookie
	wrongCookie.Value = strings.Repeat("a", len(cookie.Value))
	if request(&wrongCookie).Code != http.StatusNotFound {
		t.Fatal("wrong claim accepted")
	}
	now = now.Add(store.ConversationShareSessionDuration)
	if request(cookie).Code != http.StatusNotFound {
		t.Fatal("expired claim accepted")
	}
	now = now.Add(-store.ConversationShareSessionDuration)
	if err := db.RevokeConversationShare(context.Background(), share.ID, "owner", now); err != nil {
		t.Fatal(err)
	}
	if request(cookie).Code != http.StatusNotFound {
		t.Fatal("revoked legacy share accepted")
	}
}
