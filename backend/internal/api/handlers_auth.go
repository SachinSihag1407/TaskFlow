package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/taskflow/taskflow/internal/domain"
	"github.com/taskflow/taskflow/internal/service"
)

type meResponse struct {
	User          *domain.User   `json:"user"`
	Workspace     *domain.Tenant `json:"workspace"`
	CSRFToken     string         `json:"csrf_token"`
	PlatformAdmin bool           `json:"platform_admin"`
}

func (s *Server) setSessionCookie(w http.ResponseWriter, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", Expires: expires,
		HttpOnly: true, Secure: s.cfg.CookieSecure, SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.cfg.CookieSecure, SameSite: http.SameSiteLaxMode})
}

func (s *Server) authConfig(w http.ResponseWriter, _ *http.Request) {
	ok(w, map[string]bool{"registration_enabled": s.cfg.AllowRegistration})
}

func (s *Server) sessionResponse(w http.ResponseWriter, r *http.Request, sess *service.Session, status int) {
	s.setSessionCookie(w, sess.Token, sess.ExpiresAt)
	p := &domain.Principal{TenantID: sess.User.TenantID}
	ws, err := s.ops.Tenant(r.Context(), p)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	pa := s.isPlatformAdmin(&domain.Principal{Role: sess.User.Role, Email: sess.User.Email})
	writeJSON(w, status, envelope{Data: meResponse{User: sess.User, Workspace: ws, CSRFToken: sess.CSRFToken, PlatformAdmin: pa}})
}

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.AllowRegistration {
		fail(w, r, http.StatusForbidden, "registration_disabled", "Self-service registration is disabled. Ask an admin for an account.", nil)
		return
	}
	var in service.RegisterInput
	if !s.decode(w, r, &in) {
		return
	}
	sess, err := s.auth.Register(r.Context(), in, s.clientIP(r), r.UserAgent())
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	s.sessionResponse(w, r, sess, http.StatusCreated)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !s.decode(w, r, &in) {
		return
	}
	// A second, per-account limit slows down distributed guessing against
	// one account from many IPs.
	if allowed, _ := s.limiter.Allow(r.Context(), "login_account:"+strings.ToLower(strings.TrimSpace(in.Email)), 20, 15*time.Minute); !allowed {
		fail(w, r, http.StatusTooManyRequests, "rate_limited", "Too many sign-in attempts for this account. Try again later.", nil)
		return
	}
	sess, err := s.auth.Login(r.Context(), in.Email, in.Password, s.clientIP(r), r.UserAgent())
	if errors.Is(err, service.ErrInvalidCredentials) {
		fail(w, r, http.StatusUnauthorized, "invalid_credentials", "Incorrect email or password.", nil)
		return
	}
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	s.sessionResponse(w, r, sess, http.StatusOK)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if err := s.auth.Logout(r.Context(), principal(r)); err != nil {
		s.failErr(w, r, err)
		return
	}
	s.clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	ws, err := s.ops.Tenant(r.Context(), p)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	ok(w, meResponse{User: currentUser(r), Workspace: ws, CSRFToken: p.CSRFToken, PlatformAdmin: s.isPlatformAdmin(p)})
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if !s.decode(w, r, &in) {
		return
	}
	if err := s.auth.ChangePassword(r.Context(), principal(r), in.CurrentPassword, in.NewPassword); err != nil {
		s.failErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.auth.ListUsers(r.Context(), principal(r))
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	ok(w, users)
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	var in service.UserInput
	if !s.decode(w, r, &in) {
		return
	}
	u, err := s.auth.CreateUser(r.Context(), principal(r), in)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	created(w, u)
}

func (s *Server) updateUser(w http.ResponseWriter, r *http.Request) {
	id, valid := pathUUID(w, r, "id")
	if !valid {
		return
	}
	var in service.UpdateUserInput
	if !s.decode(w, r, &in) {
		return
	}
	u, err := s.auth.UpdateUser(r.Context(), principal(r), id, in)
	if err != nil {
		s.failErr(w, r, err)
		return
	}
	ok(w, u)
}
