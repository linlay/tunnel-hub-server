package devicelink

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var (
	ErrDeviceValidationRejected    = errors.New("account device validation rejected")
	ErrDeviceValidationUnavailable = errors.New("account device validation unavailable")
)

type DeviceValidation struct {
	AccountID           string `json:"accountId"`
	SourceDeviceID      string `json:"sourceDeviceId"`
	SourceTrustLevel    string `json:"sourceTrustLevel"`
	ValidatedDeviceID   string `json:"validatedDeviceId"`
	ValidatedTrustLevel string `json:"validatedTrustLevel"`
	StateVersion        uint64 `json:"stateVersion"`
}

type AccountDeviceValidator interface {
	Validate(context.Context, string, string, string, string) (DeviceValidation, error)
}

type AccountDeviceValidatorFunc func(context.Context, string, string, string, string) (DeviceValidation, error)

func (fn AccountDeviceValidatorFunc) Validate(ctx context.Context, bearer, accountID, sourceDeviceID, deviceID string) (DeviceValidation, error) {
	return fn(ctx, bearer, accountID, sourceDeviceID, deviceID)
}

type IdentityDeviceValidator struct {
	endpoint string
	client   *http.Client
}

func NewIdentityDeviceValidator(baseURL string, client *http.Client) (*IdentityDeviceValidator, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLoopbackHost(parsed.Hostname()))) {
		return nil, errors.New("IDENTITY_API_BASE_URL must be HTTPS or loopback HTTP")
	}
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Second}
	}
	return &IdentityDeviceValidator{endpoint: baseURL + "/api/auth/account-devices/validate", client: client}, nil
}

func (v *IdentityDeviceValidator) Validate(ctx context.Context, bearer, accountID, sourceDeviceID, deviceID string) (DeviceValidation, error) {
	payload, err := json.Marshal(map[string]string{"deviceId": strings.TrimSpace(deviceID)})
	if err != nil {
		return DeviceValidation{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.endpoint, bytes.NewReader(payload))
	if err != nil {
		return DeviceValidation{}, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(bearer))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	response, err := v.client.Do(req)
	if err != nil {
		return DeviceValidation{}, fmt.Errorf("%w: %v", ErrDeviceValidationUnavailable, err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusNotFound {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return DeviceValidation{}, ErrDeviceValidationRejected
	}
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return DeviceValidation{}, fmt.Errorf("%w: identity returned %d", ErrDeviceValidationUnavailable, response.StatusCode)
	}
	var result struct {
		SchemaVersion int `json:"schemaVersion"`
		DeviceValidation
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 64<<10))
	if err := decoder.Decode(&result); err != nil || result.SchemaVersion != SchemaVersionV1 {
		return DeviceValidation{}, ErrDeviceValidationUnavailable
	}
	expectedDeviceID := strings.TrimSpace(deviceID)
	if expectedDeviceID == "" {
		expectedDeviceID = strings.TrimSpace(sourceDeviceID)
	}
	if result.AccountID != strings.TrimSpace(accountID) || result.SourceDeviceID != strings.TrimSpace(sourceDeviceID) || result.ValidatedDeviceID != expectedDeviceID || !validTrustLevel(result.SourceTrustLevel) || !validTrustLevel(result.ValidatedTrustLevel) {
		return DeviceValidation{}, ErrDeviceValidationRejected
	}
	return result.DeviceValidation, nil
}

func isLoopbackHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func validTrustLevel(value string) bool {
	switch value {
	case string(TrustLevelSession), string(TrustLevelTrusted), string(TrustLevelFullyTrusted):
		return true
	default:
		return false
	}
}
