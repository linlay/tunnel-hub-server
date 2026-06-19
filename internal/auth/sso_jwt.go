package auth

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"os"
	"strings"
	"time"
)

var (
	ErrSSOJWTNotConfigured = errors.New("SSO JWT verifier is not configured")
	ErrBearerTokenMissing  = errors.New("bearer token is missing")
)

type SSOJWTConfig struct {
	Issuer        string
	Audience      string
	PublicKeyFile string
	PublicKeyPEM  string
}

type SSOJWTPrincipal struct {
	UserID string
	Email  string
	Role   string
	Scope  string
}

type SSOJWTVerifier struct {
	issuer    string
	audience  string
	publicKey *rsa.PublicKey
}

func NewSSOJWTVerifier(config SSOJWTConfig) (*SSOJWTVerifier, error) {
	issuer := strings.TrimSpace(config.Issuer)
	audience := strings.TrimSpace(config.Audience)
	if issuer == "" && strings.TrimSpace(config.PublicKeyFile) == "" && strings.TrimSpace(config.PublicKeyPEM) == "" {
		return nil, nil
	}
	publicKey, configured, err := loadSSOJWTPublicKey(config.PublicKeyFile, config.PublicKeyPEM)
	if err != nil {
		return nil, err
	}
	if issuer == "" {
		return nil, errors.New("SSO_JWT_ISSUER is required")
	}
	if audience == "" {
		return nil, errors.New("SSO_JWT_AUDIENCE is required")
	}
	if !configured {
		return nil, errors.New("SSO_JWT_PUBLIC_KEY_FILE or SSO_JWT_PUBLIC_KEY_PEM is required")
	}
	return &SSOJWTVerifier{
		issuer:    issuer,
		audience:  audience,
		publicKey: publicKey,
	}, nil
}

func (p SSOJWTPrincipal) HasScope(scope string) bool {
	scope = strings.TrimSpace(scope)
	if scope == "" {
		return false
	}
	for _, item := range strings.Fields(p.Scope) {
		if item == scope {
			return true
		}
	}
	return false
}

func loadSSOJWTPublicKey(filePath, pemValue string) (*rsa.PublicKey, bool, error) {
	filePath = strings.TrimSpace(filePath)
	pemValue = strings.TrimSpace(pemValue)
	if filePath != "" {
		content, err := os.ReadFile(filePath)
		if err != nil {
			return nil, true, err
		}
		key, err := parseSSOJWTPublicKeyPEM(string(content))
		return key, true, err
	}
	if pemValue == "" {
		return nil, false, nil
	}
	key, err := parseSSOJWTPublicKeyPEM(strings.ReplaceAll(pemValue, `\n`, "\n"))
	return key, true, err
}

func parseSSOJWTPublicKeyPEM(value string) (*rsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(value)))
	if block == nil {
		return nil, errors.New("SSO JWT public key PEM is invalid")
	}
	if key, err := x509.ParsePKIXPublicKey(block.Bytes); err == nil {
		publicKey, ok := key.(*rsa.PublicKey)
		if !ok {
			return nil, errors.New("SSO JWT public key must be RSA")
		}
		return publicKey, nil
	}
	publicKey, err := x509.ParsePKCS1PublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	return publicKey, nil
}

func (v *SSOJWTVerifier) VerifyBearerHeader(header string) (SSOJWTPrincipal, error) {
	if v == nil {
		return SSOJWTPrincipal{}, ErrSSOJWTNotConfigured
	}
	token := bearerToken(header)
	if token == "" {
		return SSOJWTPrincipal{}, ErrBearerTokenMissing
	}
	return v.Verify(token, time.Now())
}

func (v *SSOJWTVerifier) Verify(token string, now time.Time) (SSOJWTPrincipal, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return SSOJWTPrincipal{}, errors.New("invalid JWT")
	}
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return SSOJWTPrincipal{}, err
	}
	var header map[string]any
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return SSOJWTPrincipal{}, err
	}
	if header["alg"] != "RS256" {
		return SSOJWTPrincipal{}, errors.New("unsupported JWT alg")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return SSOJWTPrincipal{}, err
	}
	signedValue := parts[0] + "." + parts[1]
	digest := sha256.Sum256([]byte(signedValue))
	if err := rsa.VerifyPKCS1v15(v.publicKey, crypto.SHA256, digest[:], signature); err != nil {
		return SSOJWTPrincipal{}, err
	}
	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return SSOJWTPrincipal{}, err
	}
	var claims map[string]any
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		return SSOJWTPrincipal{}, err
	}
	if readStringClaim(claims, "iss") != v.issuer {
		return SSOJWTPrincipal{}, errors.New("issuer mismatch")
	}
	if !claimHasAudience(claims["aud"], v.audience) {
		return SSOJWTPrincipal{}, errors.New("audience mismatch")
	}
	exp := readNumberClaim(claims, "exp")
	if exp <= 0 || exp <= now.Unix() {
		return SSOJWTPrincipal{}, errors.New("token expired")
	}
	userID := readStringClaim(claims, "user_id")
	if userID == "" {
		return SSOJWTPrincipal{}, errors.New("missing user_id")
	}
	return SSOJWTPrincipal{
		UserID: userID,
		Email:  readStringClaim(claims, "email"),
		Role:   strings.ToLower(readStringClaim(claims, "role")),
		Scope:  readStringClaim(claims, "scope"),
	}, nil
}

func bearerToken(header string) string {
	fields := strings.Fields(strings.TrimSpace(header))
	if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") {
		return ""
	}
	return strings.TrimSpace(fields[1])
}

func readStringClaim(claims map[string]any, name string) string {
	value, _ := claims[name].(string)
	return strings.TrimSpace(value)
}

func readNumberClaim(claims map[string]any, name string) int64 {
	switch value := claims[name].(type) {
	case float64:
		return int64(value)
	case int64:
		return value
	case json.Number:
		parsed, _ := value.Int64()
		return parsed
	default:
		return 0
	}
}

func claimHasAudience(value any, audience string) bool {
	switch typed := value.(type) {
	case string:
		return typed == audience
	case []any:
		for _, item := range typed {
			if item == audience {
				return true
			}
		}
	}
	return false
}
