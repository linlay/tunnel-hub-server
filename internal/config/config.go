package config

import (
	"crypto/rand"
	"encoding/base64"
	"os"
	"strconv"
	"time"
)

type RelayConfig struct {
	Addr                      string
	DatabasePath              string
	AdminHost                 string
	WebsiteDist               string
	PublicBaseDomain          string
	DesktopRegistrationSecret string
	CookieSecret              string
	BootstrapAdminUsername    string
	BootstrapAdminPassword    string
	MaxRequestBodyBytes       int64
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
		Addr:                      env("RELAY_ADDR", ":8080"),
		DatabasePath:              env("RELAY_DB_PATH", "zenmind-tunnel.db"),
		AdminHost:                 env("ADMIN_HOST", ""),
		WebsiteDist:               env("WEBSITE_DIST", ""),
		PublicBaseDomain:          env("PUBLIC_BASE_DOMAIN", "tunnel-hub.zenmind.cc"),
		DesktopRegistrationSecret: env("DESKTOP_REGISTRATION_SECRET", ""),
		CookieSecret:              env("COOKIE_SECRET", randomSecret()),
		BootstrapAdminUsername:    env("BOOTSTRAP_ADMIN_USERNAME", "admin"),
		BootstrapAdminPassword:    env("BOOTSTRAP_ADMIN_PASSWORD", "admin"),
		MaxRequestBodyBytes:       envInt64("MAX_REQUEST_BODY_BYTES", 64<<20),
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

func randomSecret() string {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "development-cookie-secret"
	}
	return base64.RawURLEncoding.EncodeToString(raw[:])
}
