package config

import (
	"os"
	"path/filepath"
	"testing"
)

const validBrandYAML = `schemaVersion: 1
brand:
  id: example
  productName: Example Desktop
  publicSiteTitle: Example Desktop
domains:
  publicBase: hub.example.test
  desktopPublicBase: m.example.test
  webAppPublicBase: wa.example.test
endpoints:
  relayPublicUrl: wss://hub.example.test/tunnel
  sharePublicBaseUrl: https://share.example.test
`

func useTestBrandConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "brand.yaml")
	if err := os.WriteFile(path, []byte(validBrandYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRAND_CONFIG_FILE", path)
	t.Setenv("RELAY_ADDR", ":18081")
	t.Setenv("RELAY_DB_PATH", ":memory:")
	t.Setenv("SSO_JWT_ISSUER", "https://issuer.example.test")
	t.Setenv("SSO_JWT_PUBLIC_KEY_FILE", "test-public.pem")
	t.Setenv("SSO_JWT_AUDIENCE", "tunnel")
	t.Setenv("SSO_JWT_USER_ID_CLAIM", "sub")
	return path
}

func TestLoadRelayConfigSupportsLegacyBootstrapAdminEnv(t *testing.T) {
	useTestBrandConfig(t)
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

func TestLoadRelayConfigUsesBrandFile(t *testing.T) {
	useTestBrandConfig(t)

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
	if cfg.DesktopPublicBaseDomain != "m.example.test" || cfg.WebAppPublicBaseDomain != "wa.example.test" {
		t.Fatalf("brand domains were not loaded: %+v", cfg)
	}
}

func TestLoadRelayConfigSupportsTrustedProxyCIDRs(t *testing.T) {
	useTestBrandConfig(t)
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
	useTestBrandConfig(t)
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
		"BRAND_CONFIG_FILE",
		"RELAY_ADDR",
		"RELAY_DB_PATH",
		"SSO_JWT_ISSUER",
		"SSO_JWT_AUDIENCE",
		"SSO_JWT_USER_ID_CLAIM",
	} {
		t.Run(key, func(t *testing.T) {
			useTestBrandConfig(t)
			t.Setenv(key, "")
			if _, err := LoadRelayConfigStrict(); err == nil || err.Error() != key+" is required" {
				t.Fatalf("error = %v", err)
			}
		})
	}

	t.Run("SSO public key", func(t *testing.T) {
		useTestBrandConfig(t)
		t.Setenv("SSO_JWT_PUBLIC_KEY_FILE", "")
		t.Setenv("SSO_JWT_PUBLIC_KEY_PEM", "")
		if _, err := LoadRelayConfigStrict(); err == nil || err.Error() != "SSO_JWT_PUBLIC_KEY_FILE or SSO_JWT_PUBLIC_KEY_PEM is required" {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestLoadRelayConfigRejectsInvalidRelayAddress(t *testing.T) {
	useTestBrandConfig(t)
	t.Setenv("RELAY_ADDR", "localhost:not-a-port")
	if _, err := LoadRelayConfigStrict(); err == nil || err.Error() != "RELAY_ADDR is invalid" {
		t.Fatalf("error = %v", err)
	}
}
