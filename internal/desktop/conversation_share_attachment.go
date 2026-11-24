package desktop

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	"example.invalid/tunnel-hub-server/internal/shareassets"
	"example.invalid/tunnel-hub-server/internal/sharefiles"
	"example.invalid/tunnel-hub-server/internal/store"
)

func writePublicConversationShareAttachmentError(w http.ResponseWriter, status int) {
	writePublicConversationShareErrorWithFrame(w, status, "'self'")
}

func (s *Server) handleGetPublicConversationShareAttachment(w http.ResponseWriter, r *http.Request) {
	query, queryErr := url.ParseQuery(r.URL.RawQuery)
	if queryErr != nil || len(query) > 2 ||
		(len(query) > 0 && (len(query["embed"]) != 1 || query.Get("embed") != "1")) ||
		(len(query) == 2 && (len(query["assetSet"]) != 1 || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(query.Get("assetSet")))) {
		writeError(w, http.StatusBadRequest, "invalid attachment preview parameters")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, publicConversationSharePagePath)
	segments := strings.Split(path, "/")
	if len(segments) != 4 || segments[1] != "attachments" ||
		!resourceIDPattern.MatchString(segments[2]) ||
		(segments[3] != "preview" && segments[3] != "download") {
		writePublicConversationShareAttachmentError(w, http.StatusNotFound)
		return
	}
	shareID, ok := conversationShareIDFromPath(publicConversationSharePagePath+segments[0], publicConversationSharePagePath)
	if !ok {
		writePublicConversationShareAttachmentError(w, http.StatusNotFound)
		return
	}
	s.serveShareResource(w, r, shareID, segments[2], segments[3] == "preview")
}

func (s *Server) serveShareResource(w http.ResponseWriter, r *http.Request, shareID, resourceID string, preview bool) {
	files := s.shareResources
	recordAccess := s.DB.RecordShareAccess
	generic, err := s.DB.DescribePublicShare(r.Context(), shareID)
	var resource store.ConversationShareResource
	if errors.Is(err, store.ErrNotFound) {
		resource, err = s.DB.ReadPublicConversationShareResource(r.Context(), shareID, resourceID, s.now().UTC(), conversationShareSessionHash(r, shareID))
		files = s.conversationShareResources
		recordAccess = s.DB.RecordConversationShareAccess
	} else if err == nil {
		if strings.Contains(r.URL.Path, "/attachments/") && (generic.Kind != "conversation" || generic.SnapshotVersion != store.ConversationSnapshotVersion) {
			writePublicConversationShareAttachmentError(w, http.StatusNotFound)
			return
		}
		resource, err = s.DB.ReadPublicShareResource(r.Context(), shareID, resourceID)
	}
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.Logger.Error("read conversation share attachment", "error", err)
		}
		writePublicConversationShareAttachmentError(w, http.StatusNotFound)
		return
	}
	if files == nil || (preview && !previewableShareMIME(resource.MIMEType)) {
		writePublicConversationShareAttachmentError(w, http.StatusNotFound)
		return
	}
	file, err := openVerifiedShareResource(files, shareID, resource)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, os.ErrNotExist) {
			status = http.StatusNotFound
		}
		writePublicConversationShareAttachmentError(w, status)
		return
	}
	defer file.Close()

	header := w.Header()
	setPublicConversationShareHeaders(header)
	if preview && resource.MIMEType == "text/html" {
		if r.URL.Query().Get("embed") != "1" {
			http.Redirect(w, r, publicConversationSharePagePath+shareID+"?attachment="+resource.ID, http.StatusSeeOther)
			return
		}
		if s.conversationShareRenderer == nil {
			writePublicConversationShareAttachmentError(w, http.StatusServiceUnavailable)
			return
		}
		body, err := s.conversationShareRenderer.RenderHTMLPreview(s.Config.SharePublicBaseURL, r.URL.Query().Get("assetSet"))
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, shareassets.ErrUnavailable) {
				status = http.StatusServiceUnavailable
			}
			writePublicConversationShareAttachmentError(w, status)
			return
		}
		header.Set("Content-Type", "text/html; charset=utf-8")
		header.Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline' https: "+s.Config.SharePublicBaseURL+"; style-src 'unsafe-inline' https:; img-src blob: data: https:; media-src blob: data: https:; font-src https:; connect-src 'none'; object-src 'none'; frame-src 'none'; worker-src 'none'; form-action 'none'; base-uri 'none'; frame-ancestors 'self'; sandbox allow-scripts")
		header.Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			if err := recordAccess(r.Context(), shareID, s.now().UTC()); err != nil {
				s.Logger.Error("record share access", "shareId", shareID, "error", err)
			}
			_, _ = w.Write(body)
		}
		return
	}
	header.Set("Content-Type", resource.MIMEType)
	header.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	info, err := file.Stat()
	if err != nil {
		writePublicConversationShareAttachmentError(w, http.StatusNotFound)
		return
	}
	disposition := "attachment"
	if preview {
		disposition = "inline"
	}
	header.Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": resource.Name}))
	if r.Method == http.MethodGet {
		if err := recordAccess(r.Context(), shareID, s.now().UTC()); err != nil {
			s.Logger.Error("record share access", "shareId", shareID, "error", err)
		}
	}
	http.ServeContent(w, r, resource.Name, info.ModTime(), file)
}

// Use the same verified file handle for preview/download; do not re-open a
// potentially different file after checking its hash.
func openVerifiedShareResource(files *sharefiles.Store, shareID string, resource store.ConversationShareResource) (*os.File, error) {
	if files == nil {
		return nil, os.ErrNotExist
	}
	file, err := files.Open(shareID, resource.ID)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || info.Size() != resource.Size {
		file.Close()
		return nil, errors.New("share resource size mismatch")
	}
	digest := sha256.New()
	if _, err := io.CopyN(digest, file, resource.Size); err != nil || hex.EncodeToString(digest.Sum(nil)) != resource.SHA256 {
		file.Close()
		return nil, errors.New("share resource hash mismatch")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func previewableShareMIME(value string) bool {
	return value == "text/html" || isShareMediaMIME(value)
}

func isShareMediaMIME(value string) bool {
	return strings.HasPrefix(value, "image/") ||
		strings.HasPrefix(value, "audio/") || strings.HasPrefix(value, "video/")
}
