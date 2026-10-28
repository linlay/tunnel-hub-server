package desktop

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"strconv"
	"strings"

	"example.invalid/tunnel-hub-server/internal/sharecontract"
	"example.invalid/tunnel-hub-server/internal/store"
)

// Explicit package resources only. Ordinary HTML attachments retain their
// sanitizer and no-script policy in the conversation adapter.
func (s *Server) handleSharePreviewResource(w http.ResponseWriter, r *http.Request) {
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, sharecontract.PublicPath), "/", 3)
	if len(parts) != 3 || parts[1] != "preview" || !sharecontract.ValidShareID(parts[0]) || !sharecontract.ValidResourcePath(parts[2]) || r.URL.EscapedPath() != r.URL.Path {
		writePublicConversationShareAttachmentError(w, http.StatusNotFound)
		return
	}
	if r.URL.RawQuery != "" {
		writeError(w, http.StatusBadRequest, "preview resources do not accept query parameters")
		return
	}
	share, err := s.DB.DescribePublicShare(r.Context(), parts[0])
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writePublicConversationShareAttachmentError(w, http.StatusNotFound)
		} else {
			writePublicConversationShareAttachmentError(w, http.StatusInternalServerError)
		}
		return
	}
	description, err := descriptionForShare(share)
	if err != nil || description.Preview == nil || description.Preview.Type != "package" {
		writePublicConversationShareAttachmentError(w, http.StatusNotFound)
		return
	}
	resource, err := s.DB.ReadPublicShareResourceByPath(r.Context(), share.ID, parts[2])
	if err != nil {
		writePublicConversationShareAttachmentError(w, http.StatusNotFound)
		return
	}
	// The trusted outer frame-src also restricts self-navigation. Require modern
	// browser fetch metadata: any executable document (including SVG/XHTML) must
	// remain in an iframe; direct document/object/worker loads cannot bypass it.
	switch r.Header.Get("Sec-Fetch-Dest") {
	case "iframe", "script", "style", "image", "font", "empty":
	default:
		writePublicConversationShareAttachmentError(w, http.StatusForbidden)
		return
	}
	file, err := s.openVerifiedShareResource(share.ID, resource)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, os.ErrNotExist) {
			status = http.StatusNotFound
		}
		writePublicConversationShareAttachmentError(w, status)
		return
	}
	defer file.Close()
	root := strings.TrimSuffix(s.Config.SharePublicBaseURL, "/") + sharecontract.PublicPath + share.ID + "/preview/"
	policy := "sandbox allow-scripts; default-src 'none'; script-src " + root + " 'unsafe-inline'; style-src " + root + " 'unsafe-inline'; img-src " + root + " data:; font-src " + root + "; connect-src " + root + "; frame-src 'none'; worker-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors " + strings.TrimSuffix(s.Config.SharePublicBaseURL, "/")
	header := w.Header()
	setPublicConversationShareHeaders(header)
	header.Set("Content-Security-Policy", policy)
	header.Set("Content-Type", resource.MIMEType)
	header.Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": resource.Name}))
	header.Set("Access-Control-Allow-Origin", "*") // Opaque iframe modules/fonts; no credentials are exposed.
	header.Set("Cross-Origin-Resource-Policy", "cross-origin")
	header.Set("Content-Length", strconv.FormatInt(resource.Size, 10))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = io.CopyN(w, file, resource.Size)
		s.recordShareAccess(r, share.ID)
	}
}
