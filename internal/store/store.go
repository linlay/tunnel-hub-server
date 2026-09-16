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

	"example.invalid/tunnel-hub-server/internal/auth"
	"example.invalid/tunnel-hub-server/internal/tunnel"
)

var (
	ErrNotFound                  = errors.New("not found")
	ErrDesktopDeviceHostConflict = errors.New("desktop device host already exists")
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
	DeviceKey   string    `json:"-"`
	DeviceID    string    `json:"deviceId"`
	DeviceName  string    `json:"deviceName,omitempty"`
	OwnerUserID string    `json:"ownerUserId,omitempty"`
	OwnerEmail  string    `json:"ownerEmail,omitempty"`
	OwnerName   string    `json:"ownerName,omitempty"`
	PublicHost  string    `json:"publicHost"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

type DesktopWebApp struct {
	ID         string    `json:"id"`
	DeviceKey  string    `json:"-"`
	Name       string    `json:"name"`
	RouteID    string    `json:"routeId"`
	PublicHost string    `json:"publicHost"`
	TargetURL  string    `json:"targetUrl"`
	Active     bool      `json:"active"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

type TrafficEvent struct {
	ID         int64     `json:"id"`
	ObjectType string    `json:"objectType"`
	PublicHost string    `json:"publicHost"`
	RouteID    string    `json:"routeId,omitempty"`
	TokenID    string    `json:"tokenId,omitempty"`
	DeviceID   string    `json:"deviceId,omitempty"`
	SessionID  string    `json:"sessionId,omitempty"`
	Kind       string    `json:"kind"`
	Method     string    `json:"method,omitempty"`
	Path       string    `json:"path,omitempty"`
	StatusCode int       `json:"statusCode,omitempty"`
	BytesIn    int64     `json:"bytesIn"`
	BytesOut   int64     `json:"bytesOut"`
	Error      string    `json:"error,omitempty"`
	OccurredAt time.Time `json:"occurredAt"`
}

type TrafficStats struct {
	RequestCount int64      `json:"requestCount"`
	BytesIn      int64      `json:"bytesIn"`
	BytesOut     int64      `json:"bytesOut"`
	LastAt       *time.Time `json:"lastAt,omitempty"`
}

type RegisterDesktopDeviceInput struct {
	DeviceID    string
	DeviceName  string
	OwnerUserID string
	OwnerEmail  string
	OwnerName   string
	PublicHost  string
}

type RegisterDesktopDeviceResult struct {
	Device  DesktopDevice
	Created bool
}

type RegisterDesktopWebAppInput struct {
	OwnerUserID string
	DeviceID    string
	Name        string
	PublicHost  string
	TargetURL   string
	Active      bool
}

type RegisterDesktopWebAppResult struct {
	Device DesktopDevice
	WebApp DesktopWebApp
	Route  Route
}

type DesktopWebAppRoute struct {
	Device DesktopDevice
	WebApp DesktopWebApp
	Route  Route
}

type AgentSession struct {
	ID             string     `json:"id"`
	TokenID        string     `json:"tokenId"`
	RemoteAddr     string     `json:"remoteAddr"`
	ConnectedAt    time.Time  `json:"connectedAt"`
	DisconnectedAt *time.Time `json:"disconnectedAt,omitempty"`
}

type DesktopSession struct {
	ID             string     `json:"id"`
	DeviceKey      string     `json:"-"`
	DeviceID       string     `json:"deviceId"`
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

func (db *DB) CreateRoute(ctx context.Context, publicHost, targetURL string, active bool, tokenID string) (Route, error) {

	if err := validateRouteFields(tunnel.NormalizeHost(publicHost), tokenID); err != nil {
		return Route{}, err
	}
	now := databaseTime(time.Now())
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

	if err := validateRouteFields(tunnel.NormalizeHost(publicHost), tokenID); err != nil {
		return Route{}, err
	}
	now := databaseTime(time.Now())
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
	now := databaseTime(time.Now())
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
	`, databaseTime(time.Now()), id)
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
	if input.DeviceID == "" {
		return RegisterDesktopDeviceResult{}, errors.New("deviceId is required")
	}
	if err := validateDesktopIdentity(input.OwnerUserID, input.DeviceID, input.PublicHost); err != nil {
		return RegisterDesktopDeviceResult{}, err
	}
	if input.OwnerUserID == "" {
		return RegisterDesktopDeviceResult{}, errors.New("ownerUserId is required")
	}
	if input.PublicHost == "" {
		return RegisterDesktopDeviceResult{}, errors.New("publicHost is required")
	}

	tx, err := db.sql.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return RegisterDesktopDeviceResult{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	device, err := lockDesktopDeviceByOwnerAndDisplayTx(ctx, tx, input.OwnerUserID, input.DeviceID)
	if errors.Is(err, ErrNotFound) {
		result, err := createDesktopDeviceRegistration(ctx, tx, input)
		if err != nil {
			if !isDuplicateKey(err) {
				return RegisterDesktopDeviceResult{}, err
			}
			_ = tx.Rollback()
			// Re-read outside the failed transaction so the winner is visible.
			existing, lookupErr := db.GetDesktopDeviceByOwnerAndID(ctx, input.OwnerUserID, input.DeviceID)
			if lookupErr == nil {
				return RegisterDesktopDeviceResult{Device: existing}, nil
			}
			if errors.Is(lookupErr, ErrNotFound) {
				return RegisterDesktopDeviceResult{}, ErrDesktopDeviceHostConflict
			}
			return RegisterDesktopDeviceResult{}, lookupErr
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
	device, err = updateDesktopDeviceTx(ctx, tx, device.DeviceKey, input.DeviceID, input.DeviceName, input.OwnerUserID, input.OwnerEmail, input.OwnerName, device.PublicHost)
	if err != nil {
		return RegisterDesktopDeviceResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return RegisterDesktopDeviceResult{}, err
	}
	committed = true
	return RegisterDesktopDeviceResult{
		Device: device,
	}, nil
}

func (db *DB) GetDesktopDeviceByPublicHost(ctx context.Context, host string) (DesktopDevice, error) {
	row := db.sql.QueryRowContext(ctx, `
		SELECT device_id, display_device_id, device_name, owner_user_id, owner_email, owner_name, public_host, created_at, updated_at
		FROM desktop_devices WHERE public_host = ?
	`, tunnel.NormalizeHost(host))
	return scanDesktopDevice(row)
}

func (db *DB) GetDesktopDeviceByOwnerAndID(ctx context.Context, ownerUserID, deviceID string) (DesktopDevice, error) {
	row := db.sql.QueryRowContext(ctx, `
		SELECT device_id, display_device_id, device_name, owner_user_id, owner_email, owner_name, public_host, created_at, updated_at
		FROM desktop_devices
		WHERE owner_user_id = ? AND display_device_id = ?
	`, strings.TrimSpace(ownerUserID), strings.TrimSpace(deviceID))
	return scanDesktopDevice(row)
}

func (db *DB) ListDesktopDevices(ctx context.Context) ([]DesktopDevice, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT device_id, display_device_id, device_name, owner_user_id, owner_email, owner_name, public_host, created_at, updated_at
		FROM desktop_devices ORDER BY updated_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	devices := make([]DesktopDevice, 0)
	for rows.Next() {
		device, err := scanDesktopDevice(rows)
		if err != nil {
			return nil, err
		}
		devices = append(devices, device)
	}
	return devices, rows.Err()
}

func (db *DB) RegisterDesktopWebApp(ctx context.Context, input RegisterDesktopWebAppInput) (RegisterDesktopWebAppResult, error) {
	input.OwnerUserID = strings.TrimSpace(input.OwnerUserID)
	input.DeviceID = strings.TrimSpace(input.DeviceID)
	input.Name = strings.TrimSpace(input.Name)
	input.PublicHost = tunnel.NormalizeHost(input.PublicHost)
	input.TargetURL = strings.TrimSpace(input.TargetURL)
	if err := validateDesktopIdentity(input.OwnerUserID, input.DeviceID, input.PublicHost); err != nil {
		return RegisterDesktopWebAppResult{}, err
	}
	if err := ValidateTextLength("name", input.Name, 63); err != nil {
		return RegisterDesktopWebAppResult{}, err
	}
	if input.OwnerUserID == "" {
		return RegisterDesktopWebAppResult{}, errors.New("ownerUserId is required")
	}
	if input.DeviceID == "" {
		return RegisterDesktopWebAppResult{}, errors.New("deviceId is required")
	}
	if input.Name == "" {
		return RegisterDesktopWebAppResult{}, errors.New("name is required")
	}
	if input.PublicHost == "" {
		return RegisterDesktopWebAppResult{}, errors.New("publicHost is required")
	}
	if input.TargetURL == "" {
		return RegisterDesktopWebAppResult{}, errors.New("targetUrl is required")
	}

	tx, err := db.sql.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return RegisterDesktopWebAppResult{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	device, err := lockDesktopDeviceByOwnerAndDisplayTx(ctx, tx, input.OwnerUserID, input.DeviceID)
	if err != nil {
		return RegisterDesktopWebAppResult{}, err
	}
	webApp, err := getDesktopWebAppByDeviceAndNameTx(ctx, tx, device.DeviceKey, input.Name)
	if errors.Is(err, ErrNotFound) {
		if err := ensurePublicHostAvailableTx(ctx, tx, input.PublicHost); err != nil {
			return RegisterDesktopWebAppResult{}, err
		}
		route, err := insertRouteTx(ctx, tx, input.PublicHost, input.TargetURL, input.Active, "")
		if err != nil {
			return RegisterDesktopWebAppResult{}, desktopHostError(err)
		}
		webApp, err := insertDesktopWebAppTx(ctx, tx, device.DeviceKey, input.Name, route)
		if err != nil {
			return RegisterDesktopWebAppResult{}, desktopHostError(err)
		}
		if err := tx.Commit(); err != nil {
			return RegisterDesktopWebAppResult{}, err
		}
		committed = true
		return RegisterDesktopWebAppResult{Device: device, WebApp: webApp, Route: route}, nil
	}
	if err != nil {
		return RegisterDesktopWebAppResult{}, err
	}

	route, err := updateDesktopWebAppRouteTx(ctx, tx, webApp.RouteID, webApp.PublicHost, input.TargetURL, input.Active)
	if err != nil {
		return RegisterDesktopWebAppResult{}, err
	}
	webApp, err = updateDesktopWebAppTx(ctx, tx, webApp.ID, route)
	if err != nil {
		return RegisterDesktopWebAppResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return RegisterDesktopWebAppResult{}, err
	}
	committed = true
	return RegisterDesktopWebAppResult{Device: device, WebApp: webApp, Route: route}, nil
}

func (db *DB) ListDesktopWebApps(ctx context.Context) ([]DesktopWebApp, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT id, device_id, name, route_id, public_host, target_url, active, created_at, updated_at
		FROM desktop_webapps ORDER BY updated_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	webApps := make([]DesktopWebApp, 0)
	for rows.Next() {
		webApp, err := scanDesktopWebApp(rows)
		if err != nil {
			return nil, err
		}
		webApps = append(webApps, webApp)
	}
	return webApps, rows.Err()
}

func (db *DB) GetActiveDesktopWebAppRouteByHost(ctx context.Context, host string) (DesktopWebAppRoute, error) {
	row := db.sql.QueryRowContext(ctx, `
		SELECT
			d.device_id, d.display_device_id, d.device_name, d.owner_user_id, d.owner_email, d.owner_name, d.public_host, d.created_at, d.updated_at,
			w.id, w.device_id, w.name, w.route_id, w.public_host, w.target_url, w.active, w.created_at, w.updated_at,
			r.id, r.public_host, r.target_url, r.token_id, r.active, r.created_at, r.updated_at
		FROM desktop_webapps w
		JOIN desktop_devices d ON d.device_id = w.device_id
		JOIN routes r ON r.id = w.route_id
		WHERE w.public_host = ? AND w.active = 1 AND r.active = 1
	`, tunnel.NormalizeHost(host))
	var result DesktopWebAppRoute
	var deviceName, ownerEmail, ownerName sql.NullString
	var routeTokenID sql.NullString
	if err := row.Scan(
		&result.Device.DeviceKey, &result.Device.DeviceID, &deviceName, &result.Device.OwnerUserID, &ownerEmail, &ownerName,
		&result.Device.PublicHost, &result.Device.CreatedAt, &result.Device.UpdatedAt,
		&result.WebApp.ID, &result.WebApp.DeviceKey, &result.WebApp.Name, &result.WebApp.RouteID,
		&result.WebApp.PublicHost, &result.WebApp.TargetURL, &result.WebApp.Active,
		&result.WebApp.CreatedAt, &result.WebApp.UpdatedAt,
		&result.Route.ID, &result.Route.PublicHost, &result.Route.TargetURL, &routeTokenID,
		&result.Route.Active, &result.Route.CreatedAt, &result.Route.UpdatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DesktopWebAppRoute{}, ErrNotFound
		}
		return DesktopWebAppRoute{}, err
	}
	result.Device.DeviceName = strings.TrimSpace(deviceName.String)
	result.Device.OwnerEmail = strings.TrimSpace(ownerEmail.String)
	result.Device.OwnerName = strings.TrimSpace(ownerName.String)
	if routeTokenID.Valid {
		result.Route.TokenID = routeTokenID.String
	}
	return result, nil
}

func (db *DB) CreateAgentSession(ctx context.Context, tokenID, remoteAddr string) (AgentSession, error) {
	session := AgentSession{
		ID:          newID("session"),
		TokenID:     tokenID,
		RemoteAddr:  remoteAddr,
		ConnectedAt: databaseTime(time.Now()),
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
	`, databaseTime(time.Now()), id)
	return err
}

func (db *DB) GetAgentSession(ctx context.Context, id string) (AgentSession, error) {
	row := db.sql.QueryRowContext(ctx, `
		SELECT id, token_id, remote_addr, connected_at, disconnected_at
		FROM agent_sessions WHERE id = ?
	`, strings.TrimSpace(id))
	return scanAgentSession(row)
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

func (db *DB) CreateDesktopSession(ctx context.Context, device DesktopDevice, remoteAddr string) (DesktopSession, error) {
	session := DesktopSession{
		ID:          newID("desktop_session"),
		DeviceKey:   device.DeviceKey,
		DeviceID:    device.DeviceID,
		RemoteAddr:  strings.TrimSpace(remoteAddr),
		ConnectedAt: databaseTime(time.Now()),
	}
	_, err := db.sql.ExecContext(ctx, `
		INSERT INTO desktop_sessions (id, device_id, remote_addr, connected_at)
		VALUES (?, ?, ?, ?)
	`, session.ID, session.DeviceKey, session.RemoteAddr, session.ConnectedAt)
	return session, err
}

func (db *DB) EndDesktopSession(ctx context.Context, id string) error {
	_, err := db.sql.ExecContext(ctx, `
		UPDATE desktop_sessions SET disconnected_at = ?
		WHERE id = ? AND disconnected_at IS NULL
	`, databaseTime(time.Now()), strings.TrimSpace(id))
	return err
}

func (db *DB) GetDesktopSession(ctx context.Context, id string) (DesktopSession, error) {
	row := db.sql.QueryRowContext(ctx, `
		SELECT s.id, s.device_id, d.display_device_id, s.remote_addr, s.connected_at, s.disconnected_at
		FROM desktop_sessions s
		JOIN desktop_devices d ON d.device_id = s.device_id
		WHERE s.id = ?
	`, strings.TrimSpace(id))
	return scanDesktopSession(row)
}

func (db *DB) ListDesktopSessions(ctx context.Context, limit int) ([]DesktopSession, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := db.sql.QueryContext(ctx, `
		SELECT s.id, s.device_id, d.display_device_id, s.remote_addr, s.connected_at, s.disconnected_at
		FROM desktop_sessions s
		JOIN desktop_devices d ON d.device_id = s.device_id
		ORDER BY s.connected_at DESC LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sessions := make([]DesktopSession, 0)
	for rows.Next() {
		session, err := scanDesktopSession(rows)
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
	`, eventType, message, details, databaseTime(time.Now()))
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

func (db *DB) RecordTrafficEvent(ctx context.Context, event TrafficEvent) error {
	event.ObjectType = strings.TrimSpace(event.ObjectType)
	event.PublicHost = tunnel.NormalizeHost(event.PublicHost)
	event.RouteID = strings.TrimSpace(event.RouteID)
	event.TokenID = strings.TrimSpace(event.TokenID)
	event.DeviceID = strings.TrimSpace(event.DeviceID)
	event.SessionID = strings.TrimSpace(event.SessionID)
	event.Kind = strings.TrimSpace(event.Kind)
	event.Method = strings.TrimSpace(event.Method)
	event.Path = strings.TrimSpace(event.Path)
	event.Error = strings.TrimSpace(event.Error)
	if event.OccurredAt.IsZero() {
		event.OccurredAt = databaseTime(time.Now())
	}
	_, err := db.sql.ExecContext(ctx, `
		INSERT INTO traffic_events (
			object_type, public_host, route_id, token_id, device_id, session_id, kind, method, path,
			status_code, bytes_in, bytes_out, error, occurred_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, event.ObjectType, event.PublicHost, nullableString(event.RouteID), nullableString(event.TokenID), nullableString(event.DeviceID), nullableString(event.SessionID), event.Kind, event.Method, event.Path, event.StatusCode, event.BytesIn, event.BytesOut, event.Error, event.OccurredAt)
	return err
}

func (db *DB) ListTrafficEvents(ctx context.Context, limit int, objectType, query string) ([]TrafficEvent, error) {
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	objectType = strings.TrimSpace(objectType)
	query = strings.TrimSpace(query)
	clauses := []string{"1 = 1"}
	args := make([]any, 0, 4)
	if objectType != "" && objectType != "all" {
		clauses = append(clauses, "object_type = ?")
		args = append(args, objectType)
	}
	if query != "" {
		clauses = append(clauses, "(public_host COLLATE utf8mb4_0900_as_ci LIKE ? OR route_id COLLATE utf8mb4_0900_as_ci LIKE ? OR token_id COLLATE utf8mb4_0900_as_ci LIKE ? OR device_id COLLATE utf8mb4_0900_as_ci LIKE ? OR session_id COLLATE utf8mb4_0900_as_ci LIKE ? OR kind COLLATE utf8mb4_0900_as_ci LIKE ? OR method COLLATE utf8mb4_0900_as_ci LIKE ? OR path COLLATE utf8mb4_0900_as_ci LIKE ? OR error COLLATE utf8mb4_0900_as_ci LIKE ?)")
		like := "%" + query + "%"
		for i := 0; i < 9; i++ {
			args = append(args, like)
		}
	}
	args = append(args, limit)
	rows, err := db.sql.QueryContext(ctx, `
		SELECT id, object_type, public_host, route_id, token_id, device_id, session_id, kind, method, path, status_code, bytes_in, bytes_out, error, occurred_at
		FROM traffic_events
		WHERE `+strings.Join(clauses, " AND ")+`
		ORDER BY occurred_at DESC, id DESC LIMIT ?
	`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTrafficEvents(rows)
}

func (db *DB) ListTrafficEventsSince(ctx context.Context, since time.Time) ([]TrafficEvent, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT id, object_type, public_host, route_id, token_id, device_id, session_id, kind, method, path, status_code, bytes_in, bytes_out, error, occurred_at
		FROM traffic_events
		WHERE occurred_at >= ?
		ORDER BY occurred_at ASC, id ASC
	`, databaseTime(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTrafficEvents(rows)
}

func (db *DB) TrafficStatsByPublicHost(ctx context.Context) (map[string]TrafficStats, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT public_host, COUNT(*), COALESCE(SUM(bytes_in), 0), COALESCE(SUM(bytes_out), 0), MAX(occurred_at)
		FROM traffic_events
		WHERE public_host != ''
		GROUP BY public_host
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	stats := make(map[string]TrafficStats)
	for rows.Next() {
		var key string
		value, err := scanTrafficStatsWithKey(rows, &key)
		if err != nil {
			return nil, err
		}
		stats[key] = value
	}
	return stats, rows.Err()
}

func (db *DB) TrafficStatsByToken(ctx context.Context) (map[string]TrafficStats, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT token_id, COUNT(*), COALESCE(SUM(bytes_in), 0), COALESCE(SUM(bytes_out), 0), MAX(occurred_at)
		FROM traffic_events
		WHERE token_id IS NOT NULL AND token_id != ''
		GROUP BY token_id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	stats := make(map[string]TrafficStats)
	for rows.Next() {
		var key string
		value, err := scanTrafficStatsWithKey(rows, &key)
		if err != nil {
			return nil, err
		}
		stats[key] = value
	}
	return stats, rows.Err()
}

func (db *DB) TrafficStatsByDevice(ctx context.Context) (map[string]TrafficStats, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT device_id, COUNT(*), COALESCE(SUM(bytes_in), 0), COALESCE(SUM(bytes_out), 0), MAX(occurred_at)
		FROM traffic_events
		WHERE device_id IS NOT NULL AND device_id != ''
		GROUP BY device_id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	stats := make(map[string]TrafficStats)
	for rows.Next() {
		var key string
		value, err := scanTrafficStatsWithKey(rows, &key)
		if err != nil {
			return nil, err
		}
		stats[key] = value
	}
	return stats, rows.Err()
}

func (db *DB) TrafficTotals(ctx context.Context) (TrafficStats, error) {
	row := db.sql.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(bytes_in), 0), COALESCE(SUM(bytes_out), 0), MAX(occurred_at)
		FROM traffic_events
	`)
	return scanTrafficStatsNoKey(row)
}

func createDesktopDeviceRegistration(ctx context.Context, tx *sql.Tx, input RegisterDesktopDeviceInput) (RegisterDesktopDeviceResult, error) {
	if err := ensurePublicHostAvailableTx(ctx, tx, input.PublicHost); err != nil {
		return RegisterDesktopDeviceResult{}, err
	}
	now := databaseTime(time.Now())
	deviceKey := desktopDeviceKey(input.OwnerUserID, input.DeviceID)
	device := DesktopDevice{
		DeviceKey:   deviceKey,
		DeviceID:    input.DeviceID,
		DeviceName:  input.DeviceName,
		OwnerUserID: input.OwnerUserID,
		OwnerEmail:  input.OwnerEmail,
		OwnerName:   input.OwnerName,
		PublicHost:  input.PublicHost,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO desktop_devices (device_id, display_device_id, device_name, owner_user_id, owner_email, owner_name, public_host, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, device.DeviceKey, device.DeviceID, device.DeviceName, device.OwnerUserID, device.OwnerEmail, device.OwnerName, device.PublicHost, device.CreatedAt, device.UpdatedAt)
	if err != nil {
		return RegisterDesktopDeviceResult{}, err
	}
	return RegisterDesktopDeviceResult{
		Device:  device,
		Created: true,
	}, nil
}

func ensurePublicHostAvailableTx(ctx context.Context, tx *sql.Tx, publicHost string) error {
	if _, err := getRouteByHostTx(ctx, tx, publicHost); err == nil {
		return ErrDesktopDeviceHostConflict
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	_, err := getDesktopDeviceByPublicHostTx(ctx, tx, publicHost)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return ErrDesktopDeviceHostConflict
}

func insertRouteTx(ctx context.Context, tx *sql.Tx, publicHost, targetURL string, active bool, tokenID string) (Route, error) {
	now := databaseTime(time.Now())
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

func updateDesktopWebAppRouteTx(ctx context.Context, tx *sql.Tx, id, publicHost, targetURL string, active bool) (Route, error) {
	now := databaseTime(time.Now())
	result, err := tx.ExecContext(ctx, `
		UPDATE routes
		SET public_host = ?, target_url = ?, token_id = ?, active = ?, updated_at = ?
		WHERE id = ?
	`, tunnel.NormalizeHost(publicHost), strings.TrimSpace(targetURL), nil, active, now, id)
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

func lockDesktopDeviceByOwnerAndDisplayTx(ctx context.Context, tx *sql.Tx, ownerUserID, deviceID string) (DesktopDevice, error) {
	row := tx.QueryRowContext(ctx, `
		SELECT device_id, display_device_id, device_name, owner_user_id, owner_email, owner_name, public_host, created_at, updated_at
		FROM desktop_devices
		WHERE owner_user_id = ? AND display_device_id = ? FOR UPDATE
	`, strings.TrimSpace(ownerUserID), strings.TrimSpace(deviceID))
	return scanDesktopDevice(row)
}

func getDesktopDeviceByKeyTx(ctx context.Context, tx *sql.Tx, deviceKey string) (DesktopDevice, error) {
	row := tx.QueryRowContext(ctx, `
		SELECT device_id, display_device_id, device_name, owner_user_id, owner_email, owner_name, public_host, created_at, updated_at
		FROM desktop_devices WHERE device_id = ?
	`, strings.TrimSpace(deviceKey))
	return scanDesktopDevice(row)
}

func getDesktopDeviceByPublicHostTx(ctx context.Context, tx *sql.Tx, publicHost string) (DesktopDevice, error) {
	row := tx.QueryRowContext(ctx, `
		SELECT device_id, display_device_id, device_name, owner_user_id, owner_email, owner_name, public_host, created_at, updated_at
		FROM desktop_devices WHERE public_host = ?
	`, tunnel.NormalizeHost(publicHost))
	return scanDesktopDevice(row)
}

func getDesktopWebAppByDeviceAndNameTx(ctx context.Context, tx *sql.Tx, deviceKey, name string) (DesktopWebApp, error) {
	row := tx.QueryRowContext(ctx, `
		SELECT id, device_id, name, route_id, public_host, target_url, active, created_at, updated_at
		FROM desktop_webapps WHERE device_id = ? AND name = ?
	`, strings.TrimSpace(deviceKey), strings.TrimSpace(name))
	return scanDesktopWebApp(row)
}

func insertDesktopWebAppTx(ctx context.Context, tx *sql.Tx, deviceKey, name string, route Route) (DesktopWebApp, error) {
	now := databaseTime(time.Now())
	webApp := DesktopWebApp{
		ID:         newID("webapp"),
		DeviceKey:  strings.TrimSpace(deviceKey),
		Name:       strings.TrimSpace(name),
		RouteID:    route.ID,
		PublicHost: route.PublicHost,
		TargetURL:  route.TargetURL,
		Active:     route.Active,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO desktop_webapps (id, device_id, name, route_id, public_host, target_url, active, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, webApp.ID, webApp.DeviceKey, webApp.Name, webApp.RouteID, webApp.PublicHost, webApp.TargetURL, webApp.Active, webApp.CreatedAt, webApp.UpdatedAt)
	return webApp, err
}

func updateDesktopWebAppTx(ctx context.Context, tx *sql.Tx, id string, route Route) (DesktopWebApp, error) {
	now := databaseTime(time.Now())
	result, err := tx.ExecContext(ctx, `
		UPDATE desktop_webapps
		SET route_id = ?, public_host = ?, target_url = ?, active = ?, updated_at = ?
		WHERE id = ?
	`, route.ID, route.PublicHost, route.TargetURL, route.Active, now, strings.TrimSpace(id))
	if err != nil {
		return DesktopWebApp{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return DesktopWebApp{}, err
	}
	if affected == 0 {
		return DesktopWebApp{}, ErrNotFound
	}
	row := tx.QueryRowContext(ctx, `
		SELECT id, device_id, name, route_id, public_host, target_url, active, created_at, updated_at
		FROM desktop_webapps WHERE id = ?
	`, strings.TrimSpace(id))
	return scanDesktopWebApp(row)
}

func updateDesktopDeviceTx(ctx context.Context, tx *sql.Tx, deviceKey, deviceID, deviceName, ownerUserID, ownerEmail, ownerName, publicHost string) (DesktopDevice, error) {
	now := databaseTime(time.Now())
	result, err := tx.ExecContext(ctx, `
		UPDATE desktop_devices
		SET display_device_id = ?, device_name = ?, owner_user_id = ?, owner_email = ?, owner_name = ?, public_host = ?, updated_at = ?
		WHERE device_id = ?
	`, strings.TrimSpace(deviceID), strings.TrimSpace(deviceName), strings.TrimSpace(ownerUserID), strings.TrimSpace(ownerEmail), strings.TrimSpace(ownerName), tunnel.NormalizeHost(publicHost), now, strings.TrimSpace(deviceKey))
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
	var deviceName sql.NullString
	var ownerEmail sql.NullString
	var ownerName sql.NullString
	err := row.Scan(
		&device.DeviceKey,
		&device.DeviceID,
		&deviceName,
		&device.OwnerUserID,
		&ownerEmail,
		&ownerName,
		&device.PublicHost,
		&device.CreatedAt,
		&device.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return DesktopDevice{}, ErrNotFound
	}
	if err != nil {
		return DesktopDevice{}, err
	}
	if deviceName.Valid {
		device.DeviceName = strings.TrimSpace(deviceName.String)
	}
	if ownerEmail.Valid {
		device.OwnerEmail = ownerEmail.String
	}
	if ownerName.Valid {
		device.OwnerName = ownerName.String
	}
	return device, nil
}

func scanDesktopWebApp(row rowScanner) (DesktopWebApp, error) {
	var webApp DesktopWebApp
	err := row.Scan(
		&webApp.ID,
		&webApp.DeviceKey,
		&webApp.Name,
		&webApp.RouteID,
		&webApp.PublicHost,
		&webApp.TargetURL,
		&webApp.Active,
		&webApp.CreatedAt,
		&webApp.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return DesktopWebApp{}, ErrNotFound
	}
	if err != nil {
		return DesktopWebApp{}, err
	}
	return webApp, nil
}

func scanTrafficEvents(rows *sql.Rows) ([]TrafficEvent, error) {
	events := make([]TrafficEvent, 0)
	for rows.Next() {
		var event TrafficEvent
		var routeID sql.NullString
		var tokenID sql.NullString
		var deviceID sql.NullString
		var sessionID sql.NullString
		err := rows.Scan(
			&event.ID,
			&event.ObjectType,
			&event.PublicHost,
			&routeID,
			&tokenID,
			&deviceID,
			&sessionID,
			&event.Kind,
			&event.Method,
			&event.Path,
			&event.StatusCode,
			&event.BytesIn,
			&event.BytesOut,
			&event.Error,
			&event.OccurredAt,
		)
		if err != nil {
			return nil, err
		}
		if routeID.Valid {
			event.RouteID = routeID.String
		}
		if tokenID.Valid {
			event.TokenID = tokenID.String
		}
		if deviceID.Valid {
			event.DeviceID = deviceID.String
		}
		if sessionID.Valid {
			event.SessionID = sessionID.String
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func scanTrafficStatsWithKey(row rowScanner, key *string) (TrafficStats, error) {
	var stats TrafficStats
	var lastAt sql.NullTime
	if err := row.Scan(key, &stats.RequestCount, &stats.BytesIn, &stats.BytesOut, &lastAt); err != nil {
		return TrafficStats{}, err
	}
	if lastAt.Valid {
		stats.LastAt = &lastAt.Time
	}
	return stats, nil
}

func scanTrafficStatsNoKey(row rowScanner) (TrafficStats, error) {
	var stats TrafficStats
	var lastAt sql.NullTime
	if err := row.Scan(&stats.RequestCount, &stats.BytesIn, &stats.BytesOut, &lastAt); err != nil {
		return TrafficStats{}, err
	}
	if lastAt.Valid {
		stats.LastAt = &lastAt.Time
	}
	return stats, nil
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

func scanDesktopSession(row rowScanner) (DesktopSession, error) {
	var session DesktopSession
	var disconnected sql.NullTime
	err := row.Scan(&session.ID, &session.DeviceKey, &session.DeviceID, &session.RemoteAddr, &session.ConnectedAt, &disconnected)
	if errors.Is(err, sql.ErrNoRows) {
		return DesktopSession{}, ErrNotFound
	}
	if err != nil {
		return DesktopSession{}, err
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
	return nullableString(tokenID)
}

func nullableString(value string) any {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return value
}
