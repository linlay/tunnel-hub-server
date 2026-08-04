package auth

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"testing"
	"time"
)

func TestSSOJWTVerifierAllowsAnyAudienceAndCustomUserIDClaim(t *testing.T) {
	privateKey, publicKeyPEM := testJWTKey(t)
	verifier, err := NewSSOJWTVerifier(SSOJWTConfig{
		Issuer:           "https://issuer.example.test/oidc",
		UserIDClaim:      "userId",
		AllowAnyAudience: true,
		PublicKeyPEM:     publicKeyPEM,
	})
	if err != nil {
		t.Fatalf("new verifier: %v", err)
	}
	now := time.Now().UTC()
	token := signJWTClaims(t, privateKey, map[string]any{
		"iss":    "https://issuer.example.test/oidc",
		"aud":    "unrelated-client",
		"userId": "external-user-42",
		"email":  "user@example.test",
		"iat":    now.Add(-time.Minute).Unix(),
		"nbf":    now.Add(-time.Minute).Unix(),
		"exp":    now.Add(time.Hour).Unix(),
	})

	principal, err := verifier.Verify(token, now)
	if err != nil {
		t.Fatalf("verify relaxed token: %v", err)
	}
	if principal.UserID != "external-user-42" || principal.Email != "user@example.test" {
		t.Fatalf("unexpected principal: %+v", principal)
	}
}

func TestSSOJWTVerifierStillRejectsFutureTokenInRelaxedAudienceMode(t *testing.T) {
	privateKey, publicKeyPEM := testJWTKey(t)
	verifier, err := NewSSOJWTVerifier(SSOJWTConfig{
		Issuer:           "https://issuer.example.test/oidc",
		UserIDClaim:      "sub",
		AllowAnyAudience: true,
		PublicKeyPEM:     publicKeyPEM,
	})
	if err != nil {
		t.Fatalf("new verifier: %v", err)
	}
	now := time.Now().UTC()
	token := signJWTClaims(t, privateKey, map[string]any{
		"iss": "https://issuer.example.test/oidc",
		"sub": "external-user-42",
		"nbf": now.Add(5 * time.Minute).Unix(),
		"exp": now.Add(time.Hour).Unix(),
	})

	if _, err := verifier.Verify(token, now); err == nil {
		t.Fatal("future token was accepted")
	}
}

func testJWTKey(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	return privateKey, string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}))
}

func signJWTClaims(t *testing.T, privateKey *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	headerJSON, err := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT"})
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	header := base64.RawURLEncoding.EncodeToString(headerJSON)
	payload := base64.RawURLEncoding.EncodeToString(claimsJSON)
	signed := header + "." + payload
	digest := sha256.Sum256([]byte(signed))
	signature, err := rsa.SignPKCS1v15(rand.Reader, privateKey, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign JWT: %v", err)
	}
	return signed + "." + base64.RawURLEncoding.EncodeToString(signature)
}
