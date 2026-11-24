package desktop

import (
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"example.invalid/tunnel-hub-server/internal/shareassets"
	"example.invalid/tunnel-hub-server/internal/store"
)

var htmlPreviewAssetSet = regexp.MustCompile(`^[a-f0-9]{64}$`)

// The shell contains no user content. The parent delivers the frozen code once
// through a document-bound channel; this endpoint cannot claim a single-use share.
func (s *Server) handleConversationHTMLPreview(w http.ResponseWriter, r *http.Request) {
	id, ok := conversationShareIDFromPath(strings.TrimSuffix(r.URL.Path, "/html-preview"), publicConversationSharePagePath)
	if !ok || r.URL.EscapedPath() != r.URL.Path {
		writePublicConversationShareAttachmentError(w, http.StatusNotFound)
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(query) != 1 || len(query["assetSet"]) != 1 || !htmlPreviewAssetSet.MatchString(query.Get("assetSet")) {
		writePublicConversationShareAttachmentError(w, http.StatusBadRequest)
		return
	}
	share, err := s.DB.DescribePublicShare(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		err = s.DB.CheckPublicConversationShareAccess(r.Context(), id, s.now().UTC(), conversationShareSessionHash(r, id))
	} else if err == nil && (share.Kind != "conversation" || share.SnapshotVersion != store.ConversationSnapshotVersion) {
		err = store.ErrNotFound
	}
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrNotFound) {
			status = http.StatusNotFound
		}
		writePublicConversationShareAttachmentError(w, status)
		return
	}
	if s.conversationShareRenderer == nil {
		writePublicConversationShareAttachmentError(w, http.StatusServiceUnavailable)
		return
	}
	assetSet := query.Get("assetSet")
	body, err := s.conversationShareRenderer.RenderHTMLPreview(s.Config.SharePublicBaseURL, assetSet)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, shareassets.ErrUnavailable) {
			status = http.StatusServiceUnavailable
		}
		writePublicConversationShareAttachmentError(w, status)
		return
	}
	bootstrap := strings.TrimSuffix(s.Config.SharePublicBaseURL, "/") + shareassets.PublicPathPrefix + assetSet + "/html-preview-bootstrap.js"
	header := w.Header()
	setPublicConversationShareHeaders(header)
	header.Set("Content-Type", "text/html; charset=utf-8")
	header.Set("Content-Security-Policy", "default-src 'none'; script-src "+bootstrap+"; style-src 'unsafe-inline'; img-src data: blob:; media-src data: blob:; font-src data:; connect-src 'none'; object-src 'none'; frame-src 'none'; worker-src 'none'; form-action 'none'; base-uri 'none'; frame-ancestors 'self'; sandbox allow-scripts")
	header.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(body)
	}
}
