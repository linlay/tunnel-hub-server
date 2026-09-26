package desktop

import (
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

	"example.invalid/tunnel-hub-server/internal/sharefiles"
	"example.invalid/tunnel-hub-server/internal/store"
)

var resourceIDPattern = regexp.MustCompile(`^[a-f0-9]{24}$`)
var resourceHashPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var resourceMIMEPattern = regexp.MustCompile(`^[A-Za-z0-9!#$&^_.+-]+/[A-Za-z0-9!#$&^_.+-]+$`)
var errConversationShareResourceStorage = errors.New("conversation share resource storage failed")

type conversationSnapshotResource struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	MIMEType  string `json:"mimeType"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	SourceRef string `json:"sourceRef"`
}

// decodeConversationShare keeps only the bounded JSON snapshot in memory.
// Resource bodies are streamed into a request-scoped staging directory.
func decodeConversationShare(w http.ResponseWriter, r *http.Request, stage *sharefiles.Stage) ([]byte, []store.ConversationShareResource, error) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" || stage == nil {
		return nil, nil, errors.New("Content-Type must be multipart/form-data")
	}
	r.Body = http.MaxBytesReader(w, r.Body, store.MaxConversationSnapshotBytes+store.MaxConversationResourceBytes+(1<<20))
	reader, err := r.MultipartReader()
	if err != nil {
		return nil, nil, errors.New("invalid conversation share multipart request")
	}
	var snapshot []byte
	descriptors := make(map[string]conversationSnapshotResource)
	uploaded := make(map[string]store.ConversationShareResource)
	var total int64
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, errors.New("invalid conversation share multipart request")
		}
		name := part.FormName()
		if name == "snapshot" {
			if snapshot != nil || len(uploaded) != 0 {
				part.Close()
				return nil, nil, errors.New("snapshot must be the first and only snapshot part")
			}
			snapshot, err = io.ReadAll(io.LimitReader(part, store.MaxConversationSnapshotBytes+1))
			part.Close()
			if err != nil || len(snapshot) > store.MaxConversationSnapshotBytes {
				return nil, nil, newConversationShareSizeError(int64(len(snapshot)))
			}
			descriptors, err = parseConversationSnapshotResources(snapshot)
			if err != nil {
				return nil, nil, err
			}
			continue
		}
		if !strings.HasPrefix(name, "attachment:") || snapshot == nil {
			part.Close()
			return nil, nil, errors.New("unknown conversation share part")
		}
		id := strings.TrimPrefix(name, "attachment:")
		descriptor, ok := descriptors[id]
		if !ok || uploaded[id].ID != "" || part.FileName() != descriptor.Name {
			part.Close()
			return nil, nil, errors.New("invalid conversation resource part")
		}
		partMIME, _, mimeErr := mime.ParseMediaType(part.Header.Get("Content-Type"))
		if mimeErr != nil || !strings.EqualFold(partMIME, descriptor.MIMEType) {
			part.Close()
			return nil, nil, errors.New("conversation resource MIME type mismatch")
		}
		remaining := int64(store.MaxConversationResourceBytes) - total
		written, writeErr := stage.Write(id, part, remaining)
		part.Close()
		if writeErr != nil {
			if errors.Is(writeErr, sharefiles.ErrTooLarge) {
				return nil, nil, fmt.Errorf("conversation share resources are too large: %w", errConversationShareTooLarge)
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
	if len(snapshot) == 0 || len(uploaded) != len(descriptors) {
		return nil, nil, errors.New("missing conversation resource")
	}
	resources := make([]store.ConversationShareResource, 0, len(descriptors))
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
		Version     int                            `json:"version"`
		Attachments []conversationSnapshotResource `json:"attachments"`
	}
	if json.Unmarshal(snapshot, &envelope) != nil || envelope.Version != store.ConversationSnapshotVersion {
		return nil, errors.New("unsupported conversation snapshot version")
	}
	descriptors := make(map[string]conversationSnapshotResource, len(envelope.Attachments))
	for _, descriptor := range envelope.Attachments {
		if !resourceIDPattern.MatchString(descriptor.ID) || descriptors[descriptor.ID].ID != "" ||
			descriptor.Size < 0 || !resourceHashPattern.MatchString(descriptor.SHA256) ||
			!resourceMIMEPattern.MatchString(descriptor.MIMEType) || descriptor.MIMEType != strings.ToLower(descriptor.MIMEType) || len(descriptor.Name) > 255 ||
			descriptor.Name == "" || strings.ContainsAny(descriptor.Name, "/\\") ||
			strings.ContainsFunc(descriptor.Name, unicode.IsControl) || !canonicalArtifactSourceRef(descriptor.SourceRef) {
			return nil, errors.New("invalid conversation resource descriptor")
		}
		descriptors[descriptor.ID] = descriptor
	}
	return descriptors, nil
}

func canonicalArtifactSourceRef(value string) bool {
	segments := strings.Split(value, "/")
	if len(segments) != 3 || segments[0] != "artifacts" {
		return false
	}
	for _, segment := range segments[1:] {
		if segment == "" || strings.ContainsAny(segment, `\\?#`) || strings.ContainsFunc(segment, unicode.IsControl) {
			return false
		}
		decoded, err := url.PathUnescape(segment)
		if err != nil || url.PathEscape(decoded) != segment {
			return false
		}
		for depth := 0; depth < 4; depth++ {
			if decoded == "" || decoded == "." || decoded == ".." ||
				strings.ContainsAny(decoded, `/\\`) || strings.ContainsFunc(decoded, unicode.IsControl) {
				return false
			}
			next, err := url.PathUnescape(decoded)
			if err != nil || next == decoded {
				break
			}
			decoded = next
		}
	}
	return true
}
