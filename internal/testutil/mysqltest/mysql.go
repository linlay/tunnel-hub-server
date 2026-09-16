// Package mysqltest provisions isolated databases on an explicitly configured
// test server. It never reads MYSQL_* or connects to the application database.
package mysqltest

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	"example.invalid/tunnel-hub-server/internal/config"
	"github.com/go-sql-driver/mysql"
)

func NewConfig(t *testing.T) config.MySQLConfig {
	t.Helper()
	cfg := config.MySQLConfig{
		Host: required(t, "TEST_MYSQL_HOST"), User: required(t, "TEST_MYSQL_USER"),
		Password: required(t, "TEST_MYSQL_PASSWORD"), Port: 3306,
		MaxOpenConns: 10, MaxIdleConns: 5,
		TLSCAFile: os.Getenv("TEST_MYSQL_TLS_CA_FILE"),
	}
	if value := os.Getenv("TEST_MYSQL_PORT"); value != "" {
		port, err := strconv.Atoi(value)
		if err != nil {
			t.Fatal("TEST_MYSQL_PORT must be an integer")
		}
		cfg.Port = port
	}
	if value := os.Getenv("TEST_MYSQL_TLS"); value != "" {
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			t.Fatal("TEST_MYSQL_TLS must be a boolean")
		}
		cfg.TLS = enabled
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	cfg.Database = "tunnel_test_" + hex.EncodeToString(random[:])
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	driver := mysql.NewConfig()
	driver.Net, driver.Addr = "tcp", net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	driver.User, driver.Passwd = cfg.User, cfg.Password
	driver.Timeout, driver.ReadTimeout, driver.WriteTimeout = 5*time.Second, 10*time.Second, 10*time.Second
	if cfg.TLS {
		driver.TLS = &tls.Config{MinVersion: tls.VersionTLS12, ServerName: cfg.Host}
		if cfg.TLSCAFile != "" {
			pem, err := os.ReadFile(cfg.TLSCAFile)
			if err != nil {
				t.Fatal(err)
			}
			roots, err := x509.SystemCertPool()
			if err != nil {
				roots = x509.NewCertPool()
			}
			if !roots.AppendCertsFromPEM(pem) {
				t.Fatal("TEST_MYSQL_TLS_CA_FILE contains no valid certificates")
			}
			driver.TLS.RootCAs = roots
		}
	}
	connector, err := mysql.NewConnector(driver)
	if err != nil {
		t.Fatal(err)
	}
	admin := sql.OpenDB(connector)
	admin.SetMaxOpenConns(1)
	// Only this cryptographically generated identifier can be created or dropped.
	cleanupDatabase := false
	t.Cleanup(func() {
		defer admin.Close()
		if !cleanupDatabase {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.ExecContext(ctx, "DROP DATABASE IF EXISTS `"+cfg.Database+"`"); err != nil {
			t.Errorf("cleanup failed; remove test database %s manually: %v", cfg.Database, err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE `"+cfg.Database+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_bin"); err != nil {
		// A lost connection may conceal a successful CREATE. Clean up that case,
		// but never drop a pre-existing database after a server-side rejection.
		var serverError *mysql.MySQLError
		cleanupDatabase = !errors.As(err, &serverError)
		t.Fatalf("create isolated MySQL test database: %v", err)
	}
	cleanupDatabase = true
	return cfg
}

func required(t *testing.T, key string) string {
	t.Helper()
	value := os.Getenv(key)
	if value == "" {
		t.Fatalf("%s is required; database tests require a dedicated MySQL 8.0 server", key)
	}
	return value
}
