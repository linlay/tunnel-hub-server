package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"example.invalid/tunnel-hub-server/internal/config"
	"github.com/go-sql-driver/mysql"
)

func connectionTestConfig() config.MySQLConfig {
	return config.MySQLConfig{Host: "localhost", Port: 3306, Database: "test", User: "test", Password: "test", TLS: true, MaxOpenConns: 10, MaxIdleConns: 5}
}

func TestMySQLConnectionConfiguration(t *testing.T) {
	cfg := connectionTestConfig()
	cfg.Password = "  p@ss:/?#&=word  "
	driver, err := mysqlConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := mysql.ParseDSN(driver.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Passwd != cfg.Password {
		t.Fatal("password changed in DSN")
	}
	if driver.TLS == nil || driver.TLS.InsecureSkipVerify || driver.TLS.ServerName != cfg.Host {
		t.Fatal("TLS verification is not enabled")
	}
	if !driver.ParseTime || driver.Loc != time.UTC || !driver.ClientFoundRows || driver.MultiStatements {
		t.Fatal("incorrect driver semantics")
	}
	if driver.Timeout != 5*time.Second || driver.ReadTimeout != 30*time.Second || driver.WriteTimeout != 30*time.Second {
		t.Fatal("incorrect timeouts")
	}
	if driver.Collation != "utf8mb4_bin" || driver.Params["time_zone"] != "'+00:00'" || driver.Params["sql_mode"] != "'STRICT_TRANS_TABLES,NO_ENGINE_SUBSTITUTION'" || driver.MaxAllowedPacket != 0 {
		t.Fatal("incorrect MySQL session configuration")
	}
	if !strings.Contains(driver.FormatDSN(), "timeTruncate=1µs") {
		t.Fatalf("microsecond truncation missing from DSN: %s", driver.FormatDSN())
	}
	cfg.TLS = false
	driver, err = mysqlConfig(cfg)
	if err != nil || driver.TLS != nil {
		t.Fatal("explicit plaintext configuration failed")
	}
}

func TestMySQLDialectBehaviorRemainsSelectedByDefault(t *testing.T) {
	db := &DB{}
	if db.forUpdateClause() != " FOR UPDATE" || db.castID("id") != "CAST(id AS CHAR)" || !strings.Contains(db.trafficSearchClause(), "utf8mb4_0900_as_ci") {
		t.Fatal("default database behavior is not MySQL")
	}
	duplicate := fmt.Errorf("wrapped: %w", &mysql.MySQLError{Number: 1062, Message: "duplicate"})
	if !db.isDuplicateKey(duplicate) {
		t.Fatal("wrapped MySQL duplicate key was not recognized")
	}
	foreignKey := &mysql.MySQLError{Number: 1452, Message: "foreign key"}
	if db.isDuplicateKey(foreignKey) {
		t.Fatal("MySQL foreign-key error was classified as duplicate key")
	}
}

func TestMySQLInvalidCA(t *testing.T) {
	cfg := connectionTestConfig()
	cfg.TLSCAFile = filepath.Join(t.TempDir(), "ca.pem")
	if _, err := mysqlConfig(cfg); err == nil {
		t.Fatal("missing CA accepted")
	}
	if err := os.WriteFile(cfg.TLSCAFile, []byte("not a certificate"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := mysqlConfig(cfg); err == nil {
		t.Fatal("invalid CA accepted")
	}
}

func TestMySQLServerRequirements(t *testing.T) {
	for _, tc := range []struct {
		version string
		packet  int64
		valid   bool
	}{
		{"8.0.16", 64 << 20, true}, {"8.0.45-commercial", 64 << 20, true}, {"8.4.0", 64 << 20, true},
		{"8.0.15", 64 << 20, false}, {"5.7.44", 64 << 20, false}, {"10.11.0-MariaDB", 64 << 20, false},
		{"bad", 64 << 20, false}, {"8.0.45", 32 << 20, false},
	} {
		if err := validateMySQLServer(tc.version, tc.packet); (err == nil) != tc.valid {
			t.Fatalf("version %s packet %d: %v", tc.version, tc.packet, err)
		}
	}
}

func TestMySQLCanceledConnectionDoesNotExposePassword(t *testing.T) {
	cfg := connectionTestConfig()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cfg.Password = "unique-secret-do-not-log"
	db, err := Open(ctx, cfg)
	if err == nil {
		db.Close()
		t.Fatal("canceled connection succeeded")
	}
	if strings.Contains(err.Error(), cfg.Password) {
		t.Fatal("password leaked")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}

func TestValidateTextLength(t *testing.T) {
	for _, value := range []string{strings.Repeat("A", 255), strings.Repeat("中", 255), "é", "e\u0301"} {
		if err := ValidateTextLength("id", value, 255); err != nil {
			t.Fatal(err)
		}
	}
	for _, value := range []string{strings.Repeat("中", 256), string([]byte{0xff})} {
		if err := ValidateTextLength("id", value, 255); err == nil {
			t.Fatal("invalid text accepted")
		}
	}
}
