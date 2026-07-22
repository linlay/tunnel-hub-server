package proxy

import "testing"

func TestMobileWebAppSessionCookieNameUsesHostPrefixOnlyForSecureCookies(t *testing.T) {
	relay := &Relay{MobileWebAppCookieSecure: true}
	if got := relay.mobileWebAppSessionCookieName(); got != secureMobileWebAppSessionCookie {
		t.Fatalf("secure cookie name = %q", got)
	}
	relay.SetMobileWebAppCookieSecure(false)
	if got := relay.mobileWebAppSessionCookieName(); got != insecureMobileWebAppSessionCookie {
		t.Fatalf("insecure cookie name = %q", got)
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
