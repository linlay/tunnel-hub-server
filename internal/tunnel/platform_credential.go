package tunnel

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Supplied by the authenticated Desktop. Local Identity/Platform owns signature verification.
type PlatformCredential struct {
	Token            string    `json:"token"`
	PlatformDeviceID string    `json:"platformDeviceId"`
	ExpiresAt        time.Time `json:"expiresAt"`
}

func (c PlatformCredential) Validate(now time.Time) error {
	parts := strings.Split(c.Token, ".")
	if len(parts) != 3 || len(c.Token) > 16384 || c.PlatformDeviceID == "" || !c.ExpiresAt.After(now) {
		return errors.New("invalid Platform credential")
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	var claims struct {
		Scope    string `json:"scope"`
		DeviceID string `json:"device_id"`
		Exp      int64  `json:"exp"`
	}
	if err != nil || json.Unmarshal(data, &claims) != nil || claims.Scope != "app" || claims.DeviceID != c.PlatformDeviceID || !c.ExpiresAt.Equal(time.Unix(claims.Exp, 0)) {
		return errors.New("invalid Platform credential fields")
	}
	return nil
}
