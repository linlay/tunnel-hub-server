package config

import (
	"strings"
	"testing"
)

func TestMySQLConfigDefaultsAndPassword(t *testing.T) {
	useTestRelayConfig(t)
	password := "  p@ss:/?#&=word  "
	t.Setenv("MYSQL_PASSWORD", password)
	cfg, err := LoadRelayConfigStrict()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MySQL.Password != password {
		t.Fatal("password was modified")
	}
	if cfg.MySQL.Port != 3306 || !cfg.MySQL.TLS || cfg.MySQL.MaxOpenConns != 10 || cfg.MySQL.MaxIdleConns != 5 {
		t.Fatal("unexpected MySQL defaults")
	}
}

func TestMySQLConfigRejectsInvalidSettings(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{"MYSQL_PORT", "0"}, {"MYSQL_PORT", "65536"}, {"MYSQL_PORT", "bad"},
		{"MYSQL_TLS", "sometimes"}, {"MYSQL_MAX_OPEN_CONNS", "0"},
		{"MYSQL_MAX_IDLE_CONNS", "-1"}, {"MYSQL_MAX_IDLE_CONNS", "11"},
		{"MYSQL_MAX_OPEN_CONNS", "1.5"}, {"MYSQL_MAX_IDLE_CONNS", "bad"},
	} {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			useTestRelayConfig(t)
			t.Setenv(tc.key, tc.value)
			if _, err := LoadRelayConfigStrict(); err == nil || !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	t.Run("CA requires TLS", func(t *testing.T) {
		useTestRelayConfig(t)
		t.Setenv("MYSQL_TLS", "false")
		t.Setenv("MYSQL_TLS_CA_FILE", "ca.pem")
		if _, err := LoadRelayConfigStrict(); err == nil {
			t.Fatal("expected CA/TLS configuration error")
		}
	})
}

func TestAgentDoesNotRequireMySQL(t *testing.T) {
	for _, key := range []string{"MYSQL_HOST", "MYSQL_USER", "MYSQL_PASSWORD", "MYSQL_DATABASE"} {
		t.Setenv(key, "")
	}
	t.Setenv("AGENT_TOKEN", "test-token")
	if cfg := LoadAgentConfig(); cfg.Token != "test-token" {
		t.Fatal("agent config failed")
	}
}
