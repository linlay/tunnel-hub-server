package devicelink

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strings"
	"time"
)

type RouteCredentialInput struct {
	AccountID       string
	SourceDeviceID  string
	TargetDesktopID string
	RequestID       string
	JTI             string
}

type RouteCredentialSigner struct {
	issuer     string
	audience   string
	kid        string
	privateKey *rsa.PrivateKey
}

type RouteCredentialJWK struct {
	KeyType   string `json:"kty"`
	Use       string `json:"use"`
	Algorithm string `json:"alg"`
	KeyID     string `json:"kid"`
	Modulus   string `json:"n"`
	Exponent  string `json:"e"`
}

type routeCredentialHeader struct {
	Algorithm string `json:"alg"`
	Type      string `json:"typ"`
	KeyID     string `json:"kid"`
}

type routeCredentialClaims struct {
	Issuer          string `json:"iss"`
	Audience        string `json:"aud"`
	AccountID       string `json:"accountId"`
	SourceDeviceID  string `json:"sourceDeviceId"`
	TargetDesktopID string `json:"targetDesktopId"`
	RequestID       string `json:"requestId"`
	JTI             string `json:"jti"`
	IssuedAt        int64  `json:"iat"`
	ExpiresAt       int64  `json:"exp"`
}

func NewRouteCredentialSigner(issuer, audience, kid string, privateKey *rsa.PrivateKey) (*RouteCredentialSigner, error) {
	issuer = strings.TrimSpace(issuer)
	audience = strings.TrimSpace(audience)
	kid = strings.TrimSpace(kid)
	if issuer == "" || audience == "" || kid == "" || privateKey == nil || privateKey.N.BitLen() < 2048 {
		return nil, errors.New("route credential signer requires issuer, audience, kid and an RSA-2048+ key")
	}
	return &RouteCredentialSigner{issuer: issuer, audience: audience, kid: kid, privateKey: privateKey}, nil
}

func LoadRouteCredentialSigner(issuer, audience, kid, privateKeyFile, privateKeyPEM string) (*RouteCredentialSigner, error) {
	privateKeyFile = strings.TrimSpace(privateKeyFile)
	privateKeyPEM = strings.TrimSpace(privateKeyPEM)
	if privateKeyFile == "" && privateKeyPEM == "" {
		return nil, nil
	}
	if privateKeyFile != "" {
		raw, err := os.ReadFile(privateKeyFile)
		if err != nil {
			return nil, err
		}
		privateKeyPEM = string(raw)
	}
	block, _ := pem.Decode([]byte(strings.ReplaceAll(privateKeyPEM, `\n`, "\n")))
	if block == nil {
		return nil, errors.New("route credential private key PEM is invalid")
	}
	var privateKey *rsa.PrivateKey
	if parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		var ok bool
		privateKey, ok = parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("route credential private key must be RSA")
		}
	} else {
		var parseErr error
		privateKey, parseErr = x509.ParsePKCS1PrivateKey(block.Bytes)
		if parseErr != nil {
			return nil, parseErr
		}
	}
	return NewRouteCredentialSigner(issuer, audience, kid, privateKey)
}

func (s *RouteCredentialSigner) JWK() (RouteCredentialJWK, error) {
	if s == nil || s.privateKey == nil {
		return RouteCredentialJWK{}, errors.New("route credential signer is not configured")
	}
	return RouteCredentialJWK{
		KeyType: "RSA", Use: "sig", Algorithm: "RS256", KeyID: s.kid,
		Modulus:  base64.RawURLEncoding.EncodeToString(s.privateKey.PublicKey.N.Bytes()),
		Exponent: base64.RawURLEncoding.EncodeToString(big.NewInt(int64(s.privateKey.PublicKey.E)).Bytes()),
	}, nil
}

func (s *RouteCredentialSigner) Sign(input RouteCredentialInput, now time.Time) (string, TrustedRouteContext, error) {
	if s == nil || s.privateKey == nil {
		return "", TrustedRouteContext{}, errors.New("route credential signer is not configured")
	}
	input.AccountID = strings.TrimSpace(input.AccountID)
	if input.AccountID == "" || !isUUID(input.SourceDeviceID) || !isUUID(input.TargetDesktopID) || !isUUID(input.RequestID) {
		return "", TrustedRouteContext{}, ErrRouteCredentialInvalid
	}
	if strings.TrimSpace(input.JTI) == "" {
		generated, err := newUUID()
		if err != nil {
			return "", TrustedRouteContext{}, err
		}
		input.JTI = generated
	}
	if !isUUID(input.JTI) {
		return "", TrustedRouteContext{}, ErrRouteCredentialInvalid
	}
	now = now.UTC().Truncate(time.Second)
	expiresAt := now.Add(RouteCredentialTTL)
	claims := routeCredentialClaims{
		Issuer:          s.issuer,
		Audience:        s.audience,
		AccountID:       input.AccountID,
		SourceDeviceID:  input.SourceDeviceID,
		TargetDesktopID: input.TargetDesktopID,
		RequestID:       input.RequestID,
		JTI:             input.JTI,
		IssuedAt:        now.Unix(),
		ExpiresAt:       expiresAt.Unix(),
	}
	header := routeCredentialHeader{Algorithm: "RS256", Type: "JWT", KeyID: s.kid}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", TrustedRouteContext{}, err
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", TrustedRouteContext{}, err
	}
	unsigned := base64.RawURLEncoding.EncodeToString(headerJSON) + "." + base64.RawURLEncoding.EncodeToString(claimsJSON)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, s.privateKey, crypto.SHA256, digest[:])
	if err != nil {
		return "", TrustedRouteContext{}, err
	}
	context := TrustedRouteContext{
		Issuer:          claims.Issuer,
		Audience:        claims.Audience,
		AccountID:       claims.AccountID,
		SourceDeviceID:  claims.SourceDeviceID,
		TargetDesktopID: claims.TargetDesktopID,
		RequestID:       claims.RequestID,
		JTI:             claims.JTI,
		IssuedAt:        now,
		ExpiresAt:       expiresAt,
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature), context, nil
}

func VerifyRouteCredential(token string, publicKeys map[string]*rsa.PublicKey, expected RouteValidation) (TrustedRouteContext, error) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return TrustedRouteContext{}, ErrRouteCredentialInvalid
	}
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return TrustedRouteContext{}, ErrRouteCredentialInvalid
	}
	var header routeCredentialHeader
	if err := json.Unmarshal(headerJSON, &header); err != nil || header.Algorithm != "RS256" || header.Type != "JWT" || header.KeyID == "" {
		return TrustedRouteContext{}, ErrRouteCredentialInvalid
	}
	publicKey := publicKeys[header.KeyID]
	if publicKey == nil || publicKey.N.BitLen() < 2048 {
		return TrustedRouteContext{}, ErrRouteCredentialInvalid
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return TrustedRouteContext{}, ErrRouteCredentialInvalid
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(publicKey, crypto.SHA256, digest[:], signature); err != nil {
		return TrustedRouteContext{}, ErrRouteCredentialInvalid
	}
	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return TrustedRouteContext{}, ErrRouteCredentialInvalid
	}
	var claims routeCredentialClaims
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		return TrustedRouteContext{}, ErrRouteCredentialInvalid
	}
	context := TrustedRouteContext{
		Issuer:          claims.Issuer,
		Audience:        claims.Audience,
		AccountID:       claims.AccountID,
		SourceDeviceID:  claims.SourceDeviceID,
		TargetDesktopID: claims.TargetDesktopID,
		RequestID:       claims.RequestID,
		JTI:             claims.JTI,
		IssuedAt:        time.Unix(claims.IssuedAt, 0).UTC(),
		ExpiresAt:       time.Unix(claims.ExpiresAt, 0).UTC(),
	}
	encoded, err := json.Marshal(context)
	if err != nil {
		return TrustedRouteContext{}, ErrRouteCredentialInvalid
	}
	parsed, err := ParseTrustedRouteContext(encoded)
	if err != nil {
		return TrustedRouteContext{}, ErrRouteCredentialInvalid
	}
	if err := ValidateTrustedRouteContext(parsed, expected); err != nil {
		return TrustedRouteContext{}, ErrRouteCredentialInvalid
	}
	return parsed, nil
}

func newUUID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		value[0:4], value[4:6], value[6:8], value[8:10], value[10:16]), nil
}
