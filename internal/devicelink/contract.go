package devicelink

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	SchemaVersionV1            = 1
	RouteCredentialTTL         = 2 * time.Minute
	DefaultVerificationTTL     = 5 * time.Minute
	DefaultTemporaryGrantTTL   = 2 * time.Hour
	MinimumTemporaryGrantTTL   = 5 * time.Minute
	MaximumTemporaryGrantTTL   = 8 * time.Hour
	RemoteConversationPageSize = 10
	RouteCredentialIssuer      = "tunnel-hub"
	RouteCredentialAudience    = "desktop-route"
)

type DeviceKind string

const (
	DeviceKindMobile  DeviceKind = "mobile"
	DeviceKindDesktop DeviceKind = "desktop"
)

type TrustLevel string

const (
	TrustLevelSession      TrustLevel = "session"
	TrustLevelTrusted      TrustLevel = "trusted"
	TrustLevelFullyTrusted TrustLevel = "fully_trusted"
)

type AccountDevice struct {
	DeviceID        string     `json:"deviceId"`
	DeviceKind      DeviceKind `json:"deviceKind"`
	DeviceName      string     `json:"deviceName"`
	TrustLevel      TrustLevel `json:"trustLevel"`
	LastSeenAt      time.Time  `json:"lastSeenAt"`
	Online          *bool      `json:"online,omitempty"`
	ConnectedAt     *time.Time `json:"connectedAt,omitempty"`
	LastConnectedAt *time.Time `json:"lastConnectedAt,omitempty"`
}

type AccountDeviceListV1 struct {
	SchemaVersion int             `json:"schemaVersion"`
	Items         []AccountDevice `json:"items"`
	NextCursor    string          `json:"nextCursor,omitempty"`
}

type AccountDeviceEventV1 struct {
	SchemaVersion int            `json:"schemaVersion"`
	EventID       string         `json:"eventId"`
	EventType     string         `json:"eventType"`
	DeviceID      string         `json:"deviceId"`
	OccurredAt    time.Time      `json:"occurredAt"`
	Device        *AccountDevice `json:"device,omitempty"`
	StateVersion  uint64         `json:"stateVersion"`
}

type TrustedRouteContext struct {
	Issuer          string    `json:"issuer"`
	Audience        string    `json:"audience"`
	AccountID       string    `json:"accountId"`
	SourceDeviceID  string    `json:"sourceDeviceId"`
	TargetDesktopID string    `json:"targetDesktopId"`
	RequestID       string    `json:"requestId"`
	JTI             string    `json:"jti"`
	IssuedAt        time.Time `json:"issuedAt"`
	ExpiresAt       time.Time `json:"expiresAt"`
}

type RouteValidation struct {
	Issuer          string
	Audience        string
	AccountID       string
	TargetDesktopID string
	Now             time.Time
	ClockSkew       time.Duration
	IsJTIUsed       func(string) bool
}

var (
	ErrProtocolIncompatible   = errors.New("PROTOCOL_INCOMPATIBLE")
	ErrRouteCredentialInvalid = errors.New("ROUTE_CREDENTIAL_INVALID")
	uuidPattern               = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-8][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)
	validDeviceEvents         = map[string]struct{}{"created": {}, "updated": {}, "deleted": {}, "revoked": {}, "trust_changed": {}}
)

func ParseAccountDeviceListV1(data []byte) (AccountDeviceListV1, error) {
	var payload AccountDeviceListV1
	if err := json.Unmarshal(data, &payload); err != nil {
		return AccountDeviceListV1{}, fmt.Errorf("%w: invalid JSON", ErrProtocolIncompatible)
	}
	if payload.SchemaVersion != SchemaVersionV1 || payload.Items == nil {
		return AccountDeviceListV1{}, ErrProtocolIncompatible
	}
	for i := range payload.Items {
		if err := validateDevice(payload.Items[i]); err != nil {
			return AccountDeviceListV1{}, fmt.Errorf("%w: items[%d]: %v", ErrProtocolIncompatible, i, err)
		}
	}
	return payload, nil
}

func ParseAccountDeviceEventV1(data []byte) (AccountDeviceEventV1, error) {
	var event AccountDeviceEventV1
	if err := json.Unmarshal(data, &event); err != nil {
		return AccountDeviceEventV1{}, fmt.Errorf("%w: invalid JSON", ErrProtocolIncompatible)
	}
	if event.SchemaVersion != SchemaVersionV1 || !isUUID(event.EventID) || !isUUID(event.DeviceID) || event.OccurredAt.IsZero() || event.StateVersion == 0 {
		return AccountDeviceEventV1{}, ErrProtocolIncompatible
	}
	if _, ok := validDeviceEvents[event.EventType]; !ok {
		return AccountDeviceEventV1{}, ErrProtocolIncompatible
	}
	if event.Device != nil {
		if err := validateDevice(*event.Device); err != nil {
			return AccountDeviceEventV1{}, fmt.Errorf("%w: device: %v", ErrProtocolIncompatible, err)
		}
	}
	return event, nil
}

func ParseTrustedRouteContext(data []byte) (TrustedRouteContext, error) {
	var context TrustedRouteContext
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&context); err != nil {
		return TrustedRouteContext{}, fmt.Errorf("%w: invalid route context", ErrRouteCredentialInvalid)
	}
	if strings.TrimSpace(context.Issuer) == "" || strings.TrimSpace(context.Audience) == "" ||
		strings.TrimSpace(context.AccountID) == "" || !isUUID(context.SourceDeviceID) ||
		!isUUID(context.TargetDesktopID) || !isUUID(context.RequestID) || !isUUID(context.JTI) ||
		context.IssuedAt.IsZero() || context.ExpiresAt.IsZero() ||
		!context.ExpiresAt.After(context.IssuedAt) || context.ExpiresAt.Sub(context.IssuedAt) > RouteCredentialTTL {
		return TrustedRouteContext{}, ErrRouteCredentialInvalid
	}
	return context, nil
}

func ValidateTrustedRouteContext(context TrustedRouteContext, expected RouteValidation) error {
	now := expected.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	skew := expected.ClockSkew
	if skew < 0 {
		skew = 0
	}
	if context.Issuer != expected.Issuer || context.Audience != expected.Audience ||
		context.AccountID != expected.AccountID || context.TargetDesktopID != expected.TargetDesktopID ||
		now.Before(context.IssuedAt.Add(-skew)) || !now.Before(context.ExpiresAt.Add(skew)) {
		return ErrRouteCredentialInvalid
	}
	if expected.IsJTIUsed != nil && expected.IsJTIUsed(context.JTI) {
		return ErrRouteCredentialInvalid
	}
	return nil
}

func validateDevice(device AccountDevice) error {
	if !isUUID(device.DeviceID) || strings.TrimSpace(device.DeviceName) == "" || len([]rune(device.DeviceName)) > 64 || device.LastSeenAt.IsZero() {
		return errors.New("missing or invalid required field")
	}
	switch device.DeviceKind {
	case DeviceKindMobile, DeviceKindDesktop:
	default:
		return errors.New("unknown deviceKind")
	}
	switch device.TrustLevel {
	case TrustLevelSession, TrustLevelTrusted, TrustLevelFullyTrusted:
	default:
		return errors.New("unknown trustLevel")
	}
	if device.DeviceKind == DeviceKindMobile && device.Online != nil {
		return errors.New("mobile online presence is not connectable")
	}
	return nil
}

func isUUID(value string) bool {
	return uuidPattern.MatchString(strings.TrimSpace(value))
}

func IsUUID(value string) bool { return isUUID(value) }
