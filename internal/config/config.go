package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

type RelayConfig struct {
	BrandID                  string
	ProductName              string
	PublicSiteTitle          string
	Addr                     string
	DatabasePath             string
	RelayPublicURL           string
	AdminHost                string
	WebsiteDist              string
	SharePublicBaseURL       string
	PublicBaseDomain         string
	DesktopPublicBaseDomain  string
	WebAppPublicBaseDomain   string
	AdminUsername            string
	AdminPassword            string
	AdminSessionTTL          time.Duration
	CookieSecure             bool
	MobileWebAppCookieSecure bool
	SSOJWTIssuer             string
	SSOJWTPublicKeyFile      string
	SSOJWTPublicKeyPEM       string
	SSOJWTAudience           string
	SSOJWTUserIDClaim        string
	SSOJWTAllowAnyAudience   bool
	SSOJWTAllowAnyAdminRole  bool
	SSOJWTAllowMissingScope  bool
	MaxRequestBodyBytes      int64
	TrustedProxyCIDRs        string
}

type AgentConfig struct {
	RelayURL           string
	Token              string
	InsecureSkipVerify bool
	ReconnectInterval  time.Duration
}

func LoadRelayConfigStrict() (RelayConfig, error) {
	loadDotEnv()
	brand, err := LoadBrandConfigFromEnv()
	if err != nil {
		return RelayConfig{}, err
	}
	addr, err := requiredEnv("RELAY_ADDR")
	if err != nil {
		return RelayConfig{}, err
	}
	_, port, err := net.SplitHostPort(addr)
	portNumber, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || portNumber < 1 || portNumber > 65535 {
		return RelayConfig{}, fmt.Errorf("RELAY_ADDR is invalid")
	}
	databasePath, err := requiredEnv("RELAY_DB_PATH")
	if err != nil {
		return RelayConfig{}, err
	}
	issuer, err := requiredEnv("SSO_JWT_ISSUER")
	if err != nil {
		return RelayConfig{}, err
	}
	audience, err := requiredEnv("SSO_JWT_AUDIENCE")
	if err != nil {
		return RelayConfig{}, err
	}
	userIDClaim, err := requiredEnv("SSO_JWT_USER_ID_CLAIM")
	if err != nil {
		return RelayConfig{}, err
	}
	publicKeyFile := strings.TrimSpace(os.Getenv("SSO_JWT_PUBLIC_KEY_FILE"))
	publicKeyPEM := strings.TrimSpace(os.Getenv("SSO_JWT_PUBLIC_KEY_PEM"))
	if publicKeyFile == "" && publicKeyPEM == "" {
		return RelayConfig{}, fmt.Errorf("SSO_JWT_PUBLIC_KEY_FILE or SSO_JWT_PUBLIC_KEY_PEM is required")
	}
	return RelayConfig{
		BrandID:                  brand.Brand.ID,
		ProductName:              brand.Brand.ProductName,
		PublicSiteTitle:          brand.Brand.PublicSiteTitle,
		Addr:                     addr,
		DatabasePath:             databasePath,
		RelayPublicURL:           brand.Endpoints.RelayPublicURL,
		AdminHost:                env("ADMIN_HOST", ""),
		WebsiteDist:              env("WEBSITE_DIST", ""),
		SharePublicBaseURL:       brand.Endpoints.SharePublicBaseURL,
		PublicBaseDomain:         brand.Domains.PublicBase,
		DesktopPublicBaseDomain:  brand.Domains.DesktopPublicBase,
		WebAppPublicBaseDomain:   brand.Domains.WebAppPublicBase,
		AdminUsername:            env("ADMIN_USERNAME", env("BOOTSTRAP_ADMIN_USERNAME", "admin")),
		AdminPassword:            env("ADMIN_PASSWORD", os.Getenv("BOOTSTRAP_ADMIN_PASSWORD")),
		AdminSessionTTL:          envDuration("ADMIN_SESSION_TTL", 24*time.Hour),
		CookieSecure:             envBool("COOKIE_SECURE", false),
		MobileWebAppCookieSecure: envBool("MOBILE_WEBAPP_COOKIE_SECURE", true),
		SSOJWTIssuer:             issuer,
		SSOJWTPublicKeyFile:      publicKeyFile,
		SSOJWTPublicKeyPEM:       publicKeyPEM,
		SSOJWTAudience:           audience,
		SSOJWTUserIDClaim:        userIDClaim,
		SSOJWTAllowAnyAudience:   envBool("SSO_JWT_ALLOW_ANY_AUDIENCE", false),
		SSOJWTAllowAnyAdminRole:  envBool("SSO_JWT_ALLOW_ANY_ADMIN_ROLE", false),
		SSOJWTAllowMissingScope:  envBool("SSO_JWT_ALLOW_MISSING_TUNNEL_SCOPE", false),
		MaxRequestBodyBytes:      envInt64("MAX_REQUEST_BODY_BYTES", 64<<20),
		TrustedProxyCIDRs:        env("TRUSTED_PROXY_CIDRS", ""),
	}, nil
}

func requiredEnv(key string) (string, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return "", fmt.Errorf("%s is required", key)
	}
	return value, nil
}

func LoadAgentConfig() AgentConfig {
	loadDotEnv()

	return AgentConfig{
		RelayURL:           env("AGENT_RELAY_URL", "ws://127.0.0.1:11961/tunnel"),
		Token:              os.Getenv("AGENT_TOKEN"),
		InsecureSkipVerify: envBool("AGENT_TLS_INSECURE_SKIP_VERIFY", false),
		ReconnectInterval:  time.Duration(envInt64("AGENT_RECONNECT_SECONDS", 3)) * time.Second,
	}
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envInt64(key string, fallback int64) int64 {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

func envDuration(key string, fallback time.Duration) time.Duration {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}
