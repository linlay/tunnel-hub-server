package proxy

import (
	"net/http"
	"testing"
)

func TestRelayClientRemoteAddr(t *testing.T) {
	tests := []struct {
		name         string
		trustedCIDRs string
		remoteAddr   string
		headers      http.Header
		want         string
	}{
		{
			name:         "no trusted proxy config ignores forwarded headers",
			remoteAddr:   "172.23.0.1:47908",
			trustedCIDRs: "",
			headers:      remoteAddrTestHeader("203.0.113.10", "198.51.100.1, 203.0.113.10"),
			want:         "172.23.0.1:47908",
		},
		{
			name:         "trusted proxy uses x real ip",
			remoteAddr:   "172.23.0.1:47908",
			trustedCIDRs: "172.23.0.1/32",
			headers:      remoteAddrTestHeader("203.0.113.10", "198.51.100.1, 203.0.113.11"),
			want:         "203.0.113.10",
		},
		{
			name:         "trusted proxy uses last valid x forwarded for ip",
			remoteAddr:   "172.23.0.1:42154",
			trustedCIDRs: "172.23.0.1/32",
			headers:      remoteAddrTestHeader("not-an-ip", "198.51.100.77, unknown, 203.0.113.42"),
			want:         "203.0.113.42",
		},
		{
			name:         "untrusted remote ignores forwarded headers",
			remoteAddr:   "10.0.0.8:5000",
			trustedCIDRs: "172.23.0.1/32",
			headers:      remoteAddrTestHeader("203.0.113.10", ""),
			want:         "10.0.0.8:5000",
		},
		{
			name:         "trusted proxy falls back when headers are empty or invalid",
			remoteAddr:   "172.23.0.1:47908",
			trustedCIDRs: "172.23.0.1/32",
			headers:      remoteAddrTestHeader("", "unknown, also-bad"),
			want:         "172.23.0.1:47908",
		},
		{
			name:         "trusted ipv6 proxy parses bracketed remote address",
			remoteAddr:   "[::1]:47908",
			trustedCIDRs: "::1/128",
			headers:      remoteAddrTestHeader("2001:db8::1", ""),
			want:         "2001:db8::1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			relay := NewRelay(nil, nil, nil, 0)
			relay.SetTrustedProxyCIDRs(tc.trustedCIDRs)
			req := &http.Request{
				RemoteAddr: tc.remoteAddr,
				Header:     tc.headers,
			}
			if got := relay.clientRemoteAddr(req); got != tc.want {
				t.Fatalf("clientRemoteAddr() = %q, want %q", got, tc.want)
			}
		})
	}
}

func remoteAddrTestHeader(realIP, forwardedFor string) http.Header {
	header := make(http.Header)
	if realIP != "" {
		header.Set("X-Real-IP", realIP)
	}
	if forwardedFor != "" {
		header.Set("X-Forwarded-For", forwardedFor)
	}
	return header
}
