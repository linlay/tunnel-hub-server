package config

import (
	"fmt"
	"os"
	"strings"
)

type DatabaseType string

const (
	DatabaseMySQL  DatabaseType = "mysql"
	DatabaseSQLite DatabaseType = "sqlite"
)

func loadDatabaseConfig() (DatabaseType, string, MySQLConfig, error) {
	databaseType := DatabaseType(strings.TrimSpace(os.Getenv("DATABASE_TYPE")))
	if databaseType == "" {
		databaseType = DatabaseMySQL
	}
	switch databaseType {
	case DatabaseMySQL:
		mysqlConfig, err := loadMySQLConfig()
		return databaseType, "", mysqlConfig, err
	case DatabaseSQLite:
		path, err := requiredEnv("RELAY_DB_PATH")
		return databaseType, path, MySQLConfig{}, err
	default:
		return "", "", MySQLConfig{}, fmt.Errorf("DATABASE_TYPE must be mysql or sqlite")
	}
}
