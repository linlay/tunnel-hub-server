package admin

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"example.invalid/tunnel-hub-server/internal/shareassets"
	"example.invalid/tunnel-hub-server/internal/store"
)

func (s *Server) SetShareTemplateVersions(versions []shareassets.Version) {
	s.shareTemplateVersions = append([]shareassets.Version{}, versions...)
}

type shareTemplateItem struct {
	shareassets.Version
	Usage *store.ShareTemplateUsage `json:"usage"`
}

func (s *Server) handleShareTemplates(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	days := 30
	query, parseErr := url.ParseQuery(r.URL.RawQuery)
	if parseErr != nil {
		writeError(w, http.StatusBadRequest, "invalid query")
		return
	}
	for key, values := range query {
		if key != "days" || len(values) != 1 {
			writeError(w, http.StatusBadRequest, "only one days parameter is supported")
			return
		}
	}
	if values, ok := query["days"]; ok {
		var err error
		days, err = strconv.Atoi(values[0])
		if err != nil || (days != 7 && days != 30) {
			writeError(w, http.StatusBadRequest, "days must be 7 or 30")
			return
		}
	}
	now := time.Now().UTC()
	startDay, endDay := now.AddDate(0, 0, 1-days).Format("2006-01-02"), now.Format("2006-01-02")
	if s.shareTemplateVersions == nil {
		writeError(w, http.StatusServiceUnavailable, "share template inventory is unavailable")
		return
	}
	if r.URL.Path != "/api/admin/share-templates" {
		assetSet := strings.TrimPrefix(r.URL.Path, "/api/admin/share-templates/")
		if !strings.HasSuffix(assetSet, "/shares") {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		assetSet = strings.TrimSuffix(assetSet, "/shares")
		found := false
		for _, version := range s.shareTemplateVersions {
			found = found || version.AssetSet == assetSet
		}
		if !found {
			writeError(w, http.StatusNotFound, "template version not found")
			return
		}
		items, err := s.DB.ListShareTemplateShares(r.Context(), assetSet, startDay, endDay, 100)
		if err != nil {
			s.writeInternal(w, "list template shares", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items, "limit": 100, "startDay": startDay, "endDay": endDay})
		return
	}
	usage, err := s.DB.ShareTemplateUsage(r.Context(), startDay, endDay)
	if err != nil {
		s.writeInternal(w, "list template usage", err)
		return
	}
	items := make([]shareTemplateItem, 0, len(s.shareTemplateVersions))
	for _, version := range s.shareTemplateVersions {
		item := shareTemplateItem{Version: version}
		if value, ok := usage[version.AssetSet]; ok {
			item.Usage = &value
		}
		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "days": days, "startDay": startDay, "endDay": endDay})
}
