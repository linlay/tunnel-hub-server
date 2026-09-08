package devicelink

import "time"

const (
	ErrorTargetOffline        = "TARGET_OFFLINE"
	ErrorProtocolIncompatible = "PROTOCOL_INCOMPATIBLE"
)

type DesktopPresence struct {
	ConnectedAt     time.Time
	LastConnectedAt time.Time
}

type RouteSelectionError struct {
	Code string
}

func (e *RouteSelectionError) Error() string { return e.Code }

// MergeDesktopPresence keeps Identity as the device-list authority and uses
// Hub state only to project connectability for Desktop records. Input order is
// preserved so pagination remains stable.
func MergeDesktopPresence(devices []AccountDevice, onlineByDeviceID map[string]DesktopPresence) []AccountDevice {
	merged := make([]AccountDevice, len(devices))
	copy(merged, devices)
	for i := range merged {
		device := &merged[i]
		if device.DeviceKind != DeviceKindDesktop {
			device.Online = nil
			device.ConnectedAt = nil
			device.LastConnectedAt = nil
			continue
		}
		presence, online := onlineByDeviceID[device.DeviceID]
		device.Online = boolPointer(online)
		if online {
			device.ConnectedAt = timePointer(presence.ConnectedAt)
		} else {
			device.ConnectedAt = nil
		}
		if !presence.LastConnectedAt.IsZero() {
			device.LastConnectedAt = timePointer(presence.LastConnectedAt)
		}
	}
	return merged
}

// ResolveActiveDesktop accepts only an online Desktop from the authoritative
// account list. A mobile device or unknown enum is a protocol error; a known
// offline Desktop is a stable availability error.
func ResolveActiveDesktop(devices []AccountDevice, activeDesktopID string) (AccountDevice, error) {
	for _, device := range devices {
		if device.DeviceID != activeDesktopID {
			continue
		}
		if device.DeviceKind != DeviceKindDesktop {
			return AccountDevice{}, &RouteSelectionError{Code: ErrorProtocolIncompatible}
		}
		if device.Online == nil || !*device.Online {
			return AccountDevice{}, &RouteSelectionError{Code: ErrorTargetOffline}
		}
		return device, nil
	}
	return AccountDevice{}, &RouteSelectionError{Code: ErrorProtocolIncompatible}
}

func boolPointer(value bool) *bool { return &value }

func timePointer(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	value = value.UTC()
	return &value
}
