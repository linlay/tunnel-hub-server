package config

import (
	"strings"
	"testing"
)

func TestLoadBrandConfigFromEnv(t *testing.T) {
	setValidBrandEnv(t)
	cfg, err := LoadBrandConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Brand.ID != "example" || cfg.Endpoints.RelayPublicURL != "wss://hub.example.test/tunnel" {
		t.Fatalf("unexpected brand config: %+v", cfg)
	}
}

func TestLoadBrandConfigFromEnvValidation(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		value   string
		wantErr string
	}{
		{name: "empty id", key: "BRAND_ID", value: "", wantErr: "BRAND_ID is required"},
		{name: "invalid id", key: "BRAND_ID", value: "Example", wantErr: "BRAND_ID"},
		{name: "empty product", key: "PRODUCT_NAME", value: "", wantErr: "PRODUCT_NAME is required"},
		{name: "empty title", key: "PUBLIC_SITE_TITLE", value: "", wantErr: "PUBLIC_SITE_TITLE is required"},
		{name: "duplicate domains", key: "WEBAPP_PUBLIC_BASE_DOMAIN", value: "m.example.test", wantErr: "must be different"},
		{name: "wildcard domain", key: "PUBLIC_BASE_DOMAIN", value: "*.example.test", wantErr: "hostname"},
		{name: "empty relay", key: "RELAY_PUBLIC_URL", value: "", wantErr: "RELAY_PUBLIC_URL is required"},
		{name: "remote ws", key: "RELAY_PUBLIC_URL", value: "ws://hub.example.test/tunnel", wantErr: "must use wss"},
		{name: "relay path", key: "RELAY_PUBLIC_URL", value: "wss://hub.example.test/other", wantErr: "path /tunnel"},
		{name: "relay wildcard", key: "RELAY_PUBLIC_URL", value: "wss://*.example.test/tunnel", wantErr: "valid hostname"},
		{name: "empty share", key: "SHARE_PUBLIC_BASE_URL", value: "", wantErr: "SHARE_PUBLIC_BASE_URL is required"},
		{name: "remote share http", key: "SHARE_PUBLIC_BASE_URL", value: "http://share.example.test", wantErr: "must use https"},
		{name: "share path", key: "SHARE_PUBLIC_BASE_URL", value: "https://share.example.test/path", wantErr: "must be an origin"},
		{name: "share wildcard", key: "SHARE_PUBLIC_BASE_URL", value: "https://*.example.test", wantErr: "valid hostname"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			setValidBrandEnv(t)
			t.Setenv(test.key, test.value)
			_, err := LoadBrandConfigFromEnv()
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestLoadBrandConfigFromEnvAcceptsExplicitLoopbackEndpoints(t *testing.T) {
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
			setValidBrandEnv(t)
			t.Setenv("RELAY_PUBLIC_URL", "ws://"+test.host+":18181/tunnel")
			t.Setenv("SHARE_PUBLIC_BASE_URL", "http://"+test.shareHost+":18080/")
			cfg, err := LoadBrandConfigFromEnv()
			if err != nil {
				t.Fatal(err)
			}
			if want := "http://" + test.shareHost + ":18080"; cfg.Endpoints.SharePublicBaseURL != want {
				t.Fatalf("SharePublicBaseURL = %q, want %q", cfg.Endpoints.SharePublicBaseURL, want)
			}
		})
	}
}

func TestLoadBrandConfigFromEnvRejectsReservedNonCanonicalLocalEndpoints(t *testing.T) {
	for _, host := range []string{"127.0.0.2", "demo.localhost", "0.0.0.0"} {
		t.Run("relay_"+host, func(t *testing.T) {
			setValidBrandEnv(t)
			t.Setenv("RELAY_PUBLIC_URL", "wss://"+host+":18181/tunnel")
			if _, err := LoadBrandConfigFromEnv(); err == nil {
				t.Fatalf("reserved local relay host accepted: %s", host)
			}
		})
		t.Run("share_"+host, func(t *testing.T) {
			setValidBrandEnv(t)
			t.Setenv("SHARE_PUBLIC_BASE_URL", "https://"+host+":18080")
			if _, err := LoadBrandConfigFromEnv(); err == nil {
				t.Fatalf("reserved local share host accepted: %s", host)
			}
		})
	}
}
