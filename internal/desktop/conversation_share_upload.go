package desktop

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"example.invalid/tunnel-hub-server/internal/config"
	"example.invalid/tunnel-hub-server/internal/sharefiles"
	"example.invalid/tunnel-hub-server/internal/store"
)

var resourceIDPattern = regexp.MustCompile(`^[a-f0-9]{24}$`)
var resourceHashPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var resourceMIMEPattern = regexp.MustCompile(`^[A-Za-z0-9!#$&^_.+-]+/[A-Za-z0-9!#$&^_.+-]+$`)
var errConversationShareResourceStorage = errors.New("conversation share resource storage failed")

type conversationSnapshotResource struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	MIMEType  string            `json:"mimeType"`
	Size      int64             `json:"size"`
	SHA256    string            `json:"sha256"`
	SourceRef string            `json:"sourceRef"`
	MediaRefs map[string]string `json:"mediaRefs,omitempty"`
}

type conversationSnapshotOmission struct {
	ID        string `json:"id,omitempty"`
	SourceRef string `json:"sourceRef,omitempty"`
	Name      string `json:"name"`
	Reason    string `json:"reason"`
}

func (r *conversationSnapshotResource) UnmarshalJSON(data []byte) error {
	type resource conversationSnapshotResource
	var decoded resource
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if len(fields["size"]) == 0 || bytes.Equal(bytes.TrimSpace(fields["size"]), []byte("null")) ||
		bytes.Equal(bytes.TrimSpace(fields["mediaRefs"]), []byte("null")) {
		return errors.New("invalid resource metadata")
	}
	*r = conversationSnapshotResource(decoded)
	return nil
}

// decodeConversationShare keeps only the bounded JSON snapshot in memory.
// Resource bodies are streamed into a request-scoped staging directory.
func decodeConversationShare(w http.ResponseWriter, r *http.Request, stage *sharefiles.Stage, limits config.ConversationShareLimits) (snapshot []byte, resources []store.ConversationShareResource, err error) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" || stage == nil {
		return nil, nil, errors.New("Content-Type must be multipart/form-data")
	}
	r.Body = http.MaxBytesReader(w, r.Body, limits.RequestBodyBytes())
	defer func() {
		var maximum *http.MaxBytesError
		if errors.As(err, &maximum) {
			err = &conversationShareSizeError{"request_body_too_large", limits.RequestBodyBytes()}
		}
	}()
	reader, err := r.MultipartReader()
	if err != nil {
		return nil, nil, fmt.Errorf("invalid conversation share multipart request: %w", err)
	}
	descriptors := make(map[string]conversationSnapshotResource)
	uploaded := make(map[string]store.ConversationShareResource)
	var total int64
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("invalid conversation share multipart request: %w", err)
		}
		name := part.FormName()
		if name == "snapshot" {
			if snapshot != nil || len(uploaded) != 0 {
				return nil, nil, errors.New("snapshot must be the first and only snapshot part")
			}
			snapshot, err = io.ReadAll(io.LimitReader(part, limits.MaxSnapshotBytes+1))
			if err != nil {
				return nil, nil, err
			}
			if int64(len(snapshot)) > limits.MaxSnapshotBytes {
				return nil, nil, &conversationShareSizeError{"snapshot_too_large", limits.MaxSnapshotBytes}
			}
			descriptors, err = parseConversationSnapshotResources(snapshot)
			if err != nil {
				return nil, nil, err
			}
			continue
		}
		if !strings.HasPrefix(name, "attachment:") || snapshot == nil {
			return nil, nil, errors.New("unknown conversation share part")
		}
		id := strings.TrimPrefix(name, "attachment:")
		descriptor, ok := descriptors[id]
		if !ok || uploaded[id].ID != "" || part.FileName() != descriptor.Name {
			return nil, nil, errors.New("invalid conversation resource part")
		}
		partMIME, _, mimeErr := mime.ParseMediaType(part.Header.Get("Content-Type"))
		if mimeErr != nil || !strings.EqualFold(partMIME, descriptor.MIMEType) {
			return nil, nil, errors.New("conversation resource MIME type mismatch")
		}
		remaining := limits.MaxResourceBytes - total
		source := &conversationPartReader{Reader: part}
		written, writeErr := stage.Write(id, source, remaining)
		if writeErr != nil {
			if errors.Is(writeErr, sharefiles.ErrTooLarge) {
				return nil, nil, &conversationShareSizeError{"resources_too_large", limits.MaxResourceBytes}
			}
			if source.err != nil {
				return nil, nil, source.err
			}
			return nil, nil, fmt.Errorf("%w: %v", errConversationShareResourceStorage, writeErr)
		}
		total += written.Size
		if written.Size != descriptor.Size || !strings.EqualFold(written.SHA256, descriptor.SHA256) {
			return nil, nil, errors.New("conversation resource integrity mismatch")
		}
		uploaded[id] = store.ConversationShareResource{
			ID: id, Name: descriptor.Name, MIMEType: descriptor.MIMEType,
			Size: written.Size, SHA256: strings.ToLower(written.SHA256),
		}
	}
	// Apply the same request bound to any multipart epilogue.
	if _, err := io.Copy(io.Discard, r.Body); err != nil {
		return nil, nil, err
	}
	if len(snapshot) == 0 || len(uploaded) != len(descriptors) {
		return nil, nil, errors.New("missing conversation resource")
	}
	resources = make([]store.ConversationShareResource, 0, len(descriptors))
	var envelope struct {
		Attachments []conversationSnapshotResource `json:"attachments"`
	}
	if err := json.Unmarshal(snapshot, &envelope); err != nil {
		return nil, nil, errors.New("invalid conversation snapshot")
	}
	for _, descriptor := range envelope.Attachments {
		resources = append(resources, uploaded[descriptor.ID])
	}
	return snapshot, resources, nil
}

func parseConversationSnapshotResources(snapshot []byte) (map[string]conversationSnapshotResource, error) {
	if len(snapshot) == 0 || !utf8.Valid(snapshot) || !json.Valid(snapshot) {
		return nil, errors.New("invalid conversation snapshot")
	}
	var envelope struct {
		Version          int                            `json:"version"`
		Attachments      []conversationSnapshotResource `json:"attachments"`
		OmittedResources json.RawMessage                `json:"omittedResources"`
		Turns            []struct {
			Nodes []struct {
				AttachmentIDs json.RawMessage `json:"attachmentIds"`
			} `json:"nodes"`
		} `json:"turns"`
	}
	if json.Unmarshal(snapshot, &envelope) != nil || envelope.Version != store.ConversationSnapshotVersion {
		return nil, errors.New("unsupported conversation snapshot version")
	}
	descriptors := make(map[string]conversationSnapshotResource, len(envelope.Attachments))
	sourceRefs := make(map[string]bool)
	for _, descriptor := range envelope.Attachments {
		if !resourceIDPattern.MatchString(descriptor.ID) || descriptors[descriptor.ID].ID != "" ||
			descriptor.Size < 0 || !resourceHashPattern.MatchString(descriptor.SHA256) ||
			!resourceMIMEPattern.MatchString(descriptor.MIMEType) || descriptor.MIMEType != strings.ToLower(descriptor.MIMEType) || len(descriptor.Name) > 255 ||
			descriptor.Name == "" || strings.ContainsAny(descriptor.Name, "/\\") ||
			strings.ContainsFunc(descriptor.Name, unicode.IsControl) || !canonicalChatSourceRef(descriptor.SourceRef) || sourceRefs[descriptor.SourceRef] {
			return nil, errors.New("invalid conversation resource descriptor")
		}
		descriptors[descriptor.ID] = descriptor
		sourceRefs[descriptor.SourceRef] = true
	}
	omitted := make(map[string]bool)
	var omissions []conversationSnapshotOmission
	if len(envelope.OmittedResources) != 0 && (bytes.Equal(bytes.TrimSpace(envelope.OmittedResources), []byte("null")) || json.Unmarshal(envelope.OmittedResources, &omissions) != nil) {
		return nil, errors.New("invalid omitted resources")
	}
	for _, item := range omissions {
		if item.Name == "" || len(item.Name) > 255 || strings.ContainsAny(item.Name, `/\`) || strings.ContainsFunc(item.Name, unicode.IsControl) {
			return nil, errors.New("invalid omitted resource name")
		}
		switch item.Reason {
		case "not_found", "forbidden", "unsupported", "too_large", "timeout", "changed", "invalid", "read_failed":
		default:
			return nil, errors.New("invalid omitted resource reason")
		}
		if item.ID == "" && item.SourceRef == "" {
			continue
		}
		if !resourceIDPattern.MatchString(item.ID) || !canonicalChatSourceRef(item.SourceRef) ||
			descriptors[item.ID].ID != "" || omitted[item.ID] || sourceRefs[item.SourceRef] {
			return nil, errors.New("invalid omitted resource identity")
		}
		omitted[item.ID] = true
		sourceRefs[item.SourceRef] = true
	}
	for _, descriptor := range envelope.Attachments {
		if descriptor.MediaRefs != nil && descriptor.MIMEType != "text/html" {
			return nil, errors.New("only HTML resources may declare media references")
		}
		for raw, id := range descriptor.MediaRefs {
			target, exists := descriptors[id]
			if strings.TrimSpace(raw) == "" || strings.ContainsFunc(raw, unicode.IsControl) ||
				(!exists && !omitted[id]) || (exists && !isShareMediaMIME(target.MIMEType)) {
				return nil, errors.New("invalid HTML media reference")
			}
		}
	}
	for _, turn := range envelope.Turns {
		for _, node := range turn.Nodes {
			var ids []string
			if len(node.AttachmentIDs) != 0 && (bytes.Equal(bytes.TrimSpace(node.AttachmentIDs), []byte("null")) || json.Unmarshal(node.AttachmentIDs, &ids) != nil) {
				return nil, errors.New("invalid node attachment references")
			}
			seen := make(map[string]bool)
			for _, id := range ids {
				if seen[id] || (descriptors[id].ID == "" && !omitted[id]) {
					return nil, errors.New("invalid node attachment reference")
				}
				seen[id] = true
			}
		}
	}
	return descriptors, nil
}

func canonicalChatSourceRef(value string) bool {
	segments := strings.Split(value, "/")
	if len(value) == 0 || len(value) > 2048 {
		return false
	}
	for _, segment := range segments {
		if segment == "" || strings.ContainsAny(segment, `\\?#`) || strings.ContainsFunc(segment, unicode.IsControl) {
			return false
		}
		decoded, err := url.PathUnescape(segment)
		if err != nil || !utf8.ValidString(decoded) || url.PathEscape(decoded) != segment {
			return false
		}
		for depth := 0; ; depth++ {
			if decoded == "" || decoded == "." || decoded == ".." ||
				decoded == ".tools" || decoded == ".btw" || decoded == ".explain" ||
				strings.ContainsAny(decoded, `/\\:`) || strings.ContainsFunc(decoded, unicode.IsControl) {
				return false
			}
			next, err := url.PathUnescape(decoded)
			if err != nil || next == decoded {
				break
			}
			if depth >= 4 {
				return false
			}
			decoded = next
		}
	}
	return true
}

// Distinguish upload read failures from staging disk failures.
type conversationPartReader struct {
	io.Reader
	err error
}

func (r *conversationPartReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err != nil && err != io.EOF {
		r.err = err
	}
	return n, err
}
