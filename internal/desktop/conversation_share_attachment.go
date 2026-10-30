package desktop

import (
	"errors"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"example.invalid/tunnel-hub-server/internal/store"
)

func writePublicConversationShareAttachmentError(w http.ResponseWriter, status int) {
	writePublicConversationShareErrorWithFrame(w, status, "'self'")
}

func (s *Server) handleGetPublicConversationShareAttachment(w http.ResponseWriter, r *http.Request) {
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
	resource, err := s.DB.ReadPublicConversationShareResource(r.Context(), shareID, segments[2], s.now().UTC(), conversationShareSessionHash(r, shareID))
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.Logger.Error("read conversation share attachment", "error", err)
		}
		writePublicConversationShareAttachmentError(w, http.StatusNotFound)
		return
	}
	if s.conversationShareResources == nil || (segments[3] == "preview" && !previewableShareMIME(resource.MIMEType)) {
		writePublicConversationShareAttachmentError(w, http.StatusNotFound)
		return
	}
	file, err := s.conversationShareResources.Open(shareID, resource.ID)
	if err != nil {
		writePublicConversationShareAttachmentError(w, http.StatusNotFound)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() != resource.Size {
		writePublicConversationShareAttachmentError(w, http.StatusNotFound)
		return
	}
	header := w.Header()
	setPublicConversationShareHeaders(header)
	if segments[3] == "preview" && resource.MIMEType == "text/html" {
		if r.URL.Query().Get("embed") != "1" {
			http.Redirect(w, r, publicConversationSharePagePath+shareID+"?attachment="+resource.ID, http.StatusSeeOther)
			return
		}
		if s.conversationShareRenderer == nil {
			writePublicConversationShareAttachmentError(w, http.StatusInternalServerError)
			return
		}
		body, err := s.conversationShareRenderer.RenderHTMLPreview(s.Config.SharePublicBaseURL)
		if err != nil {
			writePublicConversationShareAttachmentError(w, http.StatusInternalServerError)
			return
		}
		header.Set("Content-Type", "text/html; charset=utf-8")
		header.Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline' https: "+s.Config.SharePublicBaseURL+"; style-src 'unsafe-inline' https:; img-src blob: data: https:; media-src blob: data: https:; font-src https:; connect-src 'none'; object-src 'none'; frame-src 'none'; worker-src 'none'; form-action 'none'; base-uri 'none'; frame-ancestors 'self'; sandbox allow-scripts")
		header.Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(body)
		}
		return
	}
	header.Set("Content-Type", resource.MIMEType)
	// This also prevents active content on direct SVG navigation.
	header.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	disposition := "attachment"
	if segments[3] == "preview" {
		disposition = "inline"
	}
	header.Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": resource.Name}))
	http.ServeContent(w, r, resource.Name, info.ModTime(), file)
}

func previewableShareMIME(value string) bool {
	return value == "text/html" || isShareMediaMIME(value)
}

func isShareMediaMIME(value string) bool {
	return strings.HasPrefix(value, "image/") ||
		strings.HasPrefix(value, "audio/") || strings.HasPrefix(value, "video/")
}
