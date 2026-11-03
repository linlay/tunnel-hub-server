package desktop

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	stdhtml "html"
	"io"
	"mime"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"

	"example.invalid/tunnel-hub-server/internal/store"
	"golang.org/x/net/html"
)

var safeDataImage = regexp.MustCompile(`^data:image/(?:png|jpeg|gif|webp);base64,[A-Za-z0-9+/=]+$`)
var unsafeCSS = regexp.MustCompile(`(?i)url\s*\(|@import|expression\s*\(|-moz-binding`)
var safeHTMLTags = map[string]bool{
	"div": true, "span": true, "p": true, "br": true, "hr": true, "h1": true,
	"h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"strong": true, "em": true, "b": true, "i": true, "u": true, "s": true,
	"blockquote": true, "pre": true, "code": true, "ul": true, "ol": true,
	"li": true, "table": true, "thead": true, "tbody": true, "tfoot": true,
	"tr": true, "th": true, "td": true, "img": true, "a": true, "section": true,
	"article": true, "header": true, "footer": true, "main": true, "figure": true,
	"figcaption": true, "small": true, "sup": true, "sub": true, "details": true,
	"summary": true, "style": true,
}
var droppedHTMLTags = map[string]bool{
	"script": true, "iframe": true, "frame": true, "frameset": true,
	"form": true, "button": true, "textarea": true, "select": true,
	"object": true, "video": true, "audio": true, "svg": true,
	"math": true, "title": true,
}

func sanitizeSharedHTML(source []byte) []byte {
	tokenizer := html.NewTokenizer(bytes.NewReader(source))
	var out strings.Builder
	out.WriteString(`<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"></head><body>`)
	skipped := 0
	style := false
	for {
		tokenType := tokenizer.Next()
		if tokenType == html.ErrorToken {
			break
		}
		token := tokenizer.Token()
		name := strings.ToLower(token.Data)
		if skipped > 0 {
			if tokenType == html.StartTagToken && droppedHTMLTags[name] {
				skipped++
			}
			if tokenType == html.EndTagToken && droppedHTMLTags[name] {
				skipped--
			}
			continue
		}
		if tokenType == html.StartTagToken && droppedHTMLTags[name] {
			skipped++
			continue
		}
		if tokenType == html.TextToken {
			if style {
				if !unsafeCSS.MatchString(token.Data) {
					out.WriteString(token.Data)
				}
			} else {
				out.WriteString(stdhtml.EscapeString(token.Data))
			}
			continue
		}
		if !safeHTMLTags[name] {
			continue
		}
		if tokenType == html.EndTagToken {
			if name == "style" {
				style = false
			}
			out.WriteString("</" + name + ">")
			continue
		}
		if tokenType != html.StartTagToken && tokenType != html.SelfClosingTagToken {
			continue
		}
		if name == "style" {
			style = true
		}
		out.WriteString("<" + name)
		for _, attribute := range token.Attr {
			key := strings.ToLower(attribute.Key)
			value := attribute.Val
			allowed := key == "class" || key == "id" || key == "title" || key == "alt" ||
				key == "width" || key == "height" || key == "colspan" || key == "rowspan"
			if key == "style" && !unsafeCSS.MatchString(value) {
				allowed = true
			}
			if key == "src" && name == "img" && safeDataImage.MatchString(value) {
				allowed = true
			}
			if key == "href" && name == "a" && (strings.HasPrefix(value, "https://") || strings.HasPrefix(value, "http://")) {
				allowed = true
			}
			if allowed {
				out.WriteString(" " + key + `="` + stdhtml.EscapeString(value) + `"`)
			}
		}
		if name == "a" {
			out.WriteString(` rel="noopener noreferrer"`)
		}
		out.WriteString(">")
	}
	out.WriteString("</body></html>")
	return []byte(out.String())
}

func writePublicConversationShareAttachmentError(w http.ResponseWriter, status int) {
	writePublicConversationShareErrorWithFrame(w, status, "'self'")
}

func (s *Server) handleGetPublicConversationShareAttachment(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" && r.URL.RawQuery != "embed=1" {
		writeError(w, http.StatusBadRequest, "attachment path does not accept query parameters")
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
	peek, err := s.DB.PeekPublicConversationShare(r.Context(), shareID)
	if err != nil || peek.Kind != "conversation" || peek.SnapshotVersion != store.ConversationSnapshotVersion {
		writePublicConversationShareAttachmentError(w, http.StatusNotFound)
		return
	}
	s.serveShareResource(w, r, shareID, segments[2], segments[3] == "preview")
}

func (s *Server) serveShareResource(w http.ResponseWriter, r *http.Request, shareID, resourceID string, preview bool) {
	resource, err := s.DB.ReadPublicConversationShareResource(r.Context(), shareID, resourceID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.Logger.Error("read conversation share attachment", "error", err)
		}
		writePublicConversationShareAttachmentError(w, http.StatusNotFound)
		return
	}
	if s.conversationShareResources == nil || (preview && !previewableShareMIME(resource.MIMEType)) {
		writePublicConversationShareAttachmentError(w, http.StatusNotFound)
		return
	}
	file, err := s.openVerifiedShareResource(shareID, resource)
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
	http.ServeContent(w, r, resource.Name, info.ModTime(), file)
}

// Use the same verified file handle for preview/download; do not re-open a
// potentially different file after checking its hash.
func (s *Server) openVerifiedShareResource(shareID string, resource store.ConversationShareResource) (*os.File, error) {
	if s.conversationShareResources == nil {
		return nil, os.ErrNotExist
	}
	file, err := s.conversationShareResources.Open(shareID, resource.ID)
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
