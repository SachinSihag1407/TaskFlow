package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/taskflow/taskflow/internal/db"
	"github.com/taskflow/taskflow/internal/domain"
)

const userColumns = `u.id, u.tenant_id, u.email, u.name, u.password_hash, u.role, u.notifications_seen_at,
	u.last_login_at, u.disabled_at, u.created_at`

func scanUser(row pgx.Row) (*domain.User, error) {
	var u domain.User
	err := row.Scan(&u.ID, &u.TenantID, &u.Email, &u.Name, &u.PasswordHash, &u.Role, &u.NotificationsSeenAt,
		&u.LastLoginAt, &u.DisabledAt, &u.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// InsertTenant creates a workspace and its default settings.
func InsertTenant(ctx context.Context, q db.DBTX, t *domain.Tenant) error {
	if _, err := q.Exec(ctx, `INSERT INTO tenants (id, name, created_at) VALUES ($1,$2,$3)`, t.ID, t.Name, t.CreatedAt); err != nil {
		return err
	}
	_, err := q.Exec(ctx, `INSERT INTO tenant_settings (tenant_id) VALUES ($1)`, t.ID)
	return err
}

// GetTenant loads a workspace.
func GetTenant(ctx context.Context, q db.DBTX, id uuid.UUID) (*domain.Tenant, error) {
	var t domain.Tenant
	err := q.QueryRow(ctx, `SELECT id, name, created_at FROM tenants WHERE id = $1`, id).Scan(&t.ID, &t.Name, &t.CreatedAt)
	if db.IsNoRows(err) {
		return nil, domain.ErrNotFound
	}
	return &t, err
}

// InsertUser creates a user. Returns a conflict error if the email is taken.
func InsertUser(ctx context.Context, q db.DBTX, u *domain.User) error {
	_, err := q.Exec(ctx, `INSERT INTO users (id, tenant_id, email, name, password_hash, role, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$7)`, u.ID, u.TenantID, u.Email, u.Name, u.PasswordHash, u.Role, u.CreatedAt)
	if db.IsUniqueViolation(err, "users_email_key") {
		return domain.Conflict("email_taken", "an account with this email already exists")
	}
	return err
}

// GetUserByEmail loads a user by case-insensitive email.
func GetUserByEmail(ctx context.Context, q db.DBTX, email string) (*domain.User, error) {
	u, err := scanUser(q.QueryRow(ctx, `SELECT `+userColumns+` FROM users u WHERE lower(u.email) = lower($1)`, email))
	if db.IsNoRows(err) {
		return nil, domain.ErrNotFound
	}
	return u, err
}

// GetUser loads a user scoped to a tenant.
func GetUser(ctx context.Context, q db.DBTX, tenantID, id uuid.UUID) (*domain.User, error) {
	u, err := scanUser(q.QueryRow(ctx, `SELECT `+userColumns+` FROM users u WHERE u.id = $1 AND u.tenant_id = $2`, id, tenantID))
	if db.IsNoRows(err) {
		return nil, domain.ErrNotFound
	}
	return u, err
}

// ListUsers returns a tenant's members.
func ListUsers(ctx context.Context, q db.DBTX, tenantID uuid.UUID) ([]*domain.User, error) {
	rows, err := q.Query(ctx, `SELECT `+userColumns+` FROM users u WHERE u.tenant_id = $1 ORDER BY u.created_at`, tenantID)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanUser)
}

// UpdateUserAccess changes a member's role or disabled state.
func UpdateUserAccess(ctx context.Context, q db.DBTX, tenantID, id uuid.UUID, role domain.Role, disabledAt *time.Time, now time.Time) error {
	tag, err := q.Exec(ctx, `UPDATE users SET role = $3, disabled_at = $4, updated_at = $5 WHERE id = $1 AND tenant_id = $2`,
		id, tenantID, role, disabledAt, now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// CountActiveAdmins returns the number of enabled admins in a tenant.
func CountActiveAdmins(ctx context.Context, q db.DBTX, tenantID uuid.UUID) (int, error) {
	var n int
	err := q.QueryRow(ctx, `SELECT count(*) FROM users WHERE tenant_id = $1 AND role = 'admin' AND disabled_at IS NULL`, tenantID).Scan(&n)
	return n, err
}

// UpdatePassword replaces a user's password hash.
func UpdatePassword(ctx context.Context, q db.DBTX, id uuid.UUID, hash string, now time.Time) error {
	_, err := q.Exec(ctx, `UPDATE users SET password_hash = $2, updated_at = $3 WHERE id = $1`, id, hash, now)
	return err
}

// TouchLogin records a successful login.
func TouchLogin(ctx context.Context, q db.DBTX, id uuid.UUID, now time.Time) error {
	_, err := q.Exec(ctx, `UPDATE users SET last_login_at = $2 WHERE id = $1`, id, now)
	return err
}

// SetNotificationsSeen records when the user last opened notifications.
func SetNotificationsSeen(ctx context.Context, q db.DBTX, id uuid.UUID, now time.Time) error {
	_, err := q.Exec(ctx, `UPDATE users SET notifications_seen_at = $2 WHERE id = $1`, id, now)
	return err
}

// Session is a stored login session.
type Session struct {
	ID         uuid.UUID
	UserID     uuid.UUID
	CSRFToken  string
	ExpiresAt  time.Time
	LastSeenAt time.Time
}

// InsertSession stores a new session keyed by the token's hash.
func InsertSession(ctx context.Context, q db.DBTX, s *Session, tokenHash []byte, ip, ua string, now time.Time) error {
	_, err := q.Exec(ctx, `INSERT INTO sessions (id, user_id, token_hash, csrf_token, ip, user_agent, created_at, last_seen_at, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$7,$8)`, s.ID, s.UserID, tokenHash, s.CSRFToken, ip, ua, now, s.ExpiresAt)
	return err
}

// SessionUser loads a live session and its user by token hash.
func SessionUser(ctx context.Context, q db.DBTX, tokenHash []byte, now time.Time) (*Session, *domain.User, error) {
	var s Session
	row := q.QueryRow(ctx, `SELECT s.id, s.user_id, s.csrf_token, s.expires_at, s.last_seen_at, `+userColumns+`
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1 AND s.expires_at > $2`, tokenHash, now)
	var u domain.User
	err := row.Scan(&s.ID, &s.UserID, &s.CSRFToken, &s.ExpiresAt, &s.LastSeenAt,
		&u.ID, &u.TenantID, &u.Email, &u.Name, &u.PasswordHash, &u.Role, &u.NotificationsSeenAt,
		&u.LastLoginAt, &u.DisabledAt, &u.CreatedAt)
	if db.IsNoRows(err) {
		return nil, nil, domain.ErrUnauthorized
	}
	if err != nil {
		return nil, nil, err
	}
	return &s, &u, nil
}

// TouchSession slides a session's last-seen time (and optionally expiry).
func TouchSession(ctx context.Context, q db.DBTX, id uuid.UUID, now, expires time.Time) error {
	_, err := q.Exec(ctx, `UPDATE sessions SET last_seen_at = $2, expires_at = $3 WHERE id = $1`, id, now, expires)
	return err
}

// DeleteSession removes one session (logout).
func DeleteSession(ctx context.Context, q db.DBTX, id uuid.UUID) error {
	_, err := q.Exec(ctx, `DELETE FROM sessions WHERE id = $1`, id)
	return err
}

// DeleteUserSessions revokes all sessions of a user, optionally keeping one.
func DeleteUserSessions(ctx context.Context, q db.DBTX, userID uuid.UUID, keep *uuid.UUID) error {
	_, err := q.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1 AND ($2::uuid IS NULL OR id <> $2)`, userID, keep)
	return err
}

// DeleteExpiredSessions purges expired sessions.
func DeleteExpiredSessions(ctx context.Context, q db.DBTX, now time.Time) (int64, error) {
	tag, err := q.Exec(ctx, `DELETE FROM sessions WHERE expires_at < $1`, now)
	return tag.RowsAffected(), err
}
