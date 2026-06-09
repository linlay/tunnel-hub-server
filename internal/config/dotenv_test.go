package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDotEnvFileMissing(t *testing.T) {
	t.Setenv("DOTENV_MISSING_VALUE", "")
	if err := loadDotEnvFile(filepath.Join(t.TempDir(), ".env")); err != nil {
		t.Fatalf("load missing .env: %v", err)
	}
	if got := os.Getenv("DOTENV_MISSING_VALUE"); got != "" {
		t.Fatalf("missing .env changed environment: %q", got)
	}
}

func TestLoadDotEnvFileParsesSupportedSyntax(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := `
# comment
RELAY_ADDR=:9090
export PUBLIC_BASE_DOMAIN="local.test"
COOKIE_SECRET='dev secret'
EMPTY_VALUE=
MAX_REQUEST_BODY_BYTES=1024 # inline comment
INVALID LINE
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}

	for _, key := range []string{"RELAY_ADDR", "PUBLIC_BASE_DOMAIN", "COOKIE_SECRET", "EMPTY_VALUE", "MAX_REQUEST_BODY_BYTES"} {
		t.Setenv(key, "")
		_ = os.Unsetenv(key)
	}

	if err := loadDotEnvFile(path); err != nil {
		t.Fatalf("load .env: %v", err)
	}

	assertEnv(t, "RELAY_ADDR", ":9090")
	assertEnv(t, "PUBLIC_BASE_DOMAIN", "local.test")
	assertEnv(t, "COOKIE_SECRET", "dev secret")
	assertEnv(t, "EMPTY_VALUE", "")
	assertEnv(t, "MAX_REQUEST_BODY_BYTES", "1024")
}

func TestLoadDotEnvFileDoesNotOverrideExistingEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("RELAY_ADDR=:9090\n"), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	t.Setenv("RELAY_ADDR", ":8081")

	if err := loadDotEnvFile(path); err != nil {
		t.Fatalf("load .env: %v", err)
	}

	assertEnv(t, "RELAY_ADDR", ":8081")
}

func assertEnv(t *testing.T, key, want string) {
	t.Helper()
	if got := os.Getenv(key); got != want {
		t.Fatalf("%s = %q, want %q", key, got, want)
	}
}
