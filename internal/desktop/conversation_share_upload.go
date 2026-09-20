package desktop

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"example.invalid/tunnel-hub-server/internal/store"
)

var attachmentIDPattern = regexp.MustCompile(`^[a-f0-9]{24}$`)

// decodeConversationShareV2 reads a single bounded multipart request in memory.
// No upload can be committed until its snapshot and every declared file match.
func decodeConversationShareV2(w http.ResponseWriter, r *http.Request) ([]byte, []store.ConversationShareAttachment, error) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" {
		return nil, nil, errors.New("Content-Type must be multipart/form-data")
	}
	r.Body = http.MaxBytesReader(w, r.Body, store.MaxConversationSnapshotBytes+store.MaxConversationAttachmentBytes+1<<20)
	reader, err := r.MultipartReader()
	if err != nil {
		return nil, nil, errors.New("invalid conversation share multipart request")
	}
	var snapshot []byte
	files := make(map[string][]byte)
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
		limit := int64(store.MaxConversationAttachmentBytes)
		if name == "snapshot" {
			limit = store.MaxConversationSnapshotBytes
		}
		body, err := io.ReadAll(io.LimitReader(part, limit+1))
		part.Close()
		if err != nil || int64(len(body)) > limit {
			return nil, nil, errors.New("conversation share payload is too large")
		}
		if name == "snapshot" {
			if snapshot != nil {
				return nil, nil, errors.New("duplicate snapshot")
			}
			snapshot = body
		} else if strings.HasPrefix(name, "attachment:") {
			id := strings.TrimPrefix(name, "attachment:")
			if !attachmentIDPattern.MatchString(id) || files[id] != nil {
				return nil, nil, errors.New("invalid attachment id")
			}
			files[id] = body
			total += int64(len(body))
			if total > store.MaxConversationAttachmentBytes {
				return nil, nil, errors.New("conversation share attachments are too large")
			}
		} else {
			return nil, nil, errors.New("unknown conversation share part")
		}
	}
	if len(snapshot) == 0 || !utf8.Valid(snapshot) || !json.Valid(snapshot) {
		return nil, nil, errors.New("invalid conversation snapshot")
	}
	var envelope struct {
		Version     int `json:"version"`
		Attachments []struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			MIMEType  string `json:"mimeType"`
			Size      int64  `json:"size"`
			SHA256    string `json:"sha256"`
			SourceRef string `json:"sourceRef"`
		} `json:"attachments"`
	}
	if json.Unmarshal(snapshot, &envelope) != nil || envelope.Version != 2 {
		return nil, nil, errors.New("unsupported conversation snapshot version")
	}
	if len(files) != len(envelope.Attachments) {
		return nil, nil, errors.New("missing conversation attachment")
	}
	attachments := make([]store.ConversationShareAttachment, 0, len(files))
	seen := map[string]bool{}
	for _, descriptor := range envelope.Attachments {
		body, ok := files[descriptor.ID]
		if !ok || seen[descriptor.ID] || !attachmentIDPattern.MatchString(descriptor.ID) ||
			descriptor.MIMEType != "text/html" || descriptor.Size != int64(len(body)) ||
			descriptor.Size == 0 || len(descriptor.Name) > 255 || descriptor.Name == "" ||
			strings.ContainsAny(descriptor.Name, "/\\") || strings.ContainsFunc(descriptor.Name, unicode.IsControl) ||
			!strings.HasPrefix(descriptor.SourceRef, "artifacts/") {
			return nil, nil, errors.New("invalid conversation attachment")
		}
		digest := sha256.Sum256(body)
		if descriptor.SHA256 != hex.EncodeToString(digest[:]) {
			return nil, nil, errors.New("conversation attachment hash mismatch")
		}
		seen[descriptor.ID] = true
		attachments = append(attachments, store.ConversationShareAttachment{
			ID: descriptor.ID, Name: descriptor.Name, MIMEType: descriptor.MIMEType,
			Size: descriptor.Size, SHA256: descriptor.SHA256, Body: body,
		})
	}
	return snapshot, attachments, nil
}
