package config

import "testing"

func TestLoadRelayConfigSupportsLegacyBootstrapAdminEnv(t *testing.T) {
	t.Setenv("ADMIN_USERNAME", "")
	t.Setenv("ADMIN_PASSWORD", "")
	t.Setenv("BOOTSTRAP_ADMIN_USERNAME", "legacy-admin")
	t.Setenv("BOOTSTRAP_ADMIN_PASSWORD", "legacy-secret")

	cfg := LoadRelayConfig()
	if cfg.AdminUsername != "legacy-admin" {
		t.Fatalf("AdminUsername = %q, want legacy-admin", cfg.AdminUsername)
	}
	if cfg.AdminPassword != "legacy-secret" {
		t.Fatalf("AdminPassword was not loaded from legacy env")
	}
}

func TestLoadRelayConfigSupportsDesktopPublicBaseDomain(t *testing.T) {
	t.Setenv("DESKTOP_PUBLIC_BASE_DOMAIN", "m.example.test")

	cfg := LoadRelayConfig()
	if cfg.DesktopPublicBaseDomain != "m.example.test" {
		t.Fatalf("DesktopPublicBaseDomain = %q, want m.example.test", cfg.DesktopPublicBaseDomain)
	}
}
