// Package sharecontract defines the public share envelope. Business payloads stay
// opaque here; conversation snapshots and receiver contexts have their own versions.
package sharecontract

import "encoding/json"

const (
	ProtocolVersion = 1
	ManagementPath  = "/api/desktop/v2/shares"
	PublicPath      = "/share/"
	ManifestPart    = "manifest"
	ResourcePart    = "resource:"
)

type Action string

const (
	Preview  Action = "preview"
	Desktop  Action = "desktop"
	Download Action = "download"
	Service  Action = "service"
	Resolve  Action = "resolve" // A read operation, never a business action.
)

// Envelope is prepared in Main by a source business, never by an untrusted page.
// Each publish owns a new ID and its own resource bytes. It contains no owner,
// access policy, absolute filesystem paths, or credentials.
type Envelope struct {
	ProtocolVersion int          `json:"protocolVersion"`
	Kind            string       `json:"kind"`
	Title           string       `json:"title"`
	Source          *Source      `json:"source,omitempty"`
	Content         Content      `json:"content"`
	Resources       []Resource   `json:"resources"`
	Preview         *PreviewSpec `json:"preview,omitempty"`
	Desktop         *Handoff     `json:"desktop,omitempty"`
	Service         *Binding     `json:"service,omitempty"`
}

// Source is only an optional local navigation hint, not an ownership claim.
type Source struct {
	Module string `json:"module"`
	ID     string `json:"id"`
}

// Content holds exactly one of Data (a JSON object) or ResourceID (a declared
// resource). Version belongs to the business content, not the outer protocol.
type Content struct {
	Version    int             `json:"version"`
	Data       json.RawMessage `json:"data,omitempty"`
	ResourceID string          `json:"resourceId,omitempty"`
}

type Resource struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	MIMEType string `json:"mimeType"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
	Path     string `json:"path,omitempty"` // Controlled, relative POSIX package path.
}

// PreviewSpec is either {type:builtin, viewer:conversation} or
// {type:package, entrypoint:<declared resource path>}. No script runs in Main.
type PreviewSpec struct {
	Type       string `json:"type"`
	Viewer     string `json:"viewer,omitempty"`
	Entrypoint string `json:"entrypoint,omitempty"`
}

type Handoff struct {
	ReceiverID     string          `json:"receiverId"`
	ContextVersion int             `json:"contextVersion"`
	Context        json.RawMessage `json:"context"`
}

// Binding refers only to an existing desktop_webapps row. The WebApp business
// verifies its ownership and current publication; it is never an arbitrary URL.
type Binding struct {
	Type     string `json:"type"` // webapp is the only supported binding type in v1.
	WebAppID string `json:"webAppId"`
}

// Record contains metadata only. HTTP times are UTC RFC3339 with milliseconds;
// Desktop adapters project these into EpochMilliseconds before crossing IPC.
type Record struct {
	ID             string  `json:"id"`
	Kind           string  `json:"kind"`
	Title          string  `json:"title"`
	URL            string  `json:"url"`
	CreatedAt      string  `json:"createdAt"`
	LastAccessedAt *string `json:"lastAccessedAt"`
	Source         *Source `json:"source,omitempty"`
}

type List struct {
	Items []Record `json:"items"`
}

// Resolution deliberately omits owner identity, resource bodies, and content.
// Actions are derived from the stored descriptors, not supplied in a URL.
type Resolution struct {
	ProtocolVersion int      `json:"protocolVersion"`
	ID              string   `json:"id"`
	Kind            string   `json:"kind"`
	Title           string   `json:"title"`
	Actions         []Action `json:"actions"`
	Desktop         *Handoff `json:"desktop,omitempty"`
}

type Limits struct {
	MaxManifestBytes int64 `json:"maxManifestBytes"`
	MaxResourceBytes int64 `json:"maxResourceBytes"`
}
