package admin

import (
	"testing"

	"example.invalid/tunnel-hub-server/internal/config"
)

func TestServicePublicHostDoesNotUseABrandFallback(t *testing.T) {
	server := &Server{Config: config.RelayConfig{}}
	if got := server.servicePublicHost("auditor"); got != "auditor." {
		t.Fatalf("servicePublicHost = %q, want no default domain", got)
	}
}
