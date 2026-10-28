package desktop

import (
	"encoding/json"
	"errors"
	"html"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"example.invalid/tunnel-hub-server/internal/sharecontract"
	"example.invalid/tunnel-hub-server/internal/store"
	"example.invalid/tunnel-hub-server/internal/tunnel"
)

func (s *Server) handleShares(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete && r.URL.Path != sharecontract.ManagementPath+"/limits" && strings.HasPrefix(r.URL.Path, sharecontract.ManagementPath+"/") {
		s.revokeShare(w, r, sharecontract.ManagementPath+"/")
		return
	}
	principal, ok := s.authorizeRegistration(w, r)
	if !ok {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.RawQuery != "" {
		writeError(w, http.StatusBadRequest, "share management does not accept query parameters")
		return
	}
	switch r.URL.Path {
	case sharecontract.ManagementPath + "/limits":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		writeJSON(w, http.StatusOK, sharecontract.Limits{MaxManifestBytes: s.Config.ShareLimits.MaxSnapshotBytes, MaxResourceBytes: s.Config.ShareLimits.MaxResourceBytes})
	case sharecontract.ManagementPath:
		switch r.Method {
		case http.MethodPost:
			for _, header := range []string{conversationShareExpirationHeader, conversationSnapshotVersionHeader, conversationShareConversationIDHeader} {
				if r.Header.Get(header) != "" {
					writeError(w, http.StatusBadRequest, "legacy share headers are not accepted by v2")
					return
				}
			}
			share, base, ok := s.publishShare(w, r, principal.UserID, decodeGenericShare)
			if ok {
				writeJSON(w, http.StatusCreated, shareRecord(share, base))
			}
		case http.MethodGet:
			base, err := s.conversationShareBaseURL()
			if err != nil {
				writeError(w, http.StatusServiceUnavailable, err.Error())
				return
			}
			shares, err := s.DB.ListShares(r.Context(), principal.UserID)
			if err != nil {
				s.writeInternal(w, "list shares", err)
				return
			}
			items := make([]sharecontract.Record, 0, len(shares))
			for _, share := range shares {
				items = append(items, shareRecord(share, base))
			}
			writeJSON(w, http.StatusOK, sharecontract.List{Items: items})
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	default:
		writeError(w, http.StatusNotFound, "share not found")
	}
}

func shareRecord(share store.ConversationShare, base string) sharecontract.Record {
	record := sharecontract.Record{ID: share.ID, Kind: share.Kind, Title: share.Title, URL: strings.TrimSuffix(base, "/") + "/" + url.PathEscape(share.ID),
		CreatedAt: share.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"), LastAccessedAt: formatConversationShareTime(share.LastAccessedAt)}
	if len(share.DescriptionJSON) > 0 {
		var description shareDescription
		if json.Unmarshal(share.DescriptionJSON, &description) == nil {
			record.Source = description.Source
		}
	}
	if record.Source == nil && share.Kind == "conversation" && share.ConversationID != "" && !strings.ContainsAny(share.ConversationID, `/\:?#`) {
		record.Source = &sharecontract.Source{Module: "conversation", ID: share.ConversationID}
	}
	return record
}

func descriptionForShare(share store.ConversationShare) (shareDescription, error) {
	if len(share.DescriptionJSON) == 0 {
		description := shareDescription{ProtocolVersion: sharecontract.ProtocolVersion}
		if share.Kind == "conversation" {
			description.Preview = &sharecontract.PreviewSpec{Type: "builtin", Viewer: "conversation"}
		}
		return description, nil
	}
	var description shareDescription
	if err := json.Unmarshal(share.DescriptionJSON, &description); err != nil {
		return description, err
	}
	if description.ProtocolVersion != sharecontract.ProtocolVersion {
		return description, errShareBehaviorUnsupported
	}
	return description, nil
}

func (s *Server) handlePublicShare(w http.ResponseWriter, r *http.Request) {
	id, ok := conversationShareIDFromPath(r.URL.Path, sharecontract.PublicPath)
	if !ok {
		writePublicConversationShareError(w, http.StatusNotFound)
		return
	}
	// The legacy attachment locator is a viewer-only display hint. It cannot
	// select a new action or participate in generic resolution.
	query := r.URL.RawQuery
	values, err := url.ParseQuery(query)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid share query")
		return
	}
	legacyAttachment := false
	if entries, exists := values["attachment"]; exists && len(entries) == 1 && resourceIDPattern.MatchString(entries[0]) && values.Get("action") == "" {
		legacyAttachment = true
		values.Del("attachment")
		query = values.Encode()
	}
	selection, err := sharecontract.ParseQuery(query)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	share, err := s.DB.DescribePublicShare(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writePublicConversationShareError(w, http.StatusNotFound)
		} else {
			s.Logger.Error("describe public share", "error", err)
			if selection.Action == sharecontract.Resolve {
				writeError(w, http.StatusServiceUnavailable, "share storage is unavailable")
			} else {
				writePublicConversationShareError(w, http.StatusInternalServerError)
			}
		}
		return
	}
	if legacyAttachment && share.Kind != "conversation" {
		writeError(w, http.StatusBadRequest, "attachment locator requires conversation viewer")
		return
	}
	description, err := descriptionForShare(share)
	if err != nil {
		if errors.Is(err, errShareBehaviorUnsupported) {
			writeError(w, http.StatusUnprocessableEntity, "share protocol is not supported")
		} else {
			writePublicConversationShareError(w, http.StatusInternalServerError)
		}
		return
	}
	resources, err := s.DB.ListPublicShareResources(r.Context(), id)
	if err != nil {
		s.writeInternal(w, "read share resource manifest", err)
		return
	}
	actions := []sharecontract.Action{}
	if description.Preview == nil || description.Preview.Type == "builtin" || description.Preview.Type == "package" {
		actions = append(actions, sharecontract.Preview)
	}
	if description.Desktop != nil {
		actions = append(actions, sharecontract.Desktop)
	}
	if len(resources) > 0 {
		actions = append(actions, sharecontract.Download)
	}
	if description.Service != nil {
		actions = append(actions, sharecontract.Service)
	}
	setPublicConversationShareHeaders(w.Header())
	if selection.Action == sharecontract.Resolve {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, http.StatusOK, sharecontract.Resolution{ProtocolVersion: sharecontract.ProtocolVersion, ID: id, Kind: share.Kind, Title: share.Title, Actions: actions, Desktop: description.Desktop})
		s.recordShareAccess(r, share.ID)
		return
	}
	supported := false
	for _, action := range actions {
		if action == selection.Action {
			supported = true
		}
	}
	if !supported {
		writeError(w, http.StatusUnprocessableEntity, errShareBehaviorUnsupported.Error())
		return
	}
	desktopURL := ""
	if description.Desktop != nil {
		desktopURL = s.Config.BrandID + "://share/" + url.PathEscape(id) + "?action=desktop"
	}
	switch selection.Action {
	case sharecontract.Preview:
		if description.Preview != nil && description.Preview.Type == "package" {
			previewURL := strings.TrimSuffix(s.Config.SharePublicBaseURL, "/") + sharecontract.PublicPath + id + "/preview/" + description.Preview.Entrypoint
			s.writeShareMetadataPage(w, r, share, resources, desktopURL, previewURL)
			return
		}
		if description.Preview != nil {
			s.handleGetPublicConversationSharePage(w, r)
			return
		}
		s.writeShareMetadataPage(w, r, share, resources, desktopURL, "")
	case sharecontract.Desktop:
		link := s.Config.BrandID + "://share/" + url.PathEscape(id) + "?action=desktop"
		s.writeShareMetadataPage(w, r, share, resources, link, "")
	case sharecontract.Download:
		s.serveShareResource(w, r, id, selection.ResourceID, false)
	case sharecontract.Service:
		binding, err := s.DB.GetActiveDesktopWebAppRouteByID(r.Context(), description.Service.WebAppID)
		if errors.Is(err, store.ErrNotFound) || (err == nil && binding.Device.OwnerUserID != share.OwnerUserID) {
			body := `<!doctype html><html><head><meta charset="utf-8"><title>在线服务不可用</title></head><body><h1>在线服务不可用</h1><p>原应用已停止发布或发布绑定已移除，分享时保存的介绍仍可查看。</p><a href="?action=preview">查看分享介绍</a></body></html>`
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Content-Security-Policy", "default-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(http.StatusNotFound)
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte(body))
			}
			return
		}
		if err != nil {
			s.writeInternal(w, "read share service binding", err)
			return
		}
		host := tunnel.NormalizeHost(binding.WebApp.PublicHost)
		if _, ok := tunnel.ParseWebAppPublicHost(host, s.webAppPublicBaseDomain()); !ok || host != binding.WebApp.PublicHost {
			s.writeInternal(w, "validate share service host", errors.New("invalid registered webapp host"))
			return
		}
		// The original public WebApp route owns offline, HTTP/WebSocket and visitor
		// authorization semantics. Never proxy through the share or forward SSO.
		if r.Method == http.MethodGet {
			s.recordShareAccess(r, share.ID)
		}
		http.Redirect(w, r, "https://"+host+"/", http.StatusSeeOther)
	}
}

func (s *Server) recordShareAccess(r *http.Request, id string) {
	record := s.recordConversationShareAccess
	if record == nil {
		record = s.DB.RecordConversationShareAccess
	}
	if err := record(r.Context(), id, s.now().UTC()); err != nil {
		s.Logger.Error("record share access", "shareId", id, "error", err)
	}
}

// Trusted outer page: never handles package messages or exposes a host bridge.
func (s *Server) writeShareMetadataPage(w http.ResponseWriter, r *http.Request, share store.ConversationShare, resources []store.ConversationShareResource, desktopURL, previewURL string) {
	var body strings.Builder
	body.WriteString(`<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>` + html.EscapeString(share.Title) + `</title></head><body><h1>` + html.EscapeString(share.Title) + `</h1>`)
	if desktopURL != "" {
		body.WriteString(`<p><a href="` + html.EscapeString(desktopURL) + `">` + html.EscapeString(s.Config.ProductName) + `</a></p>`)
	}
	if description, err := descriptionForShare(share); err == nil && description.Service != nil {
		body.WriteString(`<p><a href="?action=service">打开在线服务</a></p>`)
	}
	if previewURL != "" {
		body.WriteString(`<iframe title="Share preview" sandbox="allow-scripts" credentialless referrerpolicy="no-referrer" src="` + html.EscapeString(previewURL) + `" style="width:100%;height:75vh;border:0"></iframe>`)
	}
	for _, resource := range resources {
		body.WriteString(`<p><a href="?action=download&amp;resourceId=` + resource.ID + `">` + html.EscapeString(resource.Name) + `</a></p>`)
	}
	body.WriteString(`</body></html>`)
	setPublicConversationShareHeaders(w.Header())
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	policy := "default-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
	if previewURL != "" {
		policy += "; style-src 'unsafe-inline'; frame-src " + strings.TrimSuffix(s.Config.SharePublicBaseURL, "/") + sharecontract.PublicPath + share.ID + "/preview/"
	}
	w.Header().Set("Content-Security-Policy", policy)
	w.Header().Set("Content-Length", strconv.Itoa(body.Len()))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write([]byte(body.String()))
		s.recordShareAccess(r, share.ID)
	}
}
