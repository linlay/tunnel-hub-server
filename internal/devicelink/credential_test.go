package devicelink

import (
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRouteCredentialSigningBindingReplayAndRotation(t *testing.T) {
	currentKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	previousKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := NewRouteCredentialSigner("tunnel-hub", "desktop-route", "current", currentKey)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	token, issued, err := signer.Sign(RouteCredentialInput{
		AccountID:       "account-alice",
		SourceDeviceID:  "11111111-1111-4111-8111-111111111111",
		TargetDesktopID: "22222222-2222-4222-8222-222222222222",
		RequestID:       "55555555-5555-4555-8555-555555555555",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]*rsa.PublicKey{"previous": &previousKey.PublicKey, "current": &currentKey.PublicKey}
	expected := RouteValidation{
		Issuer: "tunnel-hub", Audience: "desktop-route", AccountID: "account-alice",
		TargetDesktopID: issued.TargetDesktopID, Now: now.Add(time.Minute),
	}
	if _, err := VerifyRouteCredential(token, keys, expected); err != nil {
		t.Fatalf("valid credential rejected: %v", err)
	}

	badTarget := expected
	badTarget.TargetDesktopID = "33333333-3333-4333-8333-333333333333"
	if _, err := VerifyRouteCredential(token, keys, badTarget); !errors.Is(err, ErrRouteCredentialInvalid) {
		t.Fatalf("target mismatch got %v", err)
	}
	expired := expected
	expired.Now = issued.ExpiresAt
	if _, err := VerifyRouteCredential(token, keys, expired); !errors.Is(err, ErrRouteCredentialInvalid) {
		t.Fatalf("expired credential got %v", err)
	}
	replayed := expected
	replayed.IsJTIUsed = func(jti string) bool { return jti == issued.JTI }
	if _, err := VerifyRouteCredential(token, keys, replayed); !errors.Is(err, ErrRouteCredentialInvalid) {
		t.Fatalf("replayed credential got %v", err)
	}

	parts := strings.Split(token, ".")
	parts[1] = parts[1][:len(parts[1])-1] + "A"
	if _, err := VerifyRouteCredential(strings.Join(parts, "."), keys, expected); !errors.Is(err, ErrRouteCredentialInvalid) {
		t.Fatalf("tampered credential got %v", err)
	}
	delete(keys, "current")
	if _, err := VerifyRouteCredential(token, keys, expected); !errors.Is(err, ErrRouteCredentialInvalid) {
		t.Fatalf("unknown kid got %v", err)
	}
}
