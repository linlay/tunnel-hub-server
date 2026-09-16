package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// MySQLConfig is the complete connection configuration for the Relay store.
type MySQLConfig struct {
	Host         string
	Port         int
	Database     string
	User         string
	Password     string
	TLS          bool
	TLSCAFile    string
	MaxOpenConns int
	MaxIdleConns int
}

func loadMySQLConfig() (MySQLConfig, error) {
	cfg := MySQLConfig{Port: 3306, TLS: true, MaxOpenConns: 10, MaxIdleConns: 5}
	for _, field := range []struct {
		key string
		dst *string
	}{
		{"MYSQL_HOST", &cfg.Host}, {"MYSQL_DATABASE", &cfg.Database}, {"MYSQL_USER", &cfg.User},
	} {
		value, err := requiredEnv(field.key)
		if err != nil {
			return MySQLConfig{}, err
		}
		*field.dst = value
	}
	cfg.Password = os.Getenv("MYSQL_PASSWORD")
	if cfg.Password == "" {
		return MySQLConfig{}, fmt.Errorf("MYSQL_PASSWORD is required")
	}
	for _, field := range []struct {
		key string
		dst *int
	}{
		{"MYSQL_PORT", &cfg.Port}, {"MYSQL_MAX_OPEN_CONNS", &cfg.MaxOpenConns}, {"MYSQL_MAX_IDLE_CONNS", &cfg.MaxIdleConns},
	} {
		if value := os.Getenv(field.key); value != "" {
			parsed, err := strconv.Atoi(value)
			if err != nil {
				return MySQLConfig{}, fmt.Errorf("%s must be an integer", field.key)
			}
			*field.dst = parsed
		}
	}
	if value := os.Getenv("MYSQL_TLS"); value != "" {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return MySQLConfig{}, fmt.Errorf("MYSQL_TLS must be a boolean")
		}
		cfg.TLS = parsed
	}
	cfg.TLSCAFile = strings.TrimSpace(os.Getenv("MYSQL_TLS_CA_FILE"))
	return cfg, cfg.Validate()
}

func (cfg MySQLConfig) Validate() error {
	for _, field := range []struct{ key, value string }{
		{"MYSQL_HOST", cfg.Host}, {"MYSQL_DATABASE", cfg.Database}, {"MYSQL_USER", cfg.User}, {"MYSQL_PASSWORD", cfg.Password},
	} {
		if field.value == "" {
			return fmt.Errorf("%s is required", field.key)
		}
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		return fmt.Errorf("MYSQL_PORT must be between 1 and 65535")
	}
	if cfg.MaxOpenConns <= 0 {
		return fmt.Errorf("MYSQL_MAX_OPEN_CONNS must be positive")
	}
	if cfg.MaxIdleConns < 0 || cfg.MaxIdleConns > cfg.MaxOpenConns {
		return fmt.Errorf("MYSQL_MAX_IDLE_CONNS must be between 0 and MYSQL_MAX_OPEN_CONNS")
	}
	if cfg.TLSCAFile != "" && !cfg.TLS {
		return fmt.Errorf("MYSQL_TLS_CA_FILE requires MYSQL_TLS=true")
	}
	return nil
}
