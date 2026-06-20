package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/linlay/zenmind-tunnel-server/internal/auth"
	"github.com/linlay/zenmind-tunnel-server/internal/tunnel"
	_ "modernc.org/sqlite"
)

var (
	ErrNotFound                   = errors.New("not found")
	ErrDesktopDeviceHostConflict  = errors.New("desktop device host already exists")
	ErrDesktopDeviceOwnerMismatch = errors.New("desktop device belongs to another user")
)

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

type DesktopDevice struct {
	DeviceKey        string    `json:"-"`
	DeviceID         string    `json:"deviceId"`
	DeviceName       string    `json:"deviceName,omitempty"`
	OwnerUserID      string    `json:"ownerUserId,omitempty"`
	OwnerEmail       string    `json:"ownerEmail,omitempty"`
	OwnerName        string    `json:"ownerName,omitempty"`
	DeviceSecretHash string    `json:"-"`
	TokenID          string    `json:"tokenId"`
	RouteID          string    `json:"routeId"`
	PublicHost       string    `json:"publicHost"`
	TargetURL        string    `json:"targetUrl"`
	CreatedAt        time.Time `json:"createdAt"`
	UpdatedAt        time.Time `json:"updatedAt"`
}

type RegisterDesktopDeviceInput struct {
	DeviceID         string
	DeviceName       string
	OwnerUserID      string
	OwnerEmail       string
	OwnerName        string
	PublicHost       string
	TargetURL        string
	RotateToken      bool
	RotatePublicHost bool
}

type RegisterDesktopDeviceResult struct {
	Device     DesktopDevice
	Route      Route
	Token      TunnelToken
	AgentToken string
	Created    bool
	Rotated    bool
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
	if err := db.ensureAdminUserColumns(ctx); err != nil {
		return err
	}
	if err := db.ensureRouteTokenIDColumn(ctx); err != nil {
		return err
	}
	return db.ensureDesktopDeviceOwnerColumns(ctx)
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

func (db *DB) RegisterDesktopDevice(ctx context.Context, input RegisterDesktopDeviceInput) (RegisterDesktopDeviceResult, error) {
	input.DeviceID = strings.TrimSpace(input.DeviceID)
	input.DeviceName = strings.TrimSpace(input.DeviceName)
	input.OwnerUserID = strings.TrimSpace(input.OwnerUserID)
	input.OwnerEmail = strings.TrimSpace(input.OwnerEmail)
	input.OwnerName = strings.TrimSpace(input.OwnerName)
	input.PublicHost = tunnel.NormalizeHost(input.PublicHost)
	input.TargetURL = strings.TrimSpace(input.TargetURL)
	if input.DeviceID == "" {
		return RegisterDesktopDeviceResult{}, errors.New("deviceId is required")
	}
	if input.OwnerUserID == "" {
		return RegisterDesktopDeviceResult{}, errors.New("ownerUserId is required")
	}
	if input.PublicHost == "" {
		return RegisterDesktopDeviceResult{}, errors.New("publicHost is required")
	}
	if input.TargetURL == "" {
		return RegisterDesktopDeviceResult{}, errors.New("targetUrl is required")
	}

	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return RegisterDesktopDeviceResult{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	device, err := getDesktopDeviceForRegistrationTx(ctx, tx, input.OwnerUserID, input.DeviceID)
	if errors.Is(err, ErrNotFound) {
		result, err := createDesktopDeviceRegistration(ctx, tx, input)
		if err != nil {
			return RegisterDesktopDeviceResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return RegisterDesktopDeviceResult{}, err
		}
		committed = true
		return result, nil
	}
	if err != nil {
		return RegisterDesktopDeviceResult{}, err
	}
	if device.OwnerUserID != "" && device.OwnerUserID != input.OwnerUserID {
		return RegisterDesktopDeviceResult{}, ErrDesktopDeviceOwnerMismatch
	}
	token, rawToken, err := tokenForDesktopRegistration(ctx, tx, device.TokenID, input.DeviceID, input.RotateToken)
	if err != nil {
		return RegisterDesktopDeviceResult{}, err
	}
	publicHost := device.PublicHost
	if publicHost == "" || input.RotatePublicHost {
		publicHost = input.PublicHost
	}
	route, err := updateDesktopRouteTx(ctx, tx, device.RouteID, publicHost, input.TargetURL, token.ID)
	if err != nil {
		return RegisterDesktopDeviceResult{}, err
	}
	device, err = updateDesktopDeviceTx(ctx, tx, device.DeviceKey, input.DeviceID, input.DeviceName, input.OwnerUserID, input.OwnerEmail, input.OwnerName, token.ID, route.ID, route.PublicHost, route.TargetURL)
	if err != nil {
		return RegisterDesktopDeviceResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return RegisterDesktopDeviceResult{}, err
	}
	committed = true
	return RegisterDesktopDeviceResult{
		Device:     device,
		Route:      route,
		Token:      token,
		AgentToken: rawToken,
		Rotated:    input.RotateToken,
	}, nil
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

func createDesktopDeviceRegistration(ctx context.Context, tx *sql.Tx, input RegisterDesktopDeviceInput) (RegisterDesktopDeviceResult, error) {
	if _, err := getRouteByHostTx(ctx, tx, input.PublicHost); err == nil {
		return RegisterDesktopDeviceResult{}, ErrDesktopDeviceHostConflict
	} else if !errors.Is(err, ErrNotFound) {
		return RegisterDesktopDeviceResult{}, err
	}
	rawToken, err := auth.NewToken()
	if err != nil {
		return RegisterDesktopDeviceResult{}, err
	}
	token, err := insertTokenTx(ctx, tx, "desktop:"+input.DeviceID, rawToken)
	if err != nil {
		return RegisterDesktopDeviceResult{}, err
	}
	route, err := insertRouteTx(ctx, tx, input.PublicHost, input.TargetURL, true, token.ID)
	if err != nil {
		return RegisterDesktopDeviceResult{}, err
	}
	now := time.Now().UTC()
	deviceKey := desktopDeviceKey(input.OwnerUserID, input.DeviceID)
	device := DesktopDevice{
		DeviceKey:        deviceKey,
		DeviceID:         input.DeviceID,
		DeviceName:       input.DeviceName,
		OwnerUserID:      input.OwnerUserID,
		OwnerEmail:       input.OwnerEmail,
		OwnerName:        input.OwnerName,
		DeviceSecretHash: "",
		TokenID:          token.ID,
		RouteID:          route.ID,
		PublicHost:       route.PublicHost,
		TargetURL:        route.TargetURL,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO desktop_devices (device_id, display_device_id, device_name, owner_user_id, owner_email, owner_name, device_secret_hash, token_id, route_id, public_host, target_url, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, device.DeviceKey, device.DeviceID, device.DeviceName, device.OwnerUserID, device.OwnerEmail, device.OwnerName, device.DeviceSecretHash, device.TokenID, device.RouteID, device.PublicHost, device.TargetURL, device.CreatedAt, device.UpdatedAt)
	if err != nil {
		return RegisterDesktopDeviceResult{}, err
	}
	return RegisterDesktopDeviceResult{
		Device:     device,
		Route:      route,
		Token:      token,
		AgentToken: rawToken,
		Created:    true,
	}, nil
}

func tokenForDesktopRegistration(ctx context.Context, tx *sql.Tx, oldTokenID, deviceID string, rotate bool) (TunnelToken, string, error) {
	if !rotate {
		token, err := getTokenByIDTx(ctx, tx, oldTokenID)
		return token, "", err
	}
	if err := deactivateTokenTx(ctx, tx, oldTokenID); err != nil && !errors.Is(err, ErrNotFound) {
		return TunnelToken{}, "", err
	}
	rawToken, err := auth.NewToken()
	if err != nil {
		return TunnelToken{}, "", err
	}
	token, err := insertTokenTx(ctx, tx, "desktop:"+deviceID, rawToken)
	if err != nil {
		return TunnelToken{}, "", err
	}
	return token, rawToken, nil
}

func insertTokenTx(ctx context.Context, tx *sql.Tx, name, rawToken string) (TunnelToken, error) {
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
	_, err = tx.ExecContext(ctx, `
		INSERT INTO tunnel_tokens (id, name, token_hash, token_prefix, active, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, token.ID, token.Name, token.TokenHash, token.TokenPrefix, token.Active, token.CreatedAt)
	return token, err
}

func getTokenByIDTx(ctx context.Context, tx *sql.Tx, id string) (TunnelToken, error) {
	row := tx.QueryRowContext(ctx, `
		SELECT id, name, token_hash, token_prefix, active, created_at, last_used_at
		FROM tunnel_tokens WHERE id = ?
	`, id)
	return scanToken(row)
}

func deactivateTokenTx(ctx context.Context, tx *sql.Tx, id string) error {
	result, err := tx.ExecContext(ctx, `UPDATE tunnel_tokens SET active = 0 WHERE id = ?`, id)
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

func insertRouteTx(ctx context.Context, tx *sql.Tx, publicHost, targetURL string, active bool, tokenID string) (Route, error) {
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
	_, err := tx.ExecContext(ctx, `
		INSERT INTO routes (id, public_host, target_url, token_id, active, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, route.ID, route.PublicHost, route.TargetURL, nullableTokenID(route.TokenID), route.Active, route.CreatedAt, route.UpdatedAt)
	return route, err
}

func updateDesktopRouteTx(ctx context.Context, tx *sql.Tx, routeID, publicHost, targetURL, tokenID string) (Route, error) {
	route, err := updateRouteByIDTx(ctx, tx, routeID, publicHost, targetURL, true, tokenID)
	if !errors.Is(err, ErrNotFound) {
		return route, err
	}
	if _, hostErr := getRouteByHostTx(ctx, tx, publicHost); hostErr == nil {
		return Route{}, ErrDesktopDeviceHostConflict
	} else if !errors.Is(hostErr, ErrNotFound) {
		return Route{}, hostErr
	}
	return insertRouteTx(ctx, tx, publicHost, targetURL, true, tokenID)
}

func updateRouteByIDTx(ctx context.Context, tx *sql.Tx, id, publicHost, targetURL string, active bool, tokenID string) (Route, error) {
	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, `
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
	return getRouteByIDTx(ctx, tx, id)
}

func getRouteByIDTx(ctx context.Context, tx *sql.Tx, id string) (Route, error) {
	row := tx.QueryRowContext(ctx, `
		SELECT id, public_host, target_url, token_id, active, created_at, updated_at
		FROM routes WHERE id = ?
	`, id)
	return scanRoute(row)
}

func getRouteByHostTx(ctx context.Context, tx *sql.Tx, host string) (Route, error) {
	row := tx.QueryRowContext(ctx, `
		SELECT id, public_host, target_url, token_id, active, created_at, updated_at
		FROM routes WHERE public_host = ?
	`, tunnel.NormalizeHost(host))
	return scanRoute(row)
}

func getDesktopDeviceForRegistrationTx(ctx context.Context, tx *sql.Tx, ownerUserID, deviceID string) (DesktopDevice, error) {
	device, err := getDesktopDeviceByOwnerAndDisplayTx(ctx, tx, ownerUserID, deviceID)
	if !errors.Is(err, ErrNotFound) {
		return device, err
	}
	legacy, legacyErr := getDesktopDeviceByKeyTx(ctx, tx, deviceID)
	if legacyErr != nil {
		return DesktopDevice{}, legacyErr
	}
	if legacy.OwnerUserID == "" || legacy.OwnerUserID == ownerUserID {
		return legacy, nil
	}
	return DesktopDevice{}, ErrNotFound
}

func getDesktopDeviceByOwnerAndDisplayTx(ctx context.Context, tx *sql.Tx, ownerUserID, deviceID string) (DesktopDevice, error) {
	row := tx.QueryRowContext(ctx, `
		SELECT device_id, display_device_id, device_name, owner_user_id, owner_email, owner_name, device_secret_hash, token_id, route_id, public_host, target_url, created_at, updated_at
		FROM desktop_devices
		WHERE owner_user_id = ? AND display_device_id = ?
	`, strings.TrimSpace(ownerUserID), strings.TrimSpace(deviceID))
	return scanDesktopDevice(row)
}

func getDesktopDeviceByKeyTx(ctx context.Context, tx *sql.Tx, deviceKey string) (DesktopDevice, error) {
	row := tx.QueryRowContext(ctx, `
		SELECT device_id, display_device_id, device_name, owner_user_id, owner_email, owner_name, device_secret_hash, token_id, route_id, public_host, target_url, created_at, updated_at
		FROM desktop_devices WHERE device_id = ?
	`, strings.TrimSpace(deviceKey))
	return scanDesktopDevice(row)
}

func updateDesktopDeviceTx(ctx context.Context, tx *sql.Tx, deviceKey, deviceID, deviceName, ownerUserID, ownerEmail, ownerName, tokenID, routeID, publicHost, targetURL string) (DesktopDevice, error) {
	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, `
		UPDATE desktop_devices
		SET display_device_id = ?, device_name = ?, owner_user_id = ?, owner_email = ?, owner_name = ?, token_id = ?, route_id = ?, public_host = ?, target_url = ?, updated_at = ?
		WHERE device_id = ?
	`, strings.TrimSpace(deviceID), strings.TrimSpace(deviceName), strings.TrimSpace(ownerUserID), strings.TrimSpace(ownerEmail), strings.TrimSpace(ownerName), tokenID, routeID, tunnel.NormalizeHost(publicHost), strings.TrimSpace(targetURL), now, strings.TrimSpace(deviceKey))
	if err != nil {
		return DesktopDevice{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return DesktopDevice{}, err
	}
	if affected == 0 {
		return DesktopDevice{}, ErrNotFound
	}
	return getDesktopDeviceByKeyTx(ctx, tx, deviceKey)
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

func scanDesktopDevice(row rowScanner) (DesktopDevice, error) {
	var device DesktopDevice
	var displayDeviceID sql.NullString
	var deviceName sql.NullString
	var ownerUserID sql.NullString
	var ownerEmail sql.NullString
	var ownerName sql.NullString
	err := row.Scan(
		&device.DeviceKey,
		&displayDeviceID,
		&deviceName,
		&ownerUserID,
		&ownerEmail,
		&ownerName,
		&device.DeviceSecretHash,
		&device.TokenID,
		&device.RouteID,
		&device.PublicHost,
		&device.TargetURL,
		&device.CreatedAt,
		&device.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return DesktopDevice{}, ErrNotFound
	}
	if err != nil {
		return DesktopDevice{}, err
	}
	if displayDeviceID.Valid && strings.TrimSpace(displayDeviceID.String) != "" {
		device.DeviceID = strings.TrimSpace(displayDeviceID.String)
	} else {
		device.DeviceID = device.DeviceKey
	}
	if deviceName.Valid {
		device.DeviceName = strings.TrimSpace(deviceName.String)
	}
	if ownerUserID.Valid {
		device.OwnerUserID = ownerUserID.String
	}
	if ownerEmail.Valid {
		device.OwnerEmail = ownerEmail.String
	}
	if ownerName.Valid {
		device.OwnerName = ownerName.String
	}
	return device, nil
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

func desktopDeviceKey(ownerUserID, deviceID string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(ownerUserID) + "\x00" + strings.TrimSpace(deviceID)))
	return "desktop_device_" + hex.EncodeToString(sum[:12])
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

func (db *DB) ensureDesktopDeviceOwnerColumns(ctx context.Context) error {
	if err := db.ensureColumn(ctx, "desktop_devices", "owner_user_id", "TEXT"); err != nil {
		return err
	}
	if err := db.ensureColumn(ctx, "desktop_devices", "owner_email", "TEXT"); err != nil {
		return err
	}
	if err := db.ensureColumn(ctx, "desktop_devices", "owner_name", "TEXT"); err != nil {
		return err
	}
	if err := db.ensureColumn(ctx, "desktop_devices", "display_device_id", "TEXT"); err != nil {
		return err
	}
	if err := db.ensureColumn(ctx, "desktop_devices", "device_name", "TEXT"); err != nil {
		return err
	}
	if _, err := db.sql.ExecContext(ctx, `
		UPDATE desktop_devices
		SET display_device_id = device_id
		WHERE display_device_id IS NULL OR TRIM(display_device_id) = ''
	`); err != nil {
		return err
	}
	_, err := db.sql.ExecContext(ctx, `
		CREATE UNIQUE INDEX IF NOT EXISTS idx_desktop_devices_owner_display
		ON desktop_devices(owner_user_id, display_device_id)
		WHERE owner_user_id IS NOT NULL AND owner_user_id != ''
			AND display_device_id IS NOT NULL AND display_device_id != ''
	`)
	return err
}

func (db *DB) ensureColumn(ctx context.Context, table, column, definition string) error {
	rows, err := db.sql.QueryContext(ctx, fmt.Sprintf(`PRAGMA table_info(%s)`, table))
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
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.sql.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, column, definition))
	return err
}

const schema = `
CREATE TABLE IF NOT EXISTS admin_users (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	username TEXT NOT NULL UNIQUE,
	password_hash TEXT NOT NULL,
	status TEXT NOT NULL DEFAULT 'active',
	created_at TIMESTAMP NOT NULL,
	updated_at TIMESTAMP NOT NULL,
	last_login_at TIMESTAMP
);

CREATE TABLE IF NOT EXISTS admin_sessions (
	id TEXT PRIMARY KEY,
	user_id INTEGER NOT NULL,
	session_hash TEXT NOT NULL UNIQUE,
	expires_at TIMESTAMP NOT NULL,
	created_at TIMESTAMP NOT NULL,
	last_seen_at TIMESTAMP NOT NULL,
	FOREIGN KEY(user_id) REFERENCES admin_users(id)
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

CREATE TABLE IF NOT EXISTS desktop_devices (
	device_id TEXT PRIMARY KEY,
	display_device_id TEXT,
	device_name TEXT,
	owner_user_id TEXT,
	owner_email TEXT,
	owner_name TEXT,
	device_secret_hash TEXT NOT NULL,
	token_id TEXT NOT NULL,
	route_id TEXT NOT NULL,
	public_host TEXT NOT NULL UNIQUE,
	target_url TEXT NOT NULL,
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
