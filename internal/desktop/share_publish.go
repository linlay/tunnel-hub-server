package desktop

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"unicode"

	"example.invalid/tunnel-hub-server/internal/config"
	"example.invalid/tunnel-hub-server/internal/sharecontract"
	"example.invalid/tunnel-hub-server/internal/sharefiles"
	"example.invalid/tunnel-hub-server/internal/store"
)

var errShareBehaviorUnsupported = errors.New("share behavior is not supported")

// The persisted description excludes content bytes and resource metadata. Those
// remain in the existing snapshot_json and resources table, once per share.
type shareDescription struct {
	ProtocolVersion   int                        `json:"protocolVersion"`
	Source            *sharecontract.Source      `json:"source,omitempty"`
	Preview           *sharecontract.PreviewSpec `json:"preview,omitempty"`
	Desktop           *sharecontract.Handoff     `json:"desktop,omitempty"`
	Service           *sharecontract.Binding     `json:"service,omitempty"`
	ContentResourceID string                     `json:"contentResourceId,omitempty"`
}

type preparedShare struct {
	envelope       sharecontract.Envelope
	resources      []store.ConversationShareResource
	conversationID string // Only the conversation adapter supplies this projection.
}

type shareUploadDecoder func(http.ResponseWriter, *http.Request, *sharefiles.Stage, config.ConversationShareLimits) (preparedShare, error)

// publishShare owns the single staging, commit, database and compensation path.
// The two HTTP edges only decode their wire protocol and project their response.
func (s *Server) publishShare(w http.ResponseWriter, r *http.Request, owner string, decode shareUploadDecoder) (store.ConversationShare, string, bool) {
	if r.URL.RawQuery != "" {
		writeError(w, http.StatusBadRequest, "share creation does not accept query parameters")
		return store.ConversationShare{}, "", false
	}
	if s.conversationShareResources == nil {
		writeError(w, http.StatusServiceUnavailable, "share resource storage is unavailable")
		return store.ConversationShare{}, "", false
	}
	base, err := s.conversationShareBaseURL()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return store.ConversationShare{}, "", false
	}
	id, err := store.NewConversationShareID()
	if err != nil {
		s.writeInternal(w, "create share id", err)
		return store.ConversationShare{}, "", false
	}
	stage, err := s.conversationShareResources.Begin()
	if err != nil {
		s.writeInternal(w, "stage share resources", err)
		return store.ConversationShare{}, "", false
	}
	defer stage.Abort()
	prepared, err := decode(w, r, stage, s.Config.ShareLimits)
	if err != nil {
		var size *conversationShareSizeError
		switch {
		case errors.As(err, &size):
			writeJSON(w, http.StatusRequestEntityTooLarge, size)
		case errors.Is(err, errConversationShareResourceStorage):
			s.writeInternal(w, "write share resources", err)
		case errors.Is(err, errShareBehaviorUnsupported):
			writeError(w, http.StatusUnprocessableEntity, err.Error())
		default:
			writeError(w, http.StatusBadRequest, err.Error())
		}
		return store.ConversationShare{}, "", false
	}
	e := prepared.envelope
	if e.Service != nil {
		binding, err := s.DB.GetActiveDesktopWebAppRouteByID(r.Context(), e.Service.WebAppID)
		if errors.Is(err, store.ErrNotFound) || (err == nil && binding.Device.OwnerUserID != owner) {
			writeError(w, http.StatusUnprocessableEntity, "share requires an existing owned webapp publication")
			return store.ConversationShare{}, "", false
		}
		if err != nil {
			s.writeInternal(w, "validate share service binding", err)
			return store.ConversationShare{}, "", false
		}
	}
	description, err := json.Marshal(shareDescription{ProtocolVersion: e.ProtocolVersion, Source: e.Source, Preview: e.Preview, Desktop: e.Desktop, Service: e.Service, ContentResourceID: e.Content.ResourceID})
	if err != nil {
		s.writeInternal(w, "encode share description", err)
		return store.ConversationShare{}, "", false
	}
	content := []byte(e.Content.Data)
	if len(content) == 0 {
		content = []byte(`{}`)
	}
	if err := stage.Commit(id); err != nil {
		s.writeInternal(w, "commit share resources", err)
		return store.ConversationShare{}, "", false
	}
	share, err := s.DB.CreateShareWithResources(r.Context(), store.ConversationShare{ID: id, OwnerUserID: owner,
		ConversationID: prepared.conversationID, Kind: e.Kind, Title: e.Title, DescriptionJSON: description,
		SnapshotVersion: e.Content.Version, SnapshotJSON: content, CreatedAt: s.now().UTC()}, prepared.resources)
	if err != nil {
		if cleanupErr := s.conversationShareResources.Delete(id); cleanupErr != nil {
			s.Logger.Error("remove failed share resources", "shareId", id, "error", cleanupErr)
		}
		s.writeInternal(w, "create share", err)
		return store.ConversationShare{}, "", false
	}
	return share, base, true
}

func decodeLegacyShare(conversationID string) shareUploadDecoder {
	return func(w http.ResponseWriter, r *http.Request, stage *sharefiles.Stage, limits config.ConversationShareLimits) (preparedShare, error) {
		snapshot, resources, err := decodeConversationShare(w, r, stage, limits)
		if err != nil {
			return preparedShare{}, err
		}
		var metadata struct {
			Title string `json:"title"`
		}
		_ = json.Unmarshal(snapshot, &metadata)
		title := strings.TrimSpace(metadata.Title)
		if title == "" || len(title) > 255 || strings.ContainsFunc(title, unicode.IsControl) {
			title = "历史对话分享"
		}
		e := sharecontract.Envelope{ProtocolVersion: sharecontract.ProtocolVersion, Kind: "conversation", Title: title,
			Content: sharecontract.Content{Version: 1, Data: snapshot}, Resources: make([]sharecontract.Resource, 0, len(resources)),
			Preview: &sharecontract.PreviewSpec{Type: "builtin", Viewer: "conversation"}}
		for _, resource := range resources {
			e.Resources = append(e.Resources, sharecontract.Resource{ID: resource.ID, Name: resource.Name, MIMEType: resource.MIMEType, Size: resource.Size, SHA256: resource.SHA256})
		}
		if !strings.ContainsAny(conversationID, `/\:?#`) && conversationID == strings.TrimSpace(conversationID) {
			e.Source = &sharecontract.Source{Module: "conversation", ID: conversationID}
		}
		if err := e.Validate(); err != nil {
			return preparedShare{}, err
		}
		return preparedShare{envelope: e, resources: resources, conversationID: conversationID}, nil
	}
}

func decodeGenericShare(w http.ResponseWriter, r *http.Request, stage *sharefiles.Stage, limits config.ConversationShareLimits) (preparedShare, error) {
	var envelope sharecontract.Envelope
	_, resources, err := decodeShareUpload(w, r, stage, limits, sharecontract.ManifestPart, sharecontract.ResourcePart, func(data []byte) ([]sharecontract.Resource, error) {
		var err error
		envelope, err = sharecontract.Decode(data)
		if err != nil {
			return nil, err
		}
		if envelope.Preview != nil && envelope.Preview.Type == "builtin" {
			if err := validateConversationEnvelope(envelope); err != nil {
				return nil, err
			}
		}
		return envelope.Resources, nil
	})
	if err != nil {
		return preparedShare{}, err
	}
	conversationID := ""
	if envelope.Kind == "conversation" && envelope.Source != nil && envelope.Source.Module == "conversation" {
		conversationID = envelope.Source.ID
	}
	return preparedShare{envelope: envelope, resources: resources, conversationID: conversationID}, nil
}

func validateConversationEnvelope(e sharecontract.Envelope) error {
	if e.Kind != "conversation" || e.Content.Version != 1 || e.Content.ResourceID != "" {
		return errors.New("invalid conversation viewer content")
	}
	descriptors, err := parseConversationSnapshotResources(e.Content.Data)
	if err != nil {
		return err
	}
	if len(descriptors) != len(e.Resources) {
		return errors.New("conversation resource manifest mismatch")
	}
	for _, resource := range e.Resources {
		item := descriptors[resource.ID]
		if item.ID == "" || item.Name != resource.Name || item.MIMEType != resource.MIMEType || item.Size != resource.Size || item.SHA256 != resource.SHA256 {
			return errors.New("conversation resource manifest mismatch")
		}
	}
	return nil
}
