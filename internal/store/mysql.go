package store

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"example.invalid/tunnel-hub-server/internal/config"
	"github.com/go-sql-driver/mysql"
)

//go:embed schema.sql
var schema string

func mysqlConfig(cfg config.MySQLConfig) (*mysql.Config, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	driver := mysql.NewConfig()
	driver.Net = "tcp"
	driver.Addr = net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	driver.User, driver.Passwd, driver.DBName = cfg.User, cfg.Password, cfg.Database
	driver.ParseTime = true
	if err := driver.Apply(mysql.TimeTruncate(time.Microsecond), mysql.Charset("utf8mb4", "utf8mb4_bin")); err != nil {
		return nil, fmt.Errorf("configure MySQL encoding: %w", err)
	}
	driver.Loc = time.UTC
	driver.ClientFoundRows = true
	driver.Timeout = 5 * time.Second
	driver.ReadTimeout, driver.WriteTimeout = 30*time.Second, 30*time.Second
	driver.Params = map[string]string{"time_zone": "'+00:00'", "sql_mode": "'STRICT_TRANS_TABLES,NO_ENGINE_SUBSTITUTION'"}
	driver.MaxAllowedPacket = 0 // Use the server limit on each connection.
	if cfg.TLS {
		driver.TLS = &tls.Config{MinVersion: tls.VersionTLS12, ServerName: cfg.Host}
		if cfg.TLSCAFile != "" {
			pem, err := os.ReadFile(cfg.TLSCAFile)
			if err != nil {
				return nil, fmt.Errorf("read MYSQL_TLS_CA_FILE: %w", err)
			}
			roots, err := x509.SystemCertPool()
			if err != nil {
				roots = x509.NewCertPool()
			}
			if !roots.AppendCertsFromPEM(pem) {
				return nil, errors.New("MYSQL_TLS_CA_FILE contains no valid certificates")
			}
			driver.TLS.RootCAs = roots
		}
	}
	return driver, nil
}

func Open(ctx context.Context, cfg config.MySQLConfig) (*DB, error) {
	driver, err := mysqlConfig(cfg)
	if err != nil {
		return nil, err
	}
	connector, err := mysql.NewConnector(driver)
	if err != nil {
		return nil, fmt.Errorf("configure MySQL connector: %w", err)
	}
	pool := sql.OpenDB(connector)
	pool.SetMaxOpenConns(cfg.MaxOpenConns)
	pool.SetMaxIdleConns(cfg.MaxIdleConns)
	pool.SetConnMaxLifetime(3 * time.Minute)
	startup, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.PingContext(startup); err != nil {
		_ = pool.Close()
		return nil, fmt.Errorf("ping MySQL: %w", err)
	}
	var version string
	var packet int64
	if err := pool.QueryRowContext(startup, `SELECT VERSION(), @@max_allowed_packet`).Scan(&version, &packet); err != nil {
		_ = pool.Close()
		return nil, fmt.Errorf("check MySQL requirements: %w", err)
	}
	if err := validateMySQLServer(version, packet, cfg.MinPacketBytes); err != nil {
		_ = pool.Close()
		return nil, err
	}
	return &DB{sql: pool, database: databaseMySQL}, nil
}

func validateMySQLServer(version string, packet, minimumPacketBytes int64) error {
	var major, minor, patch int
	_, err := fmt.Sscanf(version, "%d.%d.%d", &major, &minor, &patch)
	if err != nil || strings.Contains(strings.ToLower(version), "mariadb") || major < 8 || (major == 8 && minor == 0 && patch < 16) {
		return errors.New("MySQL 8.0.16 or later is required")
	}
	if required := max(int64(64<<20), minimumPacketBytes); packet < required {
		return fmt.Errorf("MySQL max_allowed_packet must be at least %d bytes", required)
	}
	return nil
}

func (db *DB) Close() error { return db.sql.Close() }

func (db *DB) migrateMySQL(ctx context.Context, resourceDir string) error {
	for _, statement := range strings.Split(schema, ";") {
		if strings.TrimSpace(statement) == "" {
			continue
		}
		if _, err := db.sql.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize MySQL schema: %w", err)
		}
	}
	var columns int
	if err := db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'desktop_devices' AND column_name = 'revoked_at'`).Scan(&columns); err != nil {
		return fmt.Errorf("inspect Desktop revocation schema: %w", err)
	}
	if columns == 0 {
		if _, err := db.sql.ExecContext(ctx, `ALTER TABLE desktop_devices ADD COLUMN revoked_at DATETIME(6) NULL`); err != nil {
			return fmt.Errorf("add MySQL Desktop revocation: %w", err)
		}
	}
	return db.migrateLegacyConversationShareAttachments(ctx, resourceDir)
}

func isMySQLDuplicateKey(err error) bool {
	var databaseError *mysql.MySQLError
	return errors.As(err, &databaseError) && databaseError.Number == 1062
}

func databaseTime(value time.Time) time.Time { return value.UTC().Truncate(time.Microsecond) }
