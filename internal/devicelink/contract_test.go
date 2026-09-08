package devicelink

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"
)

const fixtureSHA256 = "1984465f5d837277d54eb89fe3ca568c847e3e1451fda4558891a3d05ba3974d"

type contractFixtures struct {
	AccountDeviceLists struct {
		Valid   []json.RawMessage `json:"valid"`
		Invalid []struct {
			Name      string          `json:"name"`
			Value     json.RawMessage `json:"value"`
			ErrorCode string          `json:"errorCode"`
		} `json:"invalid"`
	} `json:"accountDeviceLists"`
	DeviceEvents struct {
		Valid []json.RawMessage `json:"valid"`
	} `json:"deviceEvents"`
	RouteContexts struct {
		Valid json.RawMessage `json:"valid"`
	} `json:"routeContexts"`
}

func loadContractFixtures(t *testing.T) contractFixtures {
	t.Helper()
	data, err := os.ReadFile("../../contracts/device-link/v1/fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != fixtureSHA256 {
		t.Fatalf("fixture drift: got %s want %s", got, fixtureSHA256)
	}
	var fixtures contractFixtures
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	return fixtures
}

func TestSharedDeviceLinkFixtures(t *testing.T) {
	fixtures := loadContractFixtures(t)
	for _, raw := range fixtures.AccountDeviceLists.Valid {
		if _, err := ParseAccountDeviceListV1(raw); err != nil {
			t.Fatalf("valid account device list rejected: %v", err)
		}
	}
	for _, fixture := range fixtures.AccountDeviceLists.Invalid {
		if _, err := ParseAccountDeviceListV1(fixture.Value); !errors.Is(err, ErrProtocolIncompatible) {
			t.Fatalf("%s: got %v, want protocol incompatible", fixture.Name, err)
		}
	}
	for _, raw := range fixtures.DeviceEvents.Valid {
		if _, err := ParseAccountDeviceEventV1(raw); err != nil {
			t.Fatalf("valid device event rejected: %v", err)
		}
	}
}

func TestTrustedRouteContextBindingAndExpiry(t *testing.T) {
	fixtures := loadContractFixtures(t)
	context, err := ParseTrustedRouteContext(fixtures.RouteContexts.Valid)
	if err != nil {
		t.Fatal(err)
	}
	expected := RouteValidation{
		Issuer:          "tunnel-hub",
		Audience:        "desktop-route",
		AccountID:       "account-alice",
		TargetDesktopID: "22222222-2222-4222-8222-222222222222",
		Now:             time.Date(2030, 1, 1, 0, 1, 0, 0, time.UTC),
	}
	if err := ValidateTrustedRouteContext(context, expected); err != nil {
		t.Fatalf("valid route rejected: %v", err)
	}
	expected.TargetDesktopID = "33333333-3333-4333-8333-333333333333"
	if err := ValidateTrustedRouteContext(context, expected); !errors.Is(err, ErrRouteCredentialInvalid) {
		t.Fatalf("target mismatch got %v", err)
	}
	expected.TargetDesktopID = context.TargetDesktopID
	expected.Now = context.ExpiresAt
	if err := ValidateTrustedRouteContext(context, expected); !errors.Is(err, ErrRouteCredentialInvalid) {
		t.Fatalf("expired route got %v", err)
	}
	expected.Now = time.Date(2030, 1, 1, 0, 1, 0, 0, time.UTC)
	expected.IsJTIUsed = func(jti string) bool { return jti == context.JTI }
	if err := ValidateTrustedRouteContext(context, expected); !errors.Is(err, ErrRouteCredentialInvalid) {
		t.Fatalf("replayed route got %v", err)
	}
}
