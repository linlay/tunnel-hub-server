package proxy

import "testing"

func TestMobileWebAppSessionCookieNameUsesHostPrefixOnlyForSecureCookies(t *testing.T) {
	relay := &Relay{BrandID: "example", MobileWebAppCookieSecure: true}
	if got := relay.mobileWebAppSessionCookieName(); got != "__Host-example_mobile_session" {
		t.Fatalf("secure cookie name = %q", got)
	}
	relay.SetMobileWebAppCookieSecure(false)
	if got := relay.mobileWebAppSessionCookieName(); got != "example_mobile_session" {
		t.Fatalf("insecure cookie name = %q", got)
	}
}

func TestMobileWebAppSessionCookieNamePreservesCurrentBrandCompatibility(t *testing.T) {
	relay := &Relay{BrandID: "example", MobileWebAppCookieSecure: true}
	if got := relay.mobileWebAppSessionCookieName(); got != "__Host-example_mobile_session" {
		t.Fatalf("secure cookie name = %q", got)
	}
	relay.SetMobileWebAppCookieSecure(false)
	if got := relay.mobileWebAppSessionCookieName(); got != "example_mobile_session" {
		t.Fatalf("insecure cookie name = %q", got)
	}
}

func TestNewRelayKeepsValidatedBrandRoutingIsolated(t *testing.T) {
	alpha := NewRelay(nil, nil, nil, "alpha", "m.alpha.example.test", "alpha.example.test", 1)
	beta := NewRelay(nil, nil, nil, "beta", "m.beta.example.test", "beta.example.test", 1)
	if alpha.DesktopBaseDomain != "m.alpha.example.test" || alpha.WebAppBaseDomain != "alpha.example.test" {
		t.Fatalf("alpha domains = %q %q", alpha.DesktopBaseDomain, alpha.WebAppBaseDomain)
	}
	if beta.DesktopBaseDomain != "m.beta.example.test" || beta.WebAppBaseDomain != "beta.example.test" {
		t.Fatalf("beta domains = %q %q", beta.DesktopBaseDomain, beta.WebAppBaseDomain)
	}
	if alpha.mobileWebAppSessionCookieName() == beta.mobileWebAppSessionCookieName() {
		t.Fatalf("brand cookie names must be isolated")
	}
}

func TestMobileWebAppHost(t *testing.T) {
	tests := []struct {
		name       string
		host       string
		baseDomain string
		wantDevice string
		wantPort   int
		wantOK     bool
	}{
		{name: "generated host", host: "desktop-43210.m.example.test", baseDomain: "m.example.test", wantDevice: "desktop.m.example.test", wantPort: 43210, wantOK: true},
		{name: "host header with port", host: "desktop-43210.m.example.test:8443", baseDomain: "m.example.test", wantDevice: "desktop.m.example.test", wantPort: 43210, wantOK: true},
		{name: "hyphenated device", host: "my-desktop-43210.m.example.test", baseDomain: "m.example.test", wantDevice: "my-desktop.m.example.test", wantPort: 43210, wantOK: true},
		{name: "zero", host: "desktop-0.m.example.test", baseDomain: "m.example.test", wantOK: false},
		{name: "too large", host: "desktop-65536.m.example.test", baseDomain: "m.example.test", wantOK: false},
		{name: "not numeric", host: "desktop-dev.m.example.test", baseDomain: "m.example.test", wantOK: false},
		{name: "device host", host: "desktop.m.example.test", baseDomain: "m.example.test", wantOK: false},
		{name: "nested labels", host: "nested.desktop-43210.m.example.test", baseDomain: "m.example.test", wantOK: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			device, port, ok := mobileWebAppHost(test.host, test.baseDomain)
			if device != test.wantDevice || port != test.wantPort || ok != test.wantOK {
				t.Fatalf("mobileWebAppHost(%q, %q) = (%q, %d, %t), want (%q, %d, %t)", test.host, test.baseDomain, device, port, ok, test.wantDevice, test.wantPort, test.wantOK)
			}
		})
	}
}
