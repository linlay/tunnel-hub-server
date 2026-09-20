package desktop

import (
	"bytes"
	"errors"
	stdhtml "html"
	"mime"
	"net/http"
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
	"math": true,
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

func (s *Server) handleGetPublicConversationShareAttachment(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, publicConversationSharePagePath)
	segments := strings.Split(path, "/")
	if len(segments) != 4 || segments[1] != "attachments" ||
		!attachmentIDPattern.MatchString(segments[2]) ||
		(segments[3] != "preview" && segments[3] != "download") {
		writePublicConversationShareError(w, http.StatusNotFound)
		return
	}
	shareID, ok := conversationShareIDFromPath(publicConversationSharePagePath+segments[0], publicConversationSharePagePath)
	if !ok {
		writePublicConversationShareError(w, http.StatusNotFound)
		return
	}
	attachment, err := s.DB.ReadPublicConversationShareAttachment(r.Context(), shareID, segments[2], s.now().UTC(), conversationShareSessionHash(r, shareID))
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.Logger.Error("read conversation share attachment", "error", err)
		}
		writePublicConversationShareError(w, http.StatusNotFound)
		return
	}
	header := w.Header()
	setPublicConversationShareHeaders(header)
	header.Set("Content-Type", "text/html; charset=utf-8")
	if segments[3] == "preview" {
		header.Set("Content-Security-Policy", "default-src 'none'; img-src data:; style-src 'unsafe-inline'; font-src 'none'; script-src 'none'; form-action 'none'; base-uri 'none'; frame-ancestors 'self'; sandbox")
		header.Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": attachment.Name}))
		body := sanitizeSharedHTML(attachment.Body)
		header.Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(body)
		}
		return
	}
	header.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	header.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": attachment.Name}))
	header.Set("Content-Length", strconv.Itoa(len(attachment.Body)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(attachment.Body)
	}
}
