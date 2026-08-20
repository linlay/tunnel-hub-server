package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadBrandConfigUsesSharedValidFixture(t *testing.T) {
	cfg, err := LoadBrandConfig(filepath.Join("..", "..", "configs", "testdata", "brand.valid.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Brand.ID != "fixture-brand" || cfg.Endpoints.RelayPublicURL != "wss://hub.fixture.example.test/tunnel" {
		t.Fatalf("unexpected brand config: %+v", cfg)
	}
}

func TestLoadBrandConfigRejectsSharedInvalidFixtures(t *testing.T) {
	for _, name := range []string{"brand.invalid-unknown.yaml", "brand.invalid-domain.yaml"} {
		t.Run(name, func(t *testing.T) {
			_, err := LoadBrandConfig(filepath.Join("..", "..", "configs", "testdata", name))
			if err == nil {
				t.Fatal("expected invalid fixture to fail")
			}
		})
	}
}

func TestLoadBrandConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		replace string
		with    string
		wantErr string
	}{
		{name: "empty id", replace: "id: example", with: `id: ""`, wantErr: "brand.id"},
		{name: "invalid id", replace: "id: example", with: "id: Example", wantErr: "brand.id"},
		{name: "empty product", replace: "productName: Example Desktop", with: `productName: ""`, wantErr: "productName"},
		{name: "empty title", replace: "publicSiteTitle: Example Desktop", with: `publicSiteTitle: ""`, wantErr: "publicSiteTitle"},
		{name: "duplicate domains", replace: "webAppPublicBase: wa.example.test", with: "webAppPublicBase: m.example.test", wantErr: "must be different"},
		{name: "wildcard domain", replace: "publicBase: hub.example.test", with: "publicBase: '*.example.test'", wantErr: "hostname"},
		{name: "empty relay", replace: "relayPublicUrl: wss://hub.example.test/tunnel", with: `relayPublicUrl: ""`, wantErr: "must not be empty"},
		{name: "remote ws", replace: "relayPublicUrl: wss://hub.example.test/tunnel", with: "relayPublicUrl: ws://hub.example.test/tunnel", wantErr: "must use wss"},
		{name: "relay path", replace: "relayPublicUrl: wss://hub.example.test/tunnel", with: "relayPublicUrl: wss://hub.example.test/other", wantErr: "path /tunnel"},
		{name: "relay wildcard", replace: "relayPublicUrl: wss://hub.example.test/tunnel", with: "relayPublicUrl: wss://*.example.test/tunnel", wantErr: "valid hostname"},
		{name: "empty share", replace: "sharePublicBaseUrl: https://share.example.test", with: `sharePublicBaseUrl: ""`, wantErr: "must not be empty"},
		{name: "remote share http", replace: "sharePublicBaseUrl: https://share.example.test", with: "sharePublicBaseUrl: http://share.example.test", wantErr: "must use https"},
		{name: "share path", replace: "sharePublicBaseUrl: https://share.example.test", with: "sharePublicBaseUrl: https://share.example.test/path", wantErr: "must be an origin"},
		{name: "share wildcard", replace: "sharePublicBaseUrl: https://share.example.test", with: "sharePublicBaseUrl: https://*.example.test", wantErr: "valid hostname"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeBrandFixture(t, strings.Replace(validBrandYAML, test.replace, test.with, 1))
			_, err := LoadBrandConfig(path)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestLoadBrandConfigAcceptsExplicitLoopbackEndpoints(t *testing.T) {
	for _, test := range []struct {
		name      string
		host      string
		shareHost string
	}{
		{name: "localhost", host: "localhost", shareHost: "localhost"},
		{name: "ipv4", host: "127.0.0.1", shareHost: "127.0.0.1"},
		{name: "ipv6", host: "[::1]", shareHost: "[::1]"},
	} {
		t.Run(test.name, func(t *testing.T) {
			yaml := strings.Replace(validBrandYAML, "relayPublicUrl: wss://hub.example.test/tunnel", "relayPublicUrl: ws://"+test.host+":18181/tunnel", 1)
			yaml = strings.Replace(yaml, "sharePublicBaseUrl: https://share.example.test", "sharePublicBaseUrl: http://"+test.shareHost+":18080/", 1)
			cfg, err := LoadBrandConfig(writeBrandFixture(t, yaml))
			if err != nil {
				t.Fatal(err)
			}
			if want := "http://" + test.shareHost + ":18080"; cfg.Endpoints.SharePublicBaseURL != want {
				t.Fatalf("SharePublicBaseURL = %q, want %q", cfg.Endpoints.SharePublicBaseURL, want)
			}
		})
	}
}

func TestLoadBrandConfigRejectsReservedNonCanonicalLocalEndpoints(t *testing.T) {
	for _, host := range []string{"127.0.0.2", "demo.localhost", "0.0.0.0"} {
		t.Run("relay_"+host, func(t *testing.T) {
			yaml := strings.Replace(validBrandYAML, "relayPublicUrl: wss://hub.example.test/tunnel", "relayPublicUrl: wss://"+host+":18181/tunnel", 1)
			if _, err := LoadBrandConfig(writeBrandFixture(t, yaml)); err == nil {
				t.Fatalf("reserved local relay host accepted: %s", host)
			}
		})
		t.Run("share_"+host, func(t *testing.T) {
			yaml := strings.Replace(validBrandYAML, "sharePublicBaseUrl: https://share.example.test", "sharePublicBaseUrl: https://"+host+":18080", 1)
			if _, err := LoadBrandConfig(writeBrandFixture(t, yaml)); err == nil {
				t.Fatalf("reserved local share host accepted: %s", host)
			}
		})
	}
}

func TestLoadBrandConfigRejectsMultipleDocuments(t *testing.T) {
	_, err := LoadBrandConfig(writeBrandFixture(t, validBrandYAML+"---\n{}\n"))
	if err == nil || !strings.Contains(err.Error(), "exactly one YAML document") {
		t.Fatalf("error = %v", err)
	}
}

func writeBrandFixture(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "brand.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
