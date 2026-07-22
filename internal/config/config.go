package config

import (
	"os"
	"strconv"
	"time"
)

type RelayConfig struct {
	Addr                     string
	DatabasePath             string
	RelayPublicURL           string
	AdminHost                string
	WebsiteDist              string
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
	MaxRequestBodyBytes      int64
	TrustedProxyCIDRs        string
}

type AgentConfig struct {
	RelayURL           string
	Token              string
	InsecureSkipVerify bool
	ReconnectInterval  time.Duration
}

func LoadRelayConfig() RelayConfig {
	loadDotEnv()

	return RelayConfig{
		Addr:                     env("RELAY_ADDR", ":8080"),
		DatabasePath:             env("RELAY_DB_PATH", "tunnel.db"),
		RelayPublicURL:           env("RELAY_PUBLIC_URL", ""),
		AdminHost:                env("ADMIN_HOST", ""),
		WebsiteDist:              env("WEBSITE_DIST", ""),
		PublicBaseDomain:         env("PUBLIC_BASE_DOMAIN", "tunnel-hub.zenmind.cc"),
		DesktopPublicBaseDomain:  env("DESKTOP_PUBLIC_BASE_DOMAIN", "m.zenmind.cc"),
		WebAppPublicBaseDomain:   env("WEBAPP_PUBLIC_BASE_DOMAIN", "wa.zenmind.cc"),
		AdminUsername:            env("ADMIN_USERNAME", env("BOOTSTRAP_ADMIN_USERNAME", "admin")),
		AdminPassword:            env("ADMIN_PASSWORD", os.Getenv("BOOTSTRAP_ADMIN_PASSWORD")),
		AdminSessionTTL:          envDuration("ADMIN_SESSION_TTL", 24*time.Hour),
		CookieSecure:             envBool("COOKIE_SECURE", false),
		MobileWebAppCookieSecure: envBool("MOBILE_WEBAPP_COOKIE_SECURE", true),
		SSOJWTIssuer:             env("SSO_JWT_ISSUER", ""),
		SSOJWTPublicKeyFile:      env("SSO_JWT_PUBLIC_KEY_FILE", ""),
		SSOJWTPublicKeyPEM:       env("SSO_JWT_PUBLIC_KEY_PEM", ""),
		SSOJWTAudience:           env("SSO_JWT_AUDIENCE", "zenmind-tunnel-hub-server"),
		MaxRequestBodyBytes:      envInt64("MAX_REQUEST_BODY_BYTES", 64<<20),
		TrustedProxyCIDRs:        env("TRUSTED_PROXY_CIDRS", ""),
	}
}

func LoadAgentConfig() AgentConfig {
	loadDotEnv()

	return AgentConfig{
		RelayURL:           env("AGENT_RELAY_URL", "ws://127.0.0.1:8080/tunnel"),
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
