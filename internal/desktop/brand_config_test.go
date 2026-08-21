package desktop

import (
	"testing"

	"github.com/linlay/zenmind-tunnel-server/internal/config"
)

func TestDesktopDomainsDoNotUseBrandFallbacks(t *testing.T) {
	server := &Server{Config: config.RelayConfig{}}
	if server.baseDomain() != "" || server.desktopPublicBaseDomain() != "" || server.webAppPublicBaseDomain() != "" {
		t.Fatalf("empty validated config should not acquire fallback domains")
	}
}
