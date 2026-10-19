package service

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/taskflow/taskflow/internal/auth"
	"github.com/taskflow/taskflow/internal/db"
	"github.com/taskflow/taskflow/internal/domain"
	"github.com/taskflow/taskflow/internal/engine"
	"github.com/taskflow/taskflow/internal/store"
)

// Auth implements registration, login, sessions and member management.
type Auth struct {
	pool       *pgxpool.Pool
	eng        *engine.Engine
	sessionTTL time.Duration
	now        func() time.Time
}

func NewAuth(pool *pgxpool.Pool, eng *engine.Engine, sessionTTL time.Duration) *Auth {
	return &Auth{pool: pool, eng: eng, sessionTTL: sessionTTL, now: func() time.Time { return time.Now().UTC() }}
}

// ErrInvalidCredentials is deliberately generic: it does not reveal whether
// the email exists or the account is disabled.
var ErrInvalidCredentials = errors.New("invalid email or password")

// Session is a freshly issued session.
type Session struct {
	Token     string
	CSRFToken string
	ExpiresAt time.Time
	User      *domain.User
}

func normalizeEmail(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func validateEmail(email string) bool {
	if len(email) < 3 || len(email) > 254 {
		return false
	}
	addr, err := mail.ParseAddress(email)
	return err == nil && addr.Address == email
}

// RegisterInput creates a workspace and its first (admin) user.
type RegisterInput struct {
	Name      string `json:"name"`
	Email     string `json:"email"`
	Password  string `json:"password"`
	Workspace string `json:"workspace"`
}

// Register creates a workspace with the caller as admin and signs them in.
func (a *Auth) Register(ctx context.Context, in RegisterInput, ip, ua string) (*Session, error) {
	in.Email = normalizeEmail(in.Email)
	in.Name = strings.TrimSpace(in.Name)
	in.Workspace = strings.TrimSpace(in.Workspace)
	v := &domain.Validator{}
	v.Check(in.Name != "" && utf8.RuneCountInString(in.Name) <= 100, "name", "Enter your name (max 100 characters).")
	v.Check(validateEmail(in.Email), "email", "Enter a valid email address.")
	if msg := auth.PasswordProblem(in.Password); msg != "" {
		v.Add("password", msg)
	}
	if in.Workspace == "" {
		in.Workspace = in.Name + "'s workspace"
	}
	v.Check(utf8.RuneCountInString(in.Workspace) <= 100, "workspace", "Workspace name must be at most 100 characters.")
	if err := v.Err(); err != nil {
		return nil, err
	}
	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		return nil, err
	}
	now := a.now()
	tenant := &domain.Tenant{ID: uuid.New(), Name: in.Workspace, CreatedAt: now}
	user := &domain.User{ID: uuid.New(), TenantID: tenant.ID, Email: in.Email, Name: in.Name,
		PasswordHash: hash, Role: domain.RoleAdmin, CreatedAt: now}
	var sess *Session
	err = db.WithTx(ctx, a.pool, func(tx pgx.Tx) error {
		if err := store.InsertTenant(ctx, tx, tenant); err != nil {
			return err
		}
		if err := store.InsertUser(ctx, tx, user); err != nil {
			var ce *domain.ConflictError
			if errors.As(err, &ce) {
				return &domain.ValidationError{Fields: map[string]string{"email": "An account with this email already exists."}}
			}
			return err
		}
		p := principalFor(user, ip, ua)
		if err := a.eng.Audit(ctx, tx, p, "auth.register", "user", user.ID.String(), map[string]any{"workspace": tenant.Name}); err != nil {
			return err
		}
		sess, err = a.issue(ctx, tx, user, ip, ua)
		return err
	})
	return sess, err
}

// Login verifies credentials and issues a session.
func (a *Auth) Login(ctx context.Context, email, password, ip, ua string) (*Session, error) {
	email = normalizeEmail(email)
	if email == "" || password == "" || len(password) > 256 {
		auth.VerifyDummy(password)
		return nil, ErrInvalidCredentials
	}
	u, err := store.GetUserByEmail(ctx, a.pool, email)
	if errors.Is(err, domain.ErrNotFound) {
		auth.VerifyDummy(password)
		a.auditAnon(ctx, "auth.login_failed", email, ip, ua)
		return nil, ErrInvalidCredentials
	}
	if err != nil {
		return nil, err
	}
	ok, err := auth.VerifyPassword(password, u.PasswordHash)
	if err != nil || !ok || u.DisabledAt != nil {
		_ = a.eng.Audit(ctx, a.pool, principalFor(u, ip, ua), "auth.login_failed", "user", u.ID.String(), nil)
		return nil, ErrInvalidCredentials
	}
	var sess *Session
	err = db.WithTx(ctx, a.pool, func(tx pgx.Tx) error {
		if err := store.TouchLogin(ctx, tx, u.ID, a.now()); err != nil {
			return err
		}
		if err := a.eng.Audit(ctx, tx, principalFor(u, ip, ua), "auth.login", "user", u.ID.String(), nil); err != nil {
			return err
		}
		sess, err = a.issue(ctx, tx, u, ip, ua)
		return err
	})
	return sess, err
}

func (a *Auth) auditAnon(ctx context.Context, action, email, ip, ua string) {
	_ = a.eng.Audit(ctx, a.pool, nil, action, "email", email, map[string]any{"ip": ip})
}

func (a *Auth) issue(ctx context.Context, tx pgx.Tx, u *domain.User, ip, ua string) (*Session, error) {
	token, err := auth.NewToken()
	if err != nil {
		return nil, err
	}
	csrf, err := auth.NewToken()
	if err != nil {
		return nil, err
	}
	now := a.now()
	if len(ua) > 512 {
		ua = ua[:512]
	}
	s := &store.Session{ID: uuid.New(), UserID: u.ID, CSRFToken: csrf, ExpiresAt: now.Add(a.sessionTTL)}
	if err := store.InsertSession(ctx, tx, s, auth.HashToken(token), ip, ua, now); err != nil {
		return nil, err
	}
	return &Session{Token: token, CSRFToken: csrf, ExpiresAt: s.ExpiresAt, User: u}, nil
}

// Authenticate resolves a session token to a principal, sliding the session's
// expiry at most once a minute to avoid a write per request.
func (a *Auth) Authenticate(ctx context.Context, token, ip, ua string) (*domain.Principal, *domain.User, error) {
	if token == "" || len(token) > 100 {
		return nil, nil, domain.ErrUnauthorized
	}
	now := a.now()
	s, u, err := store.SessionUser(ctx, a.pool, auth.HashToken(token), now)
	if err != nil {
		return nil, nil, err
	}
	if u.DisabledAt != nil {
		return nil, nil, domain.ErrUnauthorized
	}
	if now.Sub(s.LastSeenAt) > time.Minute {
		_ = store.TouchSession(ctx, a.pool, s.ID, now, now.Add(a.sessionTTL))
	}
	p := principalFor(u, ip, ua)
	p.SessionID = s.ID
	p.CSRFToken = s.CSRFToken
	return p, u, nil
}

// Logout revokes the current session.
func (a *Auth) Logout(ctx context.Context, p *domain.Principal) error {
	if err := store.DeleteSession(ctx, a.pool, p.SessionID); err != nil {
		return err
	}
	return a.eng.Audit(ctx, a.pool, p, "auth.logout", "user", p.UserID.String(), nil)
}

// ChangePassword verifies the current password, sets a new one and revokes
// all other sessions of the user.
func (a *Auth) ChangePassword(ctx context.Context, p *domain.Principal, current, next string) error {
	u, err := store.GetUser(ctx, a.pool, p.TenantID, p.UserID)
	if err != nil {
		return err
	}
	ok, err := auth.VerifyPassword(current, u.PasswordHash)
	if err != nil || !ok {
		return &domain.ValidationError{Fields: map[string]string{"current_password": "Current password is incorrect."}}
	}
	if msg := auth.PasswordProblem(next); msg != "" {
		return &domain.ValidationError{Fields: map[string]string{"new_password": msg}}
	}
	hash, err := auth.HashPassword(next)
	if err != nil {
		return err
	}
	return db.WithTx(ctx, a.pool, func(tx pgx.Tx) error {
		if err := store.UpdatePassword(ctx, tx, u.ID, hash, a.now()); err != nil {
			return err
		}
		if err := store.DeleteUserSessions(ctx, tx, u.ID, &p.SessionID); err != nil {
			return err
		}
		return a.eng.Audit(ctx, tx, p, "auth.password_changed", "user", u.ID.String(), nil)
	})
}

// UserInput creates a member (admin only).
type UserInput struct {
	Name     string      `json:"name"`
	Email    string      `json:"email"`
	Password string      `json:"password"`
	Role     domain.Role `json:"role"`
}

// CreateUser adds a member to the caller's workspace.
func (a *Auth) CreateUser(ctx context.Context, p *domain.Principal, in UserInput) (*domain.User, error) {
	in.Email = normalizeEmail(in.Email)
	in.Name = strings.TrimSpace(in.Name)
	v := &domain.Validator{}
	v.Check(in.Name != "" && utf8.RuneCountInString(in.Name) <= 100, "name", "Enter a name (max 100 characters).")
	v.Check(validateEmail(in.Email), "email", "Enter a valid email address.")
	v.Check(in.Role.Valid(), "role", "Role must be admin, operator or viewer.")
	if msg := auth.PasswordProblem(in.Password); msg != "" {
		v.Add("password", msg)
	}
	if err := v.Err(); err != nil {
		return nil, err
	}
	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		return nil, err
	}
	u := &domain.User{ID: uuid.New(), TenantID: p.TenantID, Email: in.Email, Name: in.Name,
		PasswordHash: hash, Role: in.Role, CreatedAt: a.now()}
	err = db.WithTx(ctx, a.pool, func(tx pgx.Tx) error {
		if err := store.InsertUser(ctx, tx, u); err != nil {
			var ce *domain.ConflictError
			if errors.As(err, &ce) {
				return &domain.ValidationError{Fields: map[string]string{"email": "An account with this email already exists."}}
			}
			return err
		}
		return a.eng.Audit(ctx, tx, p, "user.create", "user", u.ID.String(), map[string]any{"email": u.Email, "role": u.Role})
	})
	if err != nil {
		return nil, err
	}
	return u, nil
}

// UpdateUserInput changes a member's role or status.
type UpdateUserInput struct {
	Role     *domain.Role `json:"role,omitempty"`
	Disabled *bool        `json:"disabled,omitempty"`
}

// UpdateUser changes a member's role or disables them. A workspace always
// keeps at least one enabled admin, and admins cannot lock themselves out.
func (a *Auth) UpdateUser(ctx context.Context, p *domain.Principal, id uuid.UUID, in UpdateUserInput) (*domain.User, error) {
	var out *domain.User
	err := db.WithTx(ctx, a.pool, func(tx pgx.Tx) error {
		// Serialise membership changes per workspace so two admins cannot
		// demote each other concurrently and leave the workspace adminless.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "members:"+p.TenantID.String()); err != nil {
			return err
		}
		u, err := store.GetUser(ctx, tx, p.TenantID, id)
		if err != nil {
			return err
		}
		role, disabledAt := u.Role, u.DisabledAt
		if in.Role != nil {
			if !in.Role.Valid() {
				return &domain.ValidationError{Fields: map[string]string{"role": "Role must be admin, operator or viewer."}}
			}
			role = *in.Role
		}
		if in.Disabled != nil {
			if *in.Disabled && disabledAt == nil {
				now := a.now()
				disabledAt = &now
			} else if !*in.Disabled {
				disabledAt = nil
			}
		}
		if id == p.UserID && (role != domain.RoleAdmin || disabledAt != nil) {
			return domain.Conflict("self_lockout", "you cannot remove your own admin access")
		}
		if u.Role == domain.RoleAdmin && u.DisabledAt == nil && (role != domain.RoleAdmin || disabledAt != nil) {
			n, err := store.CountActiveAdmins(ctx, tx, p.TenantID)
			if err != nil {
				return err
			}
			if n <= 1 {
				return domain.Conflict("last_admin", "the workspace must keep at least one active admin")
			}
		}
		if err := store.UpdateUserAccess(ctx, tx, p.TenantID, id, role, disabledAt, a.now()); err != nil {
			return err
		}
		if disabledAt != nil {
			if err := store.DeleteUserSessions(ctx, tx, id, nil); err != nil {
				return err
			}
		}
		u.Role, u.DisabledAt = role, disabledAt
		out = u
		return a.eng.Audit(ctx, tx, p, "user.update", "user", id.String(), map[string]any{
			"email": u.Email, "role": role, "disabled": disabledAt != nil})
	})
	return out, err
}

// ListUsers returns the workspace members.
func (a *Auth) ListUsers(ctx context.Context, p *domain.Principal) ([]*domain.User, error) {
	users, err := store.ListUsers(ctx, a.pool, p.TenantID)
	return nonNil(users), err
}

// MarkNotificationsSeen records that the user opened the notification panel.
func (a *Auth) MarkNotificationsSeen(ctx context.Context, p *domain.Principal) error {
	return store.SetNotificationsSeen(ctx, a.pool, p.UserID, a.now())
}

func principalFor(u *domain.User, ip, ua string) *domain.Principal {
	return &domain.Principal{UserID: u.ID, TenantID: u.TenantID, Email: u.Email, Role: u.Role, IP: ip, UserAgent: ua}
}

// SeedUser creates a workspace + user directly (used by the seed command).
func (a *Auth) SeedUser(ctx context.Context, tenantName, name, email, password string, role domain.Role, tenantID *uuid.UUID) (*domain.User, error) {
	hash, err := auth.HashPassword(password)
	if err != nil {
		return nil, err
	}
	now := a.now()
	u := &domain.User{ID: uuid.New(), Email: normalizeEmail(email), Name: name, PasswordHash: hash, Role: role, CreatedAt: now}
	err = db.WithTx(ctx, a.pool, func(tx pgx.Tx) error {
		if tenantID == nil {
			t := &domain.Tenant{ID: uuid.New(), Name: tenantName, CreatedAt: now}
			if err := store.InsertTenant(ctx, tx, t); err != nil {
				return err
			}
			u.TenantID = t.ID
		} else {
			u.TenantID = *tenantID
		}
		if err := store.InsertUser(ctx, tx, u); err != nil {
			return fmt.Errorf("seed user %s: %w", email, err)
		}
		return nil
	})
	return u, err
}
