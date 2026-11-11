package desktop

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"example.invalid/tunnel-hub-server/internal/shareassets"
	"example.invalid/tunnel-hub-server/internal/sharecontract"
)

// Manual browser QA only: launch this compiled test with an explicit empty QA
// directory. The normal suite skips it; no production routes or config are used.
// URLs/logs go into that directory. Create `revoke` or `stop` there to exercise
// revocation or finish. All content and the identity cookie are synthetic.
func TestSharePreviewBrowserHarness(t *testing.T) {
	directory := os.Getenv("SHARE_PREVIEW_BROWSER_QA_DIR")
	if directory == "" {
		t.Skip("manual browser QA needs an explicit temporary directory")
	}
	if !filepath.IsAbs(directory) {
		t.Fatal("QA directory must be absolute")
	}
	cfg := desktopTestConfig(t)
	cfg.SharePublicBaseURL = "https://share.example.test"
	cfg.BrandID = "example"
	cfg.ProductName = "Example"
	cfg.ShareLimits.MaxSnapshotBytes = 8192
	cfg.ShareLimits.MaxResourceBytes = 1 << 20
	server, db := newDesktopTestServerWithConfig(t, cfg)
	bundle := shareassets.NewBundle()
	server.SetConversationShareRenderer(bundle)
	const cookieName = "shortlink_probe_01a18027_actual"
	var record sharecontract.Record
	var mutex sync.Mutex
	type qaRequest struct {
		Path               string `json:"path"`
		Destination        string `json:"destination"`
		ProbeCookiePresent bool   `json:"probeCookiePresent"`
	}
	var requests []qaRequest
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mutex.Lock()
		_, cookieErr := r.Cookie(cookieName)
		requests = append(requests, qaRequest{r.URL.Path, r.Header.Get("Sec-Fetch-Dest"), cookieErr == nil})
		data, _ := json.MarshalIndent(requests, "", "  ")
		_ = os.WriteFile(filepath.Join(directory, "requests.json"), data, 0600)
		mutex.Unlock()
		if r.URL.Path == "/qa/finish" {
			http.SetCookie(w, &http.Cookie{Name: cookieName, Path: "/", MaxAge: -1, SameSite: http.SameSiteLaxMode})
			w.Header().Set("Content-Security-Policy", "default-src 'none'")
			_, _ = w.Write([]byte("Probe cookie cleared"))
			return
		}
		if r.URL.Path == sharecontract.PublicPath+record.ID {
			http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "synthetic", Path: "/", SameSite: http.SameSiteLaxMode})
		}
		if strings.HasPrefix(r.URL.Path, shareassets.PublicPathPrefix) {
			bundle.ServeHTTP(w, r)
			return
		}
		server.ServeHTTP(w, r)
	}))
	defer host.Close()
	defer host.CloseClientConnections()
	server.Config.SharePublicBaseURL = host.URL
	manifest, bodies := previewPackageExample(t)
	record = decodeGenericRecord(t, postGenericShare(t, server, manifest, bodies))
	legacy := performConversationShareRequest(server, http.MethodPost, conversationSharesPath, []byte(validConversationSnapshot), defaultDesktopJWT)
	var conversation conversationShareRecordResponse
	if legacy.Code != http.StatusCreated || json.Unmarshal(legacy.Body.Bytes(), &conversation) != nil {
		t.Fatal("legacy preview setup failed")
	}
	info, _ := json.MarshalIndent(map[string]any{"pid": os.Getpid(), "executable": os.Args[0], "origin": host.URL, "packageUrl": record.URL, "conversationUrl": conversation.URL, "shareId": record.ID}, "", "  ")
	if err := os.WriteFile(filepath.Join(directory, "ready.json"), info, 0600); err != nil {
		t.Fatal(err)
	}
	fmt.Println(string(info))
	revoked := false
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(directory, "stop")); err == nil {
			return
		}
		if _, err := os.Stat(filepath.Join(directory, "revoke")); err == nil && !revoked {
			if err := db.RevokeConversationShare(context.Background(), record.ID, "42", time.Now()); err != nil {
				t.Fatal(err)
			}
			revoked = true
			if err := os.WriteFile(filepath.Join(directory, "revoked"), []byte("true"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("manual browser QA timed out; its temporary server has been closed")
}
