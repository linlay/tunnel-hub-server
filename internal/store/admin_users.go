package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"

	"example.invalid/tunnel-hub-server/internal/auth"
)

var (
	ErrUserNotFound    = errors.New("admin user not found")
	ErrUserInactive    = errors.New("admin user inactive")
	ErrInvalidPassword = errors.New("invalid password")
	ErrSessionNotFound = errors.New("admin session not found")
	ErrSessionExpired  = errors.New("admin session expired")
	ErrLastActiveUser  = errors.New("cannot disable the last active admin user")
)

type AdminUser struct {
	ID          string     `json:"id"`
	Username    string     `json:"username"`
	Status      string     `json:"status"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
	LastLoginAt *time.Time `json:"lastLoginAt,omitempty"`
}

type AdminUserWithPassword struct {
	AdminUser
	PasswordHash string `json:"-"`
}

type AdminSession struct {
	ID         string
	UserID     string
	Token      string
	ExpiresAt  time.Time
	CreatedAt  time.Time
	LastSeenAt time.Time
}

type AdminUserPatch struct {
	Username *string
	Password *string
	Status   *string
}

func (db *DB) EnsureAdminUser(ctx context.Context, username, password string) (AdminUser, bool, error) {
	username = normalizeUsername(username)
	if username == "" {
		username = "admin"
	}
	user, err := db.GetAdminUserByUsername(ctx, username)
	if err == nil {
		return user.AdminUser, false, nil
	}
	if !errors.Is(err, ErrUserNotFound) {
		return AdminUser{}, false, err
	}
	created, err := db.CreateAdminUser(ctx, username, password)
	if isDuplicateKey(err) {
		existing, lookupErr := db.GetAdminUserByUsername(ctx, username)
		return existing.AdminUser, false, lookupErr
	}
	return created, err == nil, err
}

func (db *DB) AdminUserCount(ctx context.Context) (int64, error) {
	var count int64
	err := db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM admin_users`).Scan(&count)
	return count, err
}

func (db *DB) CreateAdminUser(ctx context.Context, username, password string) (AdminUser, error) {
	return db.createAdminUser(ctx, username, password, "active")
}

func (db *DB) CreateAdminUserWithStatus(ctx context.Context, username, password, status string) (AdminUser, error) {
	return db.createAdminUser(ctx, username, password, status)
}

func (db *DB) createAdminUser(ctx context.Context, username, password, status string) (AdminUser, error) {
	username = normalizeUsername(username)
	if err := ValidateTextLength("username", username, 255); err != nil {
		return AdminUser{}, err
	}
	if username == "" {
		return AdminUser{}, errors.New("username is required")
	}
	if strings.TrimSpace(password) == "" {
		return AdminUser{}, errors.New("password is required")
	}
	status, err := normalizeAdminUserStatus(status)
	if err != nil {
		return AdminUser{}, err
	}
	hash, err := auth.HashSecret(password)
	if err != nil {
		return AdminUser{}, err
	}
	now := databaseTime(time.Now())
	result, err := db.sql.ExecContext(ctx, `
		INSERT INTO admin_users (username, password_hash, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
	`, username, hash, status, now, now)
	if err != nil {
		return AdminUser{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return AdminUser{}, err
	}
	return AdminUser{
		ID:        strconv.FormatInt(id, 10),
		Username:  username,
		Status:    status,
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

func (db *DB) ListAdminUsers(ctx context.Context) ([]AdminUser, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT CAST(id AS CHAR), username, status, created_at, updated_at, last_login_at
		FROM admin_users
		ORDER BY username ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	users := make([]AdminUser, 0)
	for rows.Next() {
		user, err := scanAdminUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, user)
	}
	return users, rows.Err()
}

func (db *DB) GetAdminUser(ctx context.Context, id string) (AdminUserWithPassword, error) {
	row := db.sql.QueryRowContext(ctx, `
		SELECT CAST(id AS CHAR), username, password_hash, status, created_at, updated_at, last_login_at
		FROM admin_users
		WHERE id = ?
	`, strings.TrimSpace(id))
	return scanAdminUserWithPassword(row)
}

func (db *DB) UpdateAdminUser(ctx context.Context, id string, patch AdminUserPatch) (AdminUser, error) {
	id = strings.TrimSpace(id)
	tx, err := db.sql.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return AdminUser{}, err
	}
	defer tx.Rollback()
	// Admin users are few. Lock in primary-key order so two concurrent disables
	// cannot each observe the other as the last remaining active user.
	rows, err := tx.QueryContext(ctx, `SELECT CAST(id AS CHAR), username, password_hash, status, created_at, updated_at, last_login_at FROM admin_users ORDER BY id FOR UPDATE`)
	if err != nil {
		return AdminUser{}, err
	}
	var user AdminUserWithPassword
	active := 0
	for rows.Next() {
		candidate, scanErr := scanAdminUserWithPassword(rows)
		if scanErr != nil {
			rows.Close()
			return AdminUser{}, scanErr
		}
		if candidate.Status == "active" {
			active++
		}
		if candidate.ID == id {
			user = candidate
		}
	}
	scanErr := rows.Err()
	rows.Close()
	if scanErr != nil {
		return AdminUser{}, scanErr
	}
	if user.ID == "" {
		return AdminUser{}, ErrUserNotFound
	}

	username := user.Username
	if patch.Username != nil {
		username = normalizeUsername(*patch.Username)
		if username == "" {
			return AdminUser{}, errors.New("username is required")
		}
	}

	if err := ValidateTextLength("username", username, 255); err != nil {
		return AdminUser{}, err
	}

	status := user.Status
	if patch.Status != nil {
		status, err = normalizeAdminUserStatus(*patch.Status)
		if err != nil {
			return AdminUser{}, err
		}
		if user.Status == "active" && status == "disabled" {
			if active <= 1 {
				return AdminUser{}, ErrLastActiveUser
			}
		}
	}

	passwordHash := user.PasswordHash
	if patch.Password != nil {
		if strings.TrimSpace(*patch.Password) == "" {
			return AdminUser{}, errors.New("password cannot be empty")
		}
		passwordHash, err = auth.HashSecret(*patch.Password)
		if err != nil {
			return AdminUser{}, err
		}
	}

	now := databaseTime(time.Now())
	result, err := tx.ExecContext(ctx, `
		UPDATE admin_users
		SET username = ?, password_hash = ?, status = ?, updated_at = ?
		WHERE id = ?
	`, username, passwordHash, status, now, strings.TrimSpace(id))
	if err != nil {
		return AdminUser{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return AdminUser{}, err
	}
	if affected == 0 {
		return AdminUser{}, ErrUserNotFound
	}
	if err := tx.Commit(); err != nil {
		return AdminUser{}, err
	}
	updated, err := db.GetAdminUser(ctx, id)
	if err != nil {
		return AdminUser{}, err
	}
	return updated.AdminUser, nil
}

func (db *DB) DisableAdminUser(ctx context.Context, id string) (AdminUser, error) {
	status := "disabled"
	return db.UpdateAdminUser(ctx, id, AdminUserPatch{Status: &status})
}

func (db *DB) GetAdminUserByUsername(ctx context.Context, username string) (AdminUserWithPassword, error) {
	row := db.sql.QueryRowContext(ctx, `
		SELECT CAST(id AS CHAR), username, password_hash, status, created_at, updated_at, last_login_at
		FROM admin_users
		WHERE username = ?
	`, normalizeUsername(username))
	return scanAdminUserWithPassword(row)
}

func (db *DB) VerifyAdminLogin(ctx context.Context, username, password string) (AdminUser, error) {
	user, err := db.GetAdminUserByUsername(ctx, username)
	if err != nil {
		return AdminUser{}, err
	}
	if user.Status != "active" {
		return AdminUser{}, ErrUserInactive
	}
	if !auth.VerifySecret(password, user.PasswordHash) {
		return AdminUser{}, ErrInvalidPassword
	}
	now := databaseTime(time.Now())
	_, err = db.sql.ExecContext(ctx, `
		UPDATE admin_users
		SET last_login_at = ?, updated_at = ?
		WHERE id = ?
	`, now, now, user.ID)
	if err != nil {
		return AdminUser{}, err
	}
	user.LastLoginAt = &now
	user.UpdatedAt = now
	return user.AdminUser, nil
}

func (db *DB) CreateAdminSession(ctx context.Context, userID string, ttl time.Duration) (AdminSession, error) {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	token, err := newRandomSessionToken()
	if err != nil {
		return AdminSession{}, err
	}
	now := databaseTime(time.Now())
	session := AdminSession{
		ID:         newID("admin_session"),
		UserID:     strings.TrimSpace(userID),
		Token:      token,
		ExpiresAt:  now.Add(ttl),
		CreatedAt:  now,
		LastSeenAt: now,
	}
	_, err = db.sql.ExecContext(ctx, `
		INSERT INTO admin_sessions (id, user_id, session_hash, expires_at, created_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, session.ID, session.UserID, hashSessionToken(token), session.ExpiresAt, session.CreatedAt, session.LastSeenAt)
	if err != nil {
		return AdminSession{}, err
	}
	return session, nil
}

func (db *DB) AuthenticateAdminSession(ctx context.Context, token string, now time.Time) (AdminUser, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return AdminUser{}, ErrSessionNotFound
	}
	row := db.sql.QueryRowContext(ctx, `
		SELECT CAST(u.id AS CHAR), u.username, u.status, u.created_at, u.updated_at, u.last_login_at, s.expires_at
		FROM admin_sessions s
		JOIN admin_users u ON u.id = s.user_id
		WHERE s.session_hash = ?
	`, hashSessionToken(token))
	user, expiresAt, err := scanAdminSessionUser(row)
	if err != nil {
		return AdminUser{}, err
	}
	if !expiresAt.After(now) {
		_ = db.DeleteAdminSession(ctx, token)
		return AdminUser{}, ErrSessionExpired
	}
	if user.Status != "active" {
		return AdminUser{}, ErrUserInactive
	}
	_, _ = db.sql.ExecContext(ctx, `
		UPDATE admin_sessions
		SET last_seen_at = ?
		WHERE session_hash = ?
	`, databaseTime(now), hashSessionToken(token))
	return user, nil
}

func (db *DB) DeleteAdminSession(ctx context.Context, token string) error {
	_, err := db.sql.ExecContext(ctx, `DELETE FROM admin_sessions WHERE session_hash = ?`, hashSessionToken(token))
	return err
}

func scanAdminUser(scanner rowScanner) (AdminUser, error) {
	var user AdminUser
	var lastLoginAt sql.NullTime
	err := scanner.Scan(&user.ID, &user.Username, &user.Status, &user.CreatedAt, &user.UpdatedAt, &lastLoginAt)
	if errors.Is(err, sql.ErrNoRows) {
		return AdminUser{}, ErrUserNotFound
	}
	if err != nil {
		return AdminUser{}, err
	}
	if lastLoginAt.Valid {
		user.LastLoginAt = &lastLoginAt.Time
	}
	return user, nil
}

func scanAdminUserWithPassword(scanner rowScanner) (AdminUserWithPassword, error) {
	var user AdminUserWithPassword
	var lastLoginAt sql.NullTime
	err := scanner.Scan(&user.ID, &user.Username, &user.PasswordHash, &user.Status, &user.CreatedAt, &user.UpdatedAt, &lastLoginAt)
	if errors.Is(err, sql.ErrNoRows) {
		return AdminUserWithPassword{}, ErrUserNotFound
	}
	if err != nil {
		return AdminUserWithPassword{}, err
	}
	if lastLoginAt.Valid {
		user.LastLoginAt = &lastLoginAt.Time
	}
	return user, nil
}

func scanAdminSessionUser(scanner rowScanner) (AdminUser, time.Time, error) {
	var user AdminUser
	var expiresAt time.Time
	var lastLoginAt sql.NullTime
	err := scanner.Scan(&user.ID, &user.Username, &user.Status, &user.CreatedAt, &user.UpdatedAt, &lastLoginAt, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return AdminUser{}, time.Time{}, ErrSessionNotFound
	}
	if err != nil {
		return AdminUser{}, time.Time{}, err
	}
	if lastLoginAt.Valid {
		user.LastLoginAt = &lastLoginAt.Time
	}
	return user, expiresAt, nil
}

func normalizeUsername(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func normalizeAdminUserStatus(value string) (string, error) {
	status := strings.ToLower(strings.TrimSpace(value))
	if status == "" {
		status = "active"
	}
	if status != "active" && status != "disabled" {
		return "", errors.New("status must be active or disabled")
	}
	return status, nil
}

func newRandomSessionToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func hashSessionToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
