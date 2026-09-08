package devicelink

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func TestIdentityDeviceValidatorChecksBindings(t *testing.T) {
	const sourceID = "11111111-1111-4111-8111-111111111111"
	const targetID = "22222222-2222-4222-8222-222222222222"
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != "https://identity.example.test/api/auth/account-devices/validate" || request.Header.Get("Authorization") != "Bearer source-token" {
			t.Fatalf("unexpected validation request: %s headers=%v", request.URL, request.Header)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"schemaVersion":1,"accountId":"account-alice","sourceDeviceId":"` + sourceID + `","sourceTrustLevel":"trusted","validatedDeviceId":"` + targetID + `","validatedTrustLevel":"fully_trusted","stateVersion":7}`)),
			Header:     make(http.Header),
		}, nil
	})}
	validator, err := NewIdentityDeviceValidator("https://identity.example.test", client)
	if err != nil {
		t.Fatalf("new validator: %v", err)
	}
	result, err := validator.Validate(context.Background(), "source-token", "account-alice", sourceID, targetID)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if result.StateVersion != 7 || result.ValidatedTrustLevel != "fully_trusted" {
		t.Fatalf("unexpected validation: %+v", result)
	}
	if _, err := validator.Validate(context.Background(), "source-token", "account-bob", sourceID, targetID); err != ErrDeviceValidationRejected {
		t.Fatalf("mismatched account error = %v", err)
	}
}

func TestIdentityDeviceValidatorRejectsUnsafeOrigins(t *testing.T) {
	for _, raw := range []string{"http://identity.example.test", "file:///tmp/identity", "https://"} {
		if _, err := NewIdentityDeviceValidator(raw, nil); err == nil {
			t.Fatalf("unsafe Identity URL accepted: %q", raw)
		}
	}
	if _, err := NewIdentityDeviceValidator("http://127.0.0.1:8080", nil); err != nil {
		t.Fatalf("loopback development URL rejected: %v", err)
	}
}
