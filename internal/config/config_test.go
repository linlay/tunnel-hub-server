package config

import "testing"

func setValidBrandEnv(t *testing.T) {
	t.Helper()
	t.Setenv("BRAND_ID", "example")
	t.Setenv("PRODUCT_NAME", "Example Desktop")
	t.Setenv("PUBLIC_SITE_TITLE", "Example Desktop")
	t.Setenv("PUBLIC_BASE_DOMAIN", "hub.example.test")
	t.Setenv("DESKTOP_PUBLIC_BASE_DOMAIN", "m.example.test")
	t.Setenv("WEBAPP_PUBLIC_BASE_DOMAIN", "example.test")
	t.Setenv("RELAY_PUBLIC_URL", "wss://hub.example.test/tunnel")
	t.Setenv("SHARE_PUBLIC_BASE_URL", "https://share.example.test")
}

func useTestRelayConfig(t *testing.T) {
	t.Helper()
	setValidBrandEnv(t)
	t.Setenv("RELAY_ADDR", ":18081")
	t.Setenv("DATABASE_TYPE", "")
	t.Setenv("RELAY_DB_PATH", "")
	t.Setenv("MYSQL_HOST", "127.0.0.1")
	t.Setenv("MYSQL_DATABASE", "tunnel_test")
	t.Setenv("MYSQL_USER", "tunnel_test")
	t.Setenv("MYSQL_PASSWORD", "test-password")
	for _, key := range []string{"MYSQL_PORT", "MYSQL_TLS", "MYSQL_TLS_CA_FILE", "MYSQL_MAX_OPEN_CONNS", "MYSQL_MAX_IDLE_CONNS"} {
		t.Setenv(key, "")
	}
	t.Setenv("SSO_JWT_ISSUER", "https://issuer.example.test")
	t.Setenv("SSO_JWT_PUBLIC_KEY_FILE", "test-public.pem")
	t.Setenv("SSO_JWT_AUDIENCE", "tunnel")
	t.Setenv("SSO_JWT_USER_ID_CLAIM", "sub")
}

func TestLoadRelayConfigSelectsDatabase(t *testing.T) {
	t.Run("MySQL remains the default", func(t *testing.T) {
		useTestRelayConfig(t)
		t.Setenv("RELAY_DB_PATH", "/unrelated/relay.sqlite")
		cfg, err := LoadRelayConfigStrict()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.DatabaseType != DatabaseMySQL || cfg.SQLitePath != "" || cfg.MySQL.Host != "127.0.0.1" {
			t.Fatalf("database config = %+v", cfg)
		}
	})

	t.Run("SQLite does not load MySQL settings", func(t *testing.T) {
		useTestRelayConfig(t)
		t.Setenv("DATABASE_TYPE", "sqlite")
		t.Setenv("RELAY_DB_PATH", "data/relay.sqlite")
		for _, key := range []string{"MYSQL_HOST", "MYSQL_DATABASE", "MYSQL_USER", "MYSQL_PASSWORD"} {
			t.Setenv(key, "")
		}
		cfg, err := LoadRelayConfigStrict()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.DatabaseType != DatabaseSQLite || cfg.SQLitePath != "data/relay.sqlite" || cfg.MySQL != (MySQLConfig{}) {
			t.Fatalf("database config = %+v", cfg)
		}
	})

	t.Run("SQLite path is required", func(t *testing.T) {
		useTestRelayConfig(t)
		t.Setenv("DATABASE_TYPE", "sqlite")
		if _, err := LoadRelayConfigStrict(); err == nil || err.Error() != "RELAY_DB_PATH is required" {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("unknown database type is rejected", func(t *testing.T) {
		useTestRelayConfig(t)
		t.Setenv("DATABASE_TYPE", "postgres")
		if _, err := LoadRelayConfigStrict(); err == nil || err.Error() != "DATABASE_TYPE must be mysql or sqlite" {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestLoadRelayConfigSupportsLegacyBootstrapAdminEnv(t *testing.T) {
	useTestRelayConfig(t)
	t.Setenv("ADMIN_USERNAME", "")
	t.Setenv("ADMIN_PASSWORD", "")
	t.Setenv("BOOTSTRAP_ADMIN_USERNAME", "legacy-admin")
	t.Setenv("BOOTSTRAP_ADMIN_PASSWORD", "legacy-secret")

	cfg, err := LoadRelayConfigStrict()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AdminUsername != "legacy-admin" {
		t.Fatalf("AdminUsername = %q, want legacy-admin", cfg.AdminUsername)
	}
	if cfg.AdminPassword != "legacy-secret" {
		t.Fatalf("AdminPassword was not loaded from legacy env")
	}
}

func TestLoadRelayConfigUsesBrandEnvironment(t *testing.T) {
	useTestRelayConfig(t)

	cfg, err := LoadRelayConfigStrict()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BrandID != "example" || cfg.ProductName != "Example Desktop" || cfg.PublicSiteTitle != "Example Desktop" {
		t.Fatalf("brand identity was not loaded: %+v", cfg)
	}
	if cfg.RelayPublicURL != "wss://hub.example.test/tunnel" {
		t.Fatalf("RelayPublicURL = %q", cfg.RelayPublicURL)
	}
	if cfg.DesktopPublicBaseDomain != "m.example.test" || cfg.WebAppPublicBaseDomain != "example.test" {
		t.Fatalf("brand domains were not loaded: %+v", cfg)
	}
}

func TestLoadRelayConfigSupportsTrustedProxyCIDRs(t *testing.T) {
	useTestRelayConfig(t)
	t.Setenv("TRUSTED_PROXY_CIDRS", "172.23.0.1/32,127.0.0.1/32,::1/128")

	cfg, err := LoadRelayConfigStrict()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TrustedProxyCIDRs != "172.23.0.1/32,127.0.0.1/32,::1/128" {
		t.Fatalf("TrustedProxyCIDRs = %q", cfg.TrustedProxyCIDRs)
	}
}

func TestLoadRelayConfigSupportsRelaxedSSOCompatibility(t *testing.T) {
	useTestRelayConfig(t)
	t.Setenv("SSO_JWT_USER_ID_CLAIM", "userId")
	t.Setenv("SSO_JWT_ALLOW_ANY_AUDIENCE", "true")
	t.Setenv("SSO_JWT_ALLOW_ANY_ADMIN_ROLE", "true")
	t.Setenv("SSO_JWT_ALLOW_MISSING_TUNNEL_SCOPE", "true")

	cfg, err := LoadRelayConfigStrict()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SSOJWTUserIDClaim != "userId" || !cfg.SSOJWTAllowAnyAudience || !cfg.SSOJWTAllowAnyAdminRole || !cfg.SSOJWTAllowMissingScope {
		t.Fatalf("relaxed SSO config was not loaded: %+v", cfg)
	}
}

func TestLoadRelayConfigRequiresEnvironmentSpecificValues(t *testing.T) {
	for _, key := range []string{
		"BRAND_ID",
		"PRODUCT_NAME",
		"PUBLIC_SITE_TITLE",
		"PUBLIC_BASE_DOMAIN",
		"DESKTOP_PUBLIC_BASE_DOMAIN",
		"WEBAPP_PUBLIC_BASE_DOMAIN",
		"RELAY_PUBLIC_URL",
		"SHARE_PUBLIC_BASE_URL",
		"RELAY_ADDR",
		"MYSQL_HOST", "MYSQL_DATABASE", "MYSQL_USER", "MYSQL_PASSWORD",
		"SSO_JWT_ISSUER",
		"SSO_JWT_AUDIENCE",
		"SSO_JWT_USER_ID_CLAIM",
	} {
		t.Run(key, func(t *testing.T) {
			useTestRelayConfig(t)
			t.Setenv(key, "")
			if _, err := LoadRelayConfigStrict(); err == nil || err.Error() != key+" is required" {
				t.Fatalf("error = %v", err)
			}
		})
	}

	t.Run("SSO public key", func(t *testing.T) {
		useTestRelayConfig(t)
		t.Setenv("SSO_JWT_PUBLIC_KEY_FILE", "")
		t.Setenv("SSO_JWT_PUBLIC_KEY_PEM", "")
		if _, err := LoadRelayConfigStrict(); err == nil || err.Error() != "SSO_JWT_PUBLIC_KEY_FILE or SSO_JWT_PUBLIC_KEY_PEM is required" {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestLoadRelayConfigRejectsInvalidRelayAddress(t *testing.T) {
	useTestRelayConfig(t)
	t.Setenv("RELAY_ADDR", "localhost:not-a-port")
	if _, err := LoadRelayConfigStrict(); err == nil || err.Error() != "RELAY_ADDR is invalid" {
		t.Fatalf("error = %v", err)
	}
}
