package sharecontract

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

var identifierPattern = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,127}$`)
var hashPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var mimePattern = regexp.MustCompile(`^[a-z0-9!#$&^_.+-]+/[a-z0-9!#$&^_.+-]+$`)

// Decode validates only the shared envelope. The caller bounds the manifest's
// bytes first; business adapters validate Data and Context without duplicating it.
func Decode(data []byte) (Envelope, error) {
	var envelope Envelope
	if !utf8.Valid(data) {
		return envelope, errors.New("invalid share manifest encoding")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return Envelope{}, errors.New("invalid share manifest")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return Envelope{}, errors.New("invalid share manifest suffix")
	}
	if err := envelope.Validate(); err != nil {
		return Envelope{}, err
	}
	return envelope, nil
}

func (e Envelope) Validate() error {
	if e.ProtocolVersion != ProtocolVersion || !identifierPattern.MatchString(e.Kind) ||
		!text(e.Title, 255) || e.Resources == nil || !version(e.Content.Version) {
		return errors.New("invalid share envelope")
	}
	if e.Source != nil && (!identifierPattern.MatchString(e.Source.Module) ||
		!opaqueID(e.Source.ID, 255)) {
		return errors.New("invalid share source")
	}
	resources := make(map[string]Resource, len(e.Resources))
	paths := make(map[string]bool)
	var total int64
	for _, resource := range e.Resources {
		if !resourceIDPattern.MatchString(resource.ID) || resources[resource.ID].ID != "" ||
			!text(resource.Name, 255) || strings.ContainsAny(resource.Name, `/\`) ||
			!mimePattern.MatchString(resource.MIMEType) || len(resource.MIMEType) > 100 || !hashPattern.MatchString(resource.SHA256) ||
			resource.Size < 0 || resource.Size > (1<<53)-1-total {
			return errors.New("invalid share resource")
		}
		if resource.Path != "" {
			if !ValidResourcePath(resource.Path) || paths[resource.Path] {
				return errors.New("invalid or duplicate share resource path")
			}
			paths[resource.Path] = true
		}
		total += resource.Size
		resources[resource.ID] = resource
	}
	if e.Content.ResourceID != "" {
		if len(e.Content.Data) != 0 || resources[e.Content.ResourceID].ID == "" {
			return errors.New("invalid share content reference")
		}
	} else if !object(e.Content.Data) {
		return errors.New("share content must be a JSON object or resource reference")
	}
	if e.Preview != nil {
		switch e.Preview.Type {
		case "builtin":
			if e.Preview.Viewer != "conversation" || e.Preview.Entrypoint != "" {
				return errors.New("unsupported share viewer")
			}
		case "package":
			if e.Preview.Viewer != "" || !paths[e.Preview.Entrypoint] {
				return errors.New("invalid share preview entrypoint")
			}
			for _, resource := range resources {
				if resource.Path == e.Preview.Entrypoint && resource.MIMEType != "text/html" {
					return errors.New("share preview entrypoint must be HTML")
				}
			}
		default:
			return errors.New("unsupported share preview")
		}
	}
	if e.Desktop != nil && (!identifierPattern.MatchString(e.Desktop.ReceiverID) ||
		!version(e.Desktop.ContextVersion) || !object(e.Desktop.Context)) {
		return errors.New("invalid share handoff")
	}
	if e.Service != nil && (e.Service.Type != "webapp" || !opaqueID(e.Service.WebAppID, 128)) {
		return errors.New("unsupported share service binding")
	}
	return nil
}

func version(value int) bool { return value > 0 && value <= (1<<31)-1 }

func text(value string, limit int) bool {
	return strings.TrimSpace(value) != "" && len(value) <= limit && utf8.ValidString(value) &&
		!strings.ContainsFunc(value, unicode.IsControl)
}

func opaqueID(value string, limit int) bool {
	return text(value, limit) && value == strings.TrimSpace(value) &&
		!strings.ContainsAny(value, `/\:?#`)
}

func object(value json.RawMessage) bool {
	trimmed := bytes.TrimSpace(value)
	return len(trimmed) > 0 && trimmed[0] == '{' && json.Valid(trimmed)
}
