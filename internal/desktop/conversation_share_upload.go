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

	"example.invalid/tunnel-hub-server/internal/config"
	"example.invalid/tunnel-hub-server/internal/sharecontract"
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

// The old decoder owns sourceRef/Snapshot validation; the streaming uploader
// underneath is shared with v2. Neither route buffers resource bodies in memory.
func decodeConversationShare(w http.ResponseWriter, r *http.Request, stage *sharefiles.Stage, limits config.ConversationShareLimits) ([]byte, []store.ConversationShareResource, error) {
	return decodeShareUpload(w, r, stage, limits, "snapshot", "attachment:", func(data []byte) ([]sharecontract.Resource, error) {
		descriptors, err := parseConversationSnapshotResources(data)
		if err != nil {
			return nil, err
		}
		var snapshot struct {
			Attachments []conversationSnapshotResource `json:"attachments"`
		}
		if err := json.Unmarshal(data, &snapshot); err != nil {
			return nil, errors.New("invalid conversation snapshot")
		}
		ordered := make([]sharecontract.Resource, 0, len(descriptors))
		for _, item := range snapshot.Attachments {
			ordered = append(ordered, sharecontract.Resource{ID: item.ID, Name: item.Name, MIMEType: item.MIMEType, Size: item.Size, SHA256: item.SHA256})
		}
		return ordered, nil
	})
}

func decodeShareUpload(w http.ResponseWriter, r *http.Request, stage *sharefiles.Stage, limits config.ConversationShareLimits,
	manifestName, resourcePrefix string, decodeResources func([]byte) ([]sharecontract.Resource, error)) (manifest []byte, resources []store.ConversationShareResource, err error) {
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
		return nil, nil, errors.New("invalid share multipart request")
	}
	var ordered []sharecontract.Resource
	descriptors := make(map[string]sharecontract.Resource)
	uploaded := make(map[string]store.ConversationShareResource)
	var total int64
	for {
		part, readErr := reader.NextPart()
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, nil, fmt.Errorf("invalid share multipart request: %w", readErr)
		}
		name := part.FormName()
		if name == manifestName {
			if manifest != nil || len(uploaded) != 0 {
				return nil, nil, errors.New("manifest must be the first and only manifest part")
			}
			if manifestName == sharecontract.ManifestPart {
				contentType, _, mimeErr := mime.ParseMediaType(part.Header.Get("Content-Type"))
				if mimeErr != nil || contentType != "application/json" {
					return nil, nil, errors.New("share manifest must be application/json")
				}
			}
			manifest, err = io.ReadAll(io.LimitReader(part, limits.MaxSnapshotBytes+1))
			if err != nil {
				return nil, nil, err
			}
			if int64(len(manifest)) > limits.MaxSnapshotBytes {
				return nil, nil, &conversationShareSizeError{"snapshot_too_large", limits.MaxSnapshotBytes}
			}
			ordered, err = decodeResources(manifest)
			if err != nil {
				return nil, nil, err
			}
			for _, item := range ordered {
				if descriptors[item.ID].ID != "" {
					return nil, nil, errors.New("duplicate share resource")
				}
				descriptors[item.ID] = item
			}
			continue
		}
		if !strings.HasPrefix(name, resourcePrefix) || manifest == nil {
			return nil, nil, errors.New("unknown share part")
		}
		id := strings.TrimPrefix(name, resourcePrefix)
		descriptor, ok := descriptors[id]
		if !ok || uploaded[id].ID != "" || part.FileName() != descriptor.Name {
			return nil, nil, errors.New("invalid share resource part")
		}
		partMIME, _, mimeErr := mime.ParseMediaType(part.Header.Get("Content-Type"))
		if mimeErr != nil || !strings.EqualFold(partMIME, descriptor.MIMEType) {
			return nil, nil, errors.New("share resource MIME type mismatch")
		}
		source := &conversationPartReader{Reader: part}
		written, writeErr := stage.Write(id, source, limits.MaxResourceBytes-total)
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
		if written.Size != descriptor.Size || written.SHA256 != descriptor.SHA256 {
			return nil, nil, errors.New("share resource integrity mismatch")
		}
		uploaded[id] = store.ConversationShareResource{ID: id, Name: descriptor.Name, MIMEType: descriptor.MIMEType, Size: written.Size, SHA256: written.SHA256, Path: descriptor.Path}
	}
	if _, err := io.Copy(io.Discard, r.Body); err != nil {
		return nil, nil, err
	}
	if len(manifest) == 0 || len(uploaded) != len(descriptors) {
		return nil, nil, errors.New("missing share resource")
	}
	resources = make([]store.ConversationShareResource, 0, len(ordered))
	for _, item := range ordered {
		resources = append(resources, uploaded[item.ID])
	}
	return manifest, resources, nil
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
