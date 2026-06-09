package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/linlay/zenmind-tunnel-server/internal/auth"
	"github.com/linlay/zenmind-tunnel-server/internal/tunnel"
	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

type DB struct {
	sql *sql.DB
}

type Route struct {
	ID         string    `json:"id"`
	PublicHost string    `json:"publicHost"`
	TargetURL  string    `json:"targetUrl"`
	TokenID    string    `json:"tokenId,omitempty"`
	Active     bool      `json:"active"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

type TunnelToken struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	TokenHash   string     `json:"-"`
	TokenPrefix string     `json:"tokenPrefix"`
	Active      bool       `json:"active"`
	CreatedAt   time.Time  `json:"createdAt"`
	LastUsedAt  *time.Time `json:"lastUsedAt,omitempty"`
}

type AdminAPIKey struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	KeyHash    string     `json:"-"`
	KeyPrefix  string     `json:"keyPrefix"`
	Active     bool       `json:"active"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
}

type AgentSession struct {
	ID             string     `json:"id"`
	TokenID        string     `json:"tokenId"`
	RemoteAddr     string     `json:"remoteAddr"`
	ConnectedAt    time.Time  `json:"connectedAt"`
	DisconnectedAt *time.Time `json:"disconnectedAt,omitempty"`
}

type Event struct {
	ID        int64     `json:"id"`
	Type      string    `json:"type"`
	Message   string    `json:"message"`
	Details   string    `json:"details"`
	CreatedAt time.Time `json:"createdAt"`
}

func Open(path string) (*DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA foreign_keys = ON; PRAGMA journal_mode = WAL;`); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &DB{sql: db}, nil
}

func (db *DB) Close() error {
	return db.sql.Close()
}

func (db *DB) Migrate(ctx context.Context) error {
	if _, err := db.sql.ExecContext(ctx, schema); err != nil {
		return err
	}
	return db.ensureRouteTokenIDColumn(ctx)
}

func (db *DB) BootstrapAdmin(ctx context.Context, username, password string) error {
	var count int
	if err := db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM admin_users`).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	hash, err := auth.HashSecret(password)
	if err != nil {
		return err
	}
	_, err = db.sql.ExecContext(ctx, `
		INSERT INTO admin_users (username, password_hash, created_at)
		VALUES (?, ?, ?)
	`, username, hash, time.Now().UTC())
	return err
}

func (db *DB) ValidateAdmin(ctx context.Context, username, password string) (bool, error) {
	var hash string
	err := db.sql.QueryRowContext(ctx, `
		SELECT password_hash FROM admin_users WHERE username = ?
	`, username).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return auth.VerifySecret(password, hash), nil
}

func (db *DB) CreateRoute(ctx context.Context, publicHost, targetURL string, active bool, tokenID string) (Route, error) {
	now := time.Now().UTC()
	route := Route{
		ID:         newID("route"),
		PublicHost: tunnel.NormalizeHost(publicHost),
		TargetURL:  strings.TrimSpace(targetURL),
		TokenID:    strings.TrimSpace(tokenID),
		Active:     active,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	_, err := db.sql.ExecContext(ctx, `
		INSERT INTO routes (id, public_host, target_url, token_id, active, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, route.ID, route.PublicHost, route.TargetURL, nullableTokenID(route.TokenID), route.Active, route.CreatedAt, route.UpdatedAt)
	return route, err
}

func (db *DB) UpdateRoute(ctx context.Context, id, publicHost, targetURL string, active bool, tokenID string) (Route, error) {
	now := time.Now().UTC()
	result, err := db.sql.ExecContext(ctx, `
		UPDATE routes
		SET public_host = ?, target_url = ?, token_id = ?, active = ?, updated_at = ?
		WHERE id = ?
	`, tunnel.NormalizeHost(publicHost), strings.TrimSpace(targetURL), nullableTokenID(tokenID), active, now, id)
	if err != nil {
		return Route{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return Route{}, err
	}
	if affected == 0 {
		return Route{}, ErrNotFound
	}
	return db.GetRoute(ctx, id)
}

func (db *DB) DeleteRoute(ctx context.Context, id string) error {
	result, err := db.sql.ExecContext(ctx, `DELETE FROM routes WHERE id = ?`, id)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

func (db *DB) GetRoute(ctx context.Context, id string) (Route, error) {
	row := db.sql.QueryRowContext(ctx, `
		SELECT id, public_host, target_url, token_id, active, created_at, updated_at
		FROM routes WHERE id = ?
	`, id)
	return scanRoute(row)
}

func (db *DB) GetRouteByHost(ctx context.Context, host string) (Route, error) {
	row := db.sql.QueryRowContext(ctx, `
		SELECT id, public_host, target_url, token_id, active, created_at, updated_at
		FROM routes WHERE public_host = ?
	`, tunnel.NormalizeHost(host))
	return scanRoute(row)
}

func (db *DB) GetActiveRouteByHost(ctx context.Context, host string) (Route, error) {
	row := db.sql.QueryRowContext(ctx, `
		SELECT id, public_host, target_url, token_id, active, created_at, updated_at
		FROM routes WHERE public_host = ? AND active = 1 AND token_id IS NOT NULL AND token_id != ''
	`, tunnel.NormalizeHost(host))
	return scanRoute(row)
}

func (db *DB) DeleteRouteByHost(ctx context.Context, host string) error {
	result, err := db.sql.ExecContext(ctx, `DELETE FROM routes WHERE public_host = ?`, tunnel.NormalizeHost(host))
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

func (db *DB) ListRoutes(ctx context.Context) ([]Route, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT id, public_host, target_url, token_id, active, created_at, updated_at
		FROM routes ORDER BY public_host ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	routes := make([]Route, 0)
	for rows.Next() {
		route, err := scanRoute(rows)
		if err != nil {
			return nil, err
		}
		routes = append(routes, route)
	}
	return routes, rows.Err()
}

func (db *DB) CreateToken(ctx context.Context, name, rawToken string) (TunnelToken, error) {
	hash, err := auth.HashSecret(rawToken)
	if err != nil {
		return TunnelToken{}, err
	}
	now := time.Now().UTC()
	token := TunnelToken{
		ID:          newID("token"),
		Name:        strings.TrimSpace(name),
		TokenHash:   hash,
		TokenPrefix: tokenPrefix(rawToken),
		Active:      true,
		CreatedAt:   now,
	}
	_, err = db.sql.ExecContext(ctx, `
		INSERT INTO tunnel_tokens (id, name, token_hash, token_prefix, active, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, token.ID, token.Name, token.TokenHash, token.TokenPrefix, token.Active, token.CreatedAt)
	return token, err
}

func (db *DB) FindActiveTokenBySecret(ctx context.Context, rawToken string) (TunnelToken, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT id, name, token_hash, token_prefix, active, created_at, last_used_at
		FROM tunnel_tokens WHERE active = 1
	`)
	if err != nil {
		return TunnelToken{}, err
	}
	defer rows.Close()
	for rows.Next() {
		token, err := scanToken(rows)
		if err != nil {
			return TunnelToken{}, err
		}
		if auth.VerifySecret(rawToken, token.TokenHash) {
			return token, nil
		}
	}
	if err := rows.Err(); err != nil {
		return TunnelToken{}, err
	}
	return TunnelToken{}, ErrNotFound
}

func (db *DB) TouchToken(ctx context.Context, id string) error {
	_, err := db.sql.ExecContext(ctx, `
		UPDATE tunnel_tokens SET last_used_at = ? WHERE id = ?
	`, time.Now().UTC(), id)
	return err
}

func (db *DB) GetActiveToken(ctx context.Context, id string) (TunnelToken, error) {
	row := db.sql.QueryRowContext(ctx, `
		SELECT id, name, token_hash, token_prefix, active, created_at, last_used_at
		FROM tunnel_tokens WHERE id = ? AND active = 1
	`, id)
	return scanToken(row)
}

func (db *DB) ListTokens(ctx context.Context) ([]TunnelToken, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT id, name, token_hash, token_prefix, active, created_at, last_used_at
		FROM tunnel_tokens ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tokens := make([]TunnelToken, 0)
	for rows.Next() {
		token, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		tokens = append(tokens, token)
	}
	return tokens, rows.Err()
}

func (db *DB) DeactivateToken(ctx context.Context, id string) error {
	result, err := db.sql.ExecContext(ctx, `
		UPDATE tunnel_tokens SET active = 0 WHERE id = ?
	`, id)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

func (db *DB) CreateAdminAPIKey(ctx context.Context, name, rawKey string) (AdminAPIKey, error) {
	hash, err := auth.HashSecret(rawKey)
	if err != nil {
		return AdminAPIKey{}, err
	}
	now := time.Now().UTC()
	key := AdminAPIKey{
		ID:        newID("apikey"),
		Name:      strings.TrimSpace(name),
		KeyHash:   hash,
		KeyPrefix: tokenPrefix(rawKey),
		Active:    true,
		CreatedAt: now,
	}
	_, err = db.sql.ExecContext(ctx, `
		INSERT INTO admin_api_keys (id, name, key_hash, key_prefix, active, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, key.ID, key.Name, key.KeyHash, key.KeyPrefix, key.Active, key.CreatedAt)
	return key, err
}

func (db *DB) FindActiveAdminAPIKeyBySecret(ctx context.Context, rawKey string) (AdminAPIKey, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT id, name, key_hash, key_prefix, active, created_at, last_used_at
		FROM admin_api_keys WHERE active = 1
	`)
	if err != nil {
		return AdminAPIKey{}, err
	}
	defer rows.Close()
	for rows.Next() {
		key, err := scanAdminAPIKey(rows)
		if err != nil {
			return AdminAPIKey{}, err
		}
		if auth.VerifySecret(rawKey, key.KeyHash) {
			return key, nil
		}
	}
	if err := rows.Err(); err != nil {
		return AdminAPIKey{}, err
	}
	return AdminAPIKey{}, ErrNotFound
}

func (db *DB) TouchAdminAPIKey(ctx context.Context, id string) error {
	_, err := db.sql.ExecContext(ctx, `
		UPDATE admin_api_keys SET last_used_at = ? WHERE id = ?
	`, time.Now().UTC(), id)
	return err
}

func (db *DB) ListAdminAPIKeys(ctx context.Context) ([]AdminAPIKey, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT id, name, key_hash, key_prefix, active, created_at, last_used_at
		FROM admin_api_keys ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := make([]AdminAPIKey, 0)
	for rows.Next() {
		key, err := scanAdminAPIKey(rows)
		if err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

func (db *DB) DeactivateAdminAPIKey(ctx context.Context, id string) error {
	result, err := db.sql.ExecContext(ctx, `
		UPDATE admin_api_keys SET active = 0 WHERE id = ?
	`, id)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

func (db *DB) CreateAgentSession(ctx context.Context, tokenID, remoteAddr string) (AgentSession, error) {
	session := AgentSession{
		ID:          newID("session"),
		TokenID:     tokenID,
		RemoteAddr:  remoteAddr,
		ConnectedAt: time.Now().UTC(),
	}
	_, err := db.sql.ExecContext(ctx, `
		INSERT INTO agent_sessions (id, token_id, remote_addr, connected_at)
		VALUES (?, ?, ?, ?)
	`, session.ID, session.TokenID, session.RemoteAddr, session.ConnectedAt)
	return session, err
}

func (db *DB) EndAgentSession(ctx context.Context, id string) error {
	_, err := db.sql.ExecContext(ctx, `
		UPDATE agent_sessions SET disconnected_at = ?
		WHERE id = ? AND disconnected_at IS NULL
	`, time.Now().UTC(), id)
	return err
}

func (db *DB) ListAgentSessions(ctx context.Context, limit int) ([]AgentSession, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := db.sql.QueryContext(ctx, `
		SELECT id, token_id, remote_addr, connected_at, disconnected_at
		FROM agent_sessions ORDER BY connected_at DESC LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sessions := make([]AgentSession, 0)
	for rows.Next() {
		session, err := scanAgentSession(rows)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}
	return sessions, rows.Err()
}

func (db *DB) AddEvent(ctx context.Context, eventType, message, details string) error {
	_, err := db.sql.ExecContext(ctx, `
		INSERT INTO events (type, message, details, created_at)
		VALUES (?, ?, ?, ?)
	`, eventType, message, details, time.Now().UTC())
	return err
}

func (db *DB) ListEvents(ctx context.Context, limit int) ([]Event, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := db.sql.QueryContext(ctx, `
		SELECT id, type, message, details, created_at
		FROM events ORDER BY created_at DESC LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := make([]Event, 0)
	for rows.Next() {
		var event Event
		if err := rows.Scan(&event.ID, &event.Type, &event.Message, &event.Details, &event.CreatedAt); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanRoute(row rowScanner) (Route, error) {
	var route Route
	var tokenID sql.NullString
	err := row.Scan(&route.ID, &route.PublicHost, &route.TargetURL, &tokenID, &route.Active, &route.CreatedAt, &route.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Route{}, ErrNotFound
	}
	if err != nil {
		return Route{}, err
	}
	if tokenID.Valid {
		route.TokenID = tokenID.String
	}
	return route, nil
}

func scanToken(row rowScanner) (TunnelToken, error) {
	var token TunnelToken
	var lastUsed sql.NullTime
	err := row.Scan(&token.ID, &token.Name, &token.TokenHash, &token.TokenPrefix, &token.Active, &token.CreatedAt, &lastUsed)
	if errors.Is(err, sql.ErrNoRows) {
		return TunnelToken{}, ErrNotFound
	}
	if err != nil {
		return TunnelToken{}, err
	}
	if lastUsed.Valid {
		token.LastUsedAt = &lastUsed.Time
	}
	return token, nil
}

func scanAdminAPIKey(row rowScanner) (AdminAPIKey, error) {
	var key AdminAPIKey
	var lastUsed sql.NullTime
	err := row.Scan(&key.ID, &key.Name, &key.KeyHash, &key.KeyPrefix, &key.Active, &key.CreatedAt, &lastUsed)
	if errors.Is(err, sql.ErrNoRows) {
		return AdminAPIKey{}, ErrNotFound
	}
	if err != nil {
		return AdminAPIKey{}, err
	}
	if lastUsed.Valid {
		key.LastUsedAt = &lastUsed.Time
	}
	return key, nil
}

func scanAgentSession(row rowScanner) (AgentSession, error) {
	var session AgentSession
	var disconnected sql.NullTime
	err := row.Scan(&session.ID, &session.TokenID, &session.RemoteAddr, &session.ConnectedAt, &disconnected)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentSession{}, ErrNotFound
	}
	if err != nil {
		return AgentSession{}, err
	}
	if disconnected.Valid {
		session.DisconnectedAt = &disconnected.Time
	}
	return session, nil
}

func newID(prefix string) string {
	return fmt.Sprintf("%s_%d", prefix, time.Now().UTC().UnixNano())
}

func tokenPrefix(token string) string {
	if len(token) <= 12 {
		return token
	}
	return token[:12]
}

func nullableTokenID(tokenID string) any {
	tokenID = strings.TrimSpace(tokenID)
	if tokenID == "" {
		return nil
	}
	return tokenID
}

func (db *DB) ensureRouteTokenIDColumn(ctx context.Context) error {
	rows, err := db.sql.QueryContext(ctx, `PRAGMA table_info(routes)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk); err != nil {
			return err
		}
		if name == "token_id" {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.sql.ExecContext(ctx, `ALTER TABLE routes ADD COLUMN token_id TEXT`)
	return err
}

const schema = `
CREATE TABLE IF NOT EXISTS admin_users (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	username TEXT NOT NULL UNIQUE,
	password_hash TEXT NOT NULL,
	created_at TIMESTAMP NOT NULL
);

CREATE TABLE IF NOT EXISTS tunnel_tokens (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	token_hash TEXT NOT NULL,
	token_prefix TEXT NOT NULL,
	active BOOLEAN NOT NULL DEFAULT 1,
	created_at TIMESTAMP NOT NULL,
	last_used_at TIMESTAMP
);

CREATE TABLE IF NOT EXISTS admin_api_keys (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	key_hash TEXT NOT NULL,
	key_prefix TEXT NOT NULL,
	active BOOLEAN NOT NULL DEFAULT 1,
	created_at TIMESTAMP NOT NULL,
	last_used_at TIMESTAMP
);

CREATE TABLE IF NOT EXISTS routes (
	id TEXT PRIMARY KEY,
	public_host TEXT NOT NULL UNIQUE,
	target_url TEXT NOT NULL,
	token_id TEXT,
	active BOOLEAN NOT NULL DEFAULT 1,
	created_at TIMESTAMP NOT NULL,
	updated_at TIMESTAMP NOT NULL,
	FOREIGN KEY (token_id) REFERENCES tunnel_tokens(id)
);

CREATE TABLE IF NOT EXISTS agent_sessions (
	id TEXT PRIMARY KEY,
	token_id TEXT NOT NULL,
	remote_addr TEXT NOT NULL,
	connected_at TIMESTAMP NOT NULL,
	disconnected_at TIMESTAMP,
	FOREIGN KEY (token_id) REFERENCES tunnel_tokens(id)
);

CREATE TABLE IF NOT EXISTS events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	type TEXT NOT NULL,
	message TEXT NOT NULL,
	details TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMP NOT NULL
);
`
