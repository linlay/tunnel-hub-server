package admin

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"example.invalid/tunnel-hub-server/internal/shareassets"
	"example.invalid/tunnel-hub-server/internal/store"
)

func (s *Server) SetShareTemplates(bundle *shareassets.Bundle) { s.shareTemplates = bundle }

type shareTemplateItem struct {
	shareassets.Version
	Usage *store.ShareTemplateUsage `json:"usage"`
}

func (s *Server) handleShareTemplates(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Path == "/api/admin/share-templates/upload" || regexp.MustCompile(`^/api/admin/share-templates/[a-f0-9]{64}/activate$`).MatchString(r.URL.Path) {
		s.handleShareTemplateWrite(w, r)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/activate") {
		writeError(w, 404, "not found")
		return
	}
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
	if s.shareTemplates == nil {
		writeError(w, http.StatusServiceUnavailable, "share template inventory is unavailable")
		return
	}
	versions, inventoryErr := s.shareTemplates.Versions()
	if r.URL.Path != "/api/admin/share-templates" {
		assetSet := strings.TrimPrefix(r.URL.Path, "/api/admin/share-templates/")
		if !strings.HasSuffix(assetSet, "/shares") {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		assetSet = strings.TrimSuffix(assetSet, "/shares")
		found := false
		for _, version := range versions {
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
	items := make([]shareTemplateItem, 0, len(versions))
	for _, version := range versions {
		item := shareTemplateItem{Version: version}
		if item.Error != "" {
			item.Error = "模板内容不可用，请查看服务日志或启用其他有效版本。"
		}
		if value, ok := usage[version.AssetSet]; ok {
			item.Usage = &value
		}
		items = append(items, item)
	}
	response := map[string]any{"items": items, "days": days, "startDay": startDay, "endDay": endDay, "currentAssetSet": nil, "maxUploadBytes": shareassets.MaxUploadBytes, "uploadEnabled": s.shareTemplates.UploadEnabled()}
	if current := s.shareTemplates.CurrentAssetSet(); current != "" {
		response["currentAssetSet"] = current
	}
	if inventoryErr != nil {
		response["error"] = "当前模板或存储不可用，请查看服务日志并显式启用有效版本。"
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleShareTemplateWrite(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, 405, "method not allowed")
		return
	}
	if r.Header.Get("X-Template-Action") != "1" {
		writeError(w, 403, "template action header required")
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		parsed, err := url.Parse(origin)
		scheme := "http"
		if r.TLS != nil || s.Config.CookieSecure {
			scheme = "https"
		}
		if err != nil || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Scheme != scheme || !strings.EqualFold(parsed.Host, r.Host) {
			writeError(w, 403, "same origin required")
			return
		}
	}
	if r.URL.RawQuery != "" {
		writeError(w, 400, "query parameters are not supported")
		return
	}
	if s.shareTemplates == nil {
		writeError(w, 503, "template storage unavailable")
		return
	}
	if strings.HasSuffix(r.URL.Path, "/activate") {
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/admin/share-templates/"), "/activate")
		if err := s.shareTemplates.Activate(id); err != nil {
			s.writeTemplateError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"currentAssetSet": s.shareTemplates.CurrentAssetSet()})
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/zip" {
		writeError(w, 415, "application/zip required")
		return
	}
	if r.ContentLength > shareassets.MaxUploadBytes {
		writeError(w, 413, "template package exceeds size limit")
		return
	}
	body := http.MaxBytesReader(w, r.Body, shareassets.MaxUploadBytes)
	defer body.Close()
	file, err := os.CreateTemp("", "share-template-http-*.zip")
	if err != nil {
		s.writeTemplateError(w, shareassets.ErrUnavailable)
		return
	}
	defer func() {
		_ = file.Close()
		if err := os.Remove(file.Name()); err != nil {
			s.Logger.Error("remove template upload temporary file", "error", err)
		}
	}()
	size, err := io.Copy(file, body)
	if err != nil {
		var limit *http.MaxBytesError
		if errors.As(err, &limit) {
			writeError(w, 413, "template package exceeds size limit")
		} else {
			writeError(w, 400, "incomplete template upload")
		}
		return
	}
	id, exists, err := s.shareTemplates.Upload(file, size)
	if err != nil {
		s.writeTemplateError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 250*time.Millisecond)
	defer cancel()
	if err := s.DB.StartShareTemplateObservation(ctx, []string{id}, time.Now()); err != nil {
		s.Logger.Error("observe uploaded share template", "error", err)
	}
	writeJSON(w, 200, map[string]any{"assetSet": id, "alreadyExists": exists})
}
func (s *Server) writeTemplateError(w http.ResponseWriter, err error) {
	status, message := 503, "template storage unavailable"
	switch {
	case errors.Is(err, shareassets.ErrTooLarge):
		status, message = 413, "template package exceeds size limit"
	case errors.Is(err, shareassets.ErrInvalidPackage):
		status, message = 400, "invalid template package"
	case errors.Is(err, shareassets.ErrConflict):
		status, message = 409, "stored template is unavailable or conflicts"
	case errors.Is(err, shareassets.ErrNotFound):
		status, message = 404, "template version not found"
	}
	s.Logger.Error("share template operation failed", "error", err)
	writeError(w, status, message)
}
