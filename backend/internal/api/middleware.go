package api

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/taskflow/taskflow/internal/domain"
	"github.com/taskflow/taskflow/internal/metrics"
)

type ctxKey int

const (
	keyRequestID ctxKey = iota
	keyPrincipal
	keyUser
	keyInfo
)

// reqInfo is filled in by inner layers (router, auth) and read by the outer
// logging middleware, which only holds the original *http.Request.
type reqInfo struct {
	route  string
	userID string
}

func info(r *http.Request) *reqInfo {
	if v, ok := r.Context().Value(keyInfo).(*reqInfo); ok {
		return v
	}
	return &reqInfo{}
}

const (
	sessionCookie = "tf_session"
	csrfHeader    = "X-CSRF-Token"
)

func requestID(r *http.Request) string {
	if v, ok := r.Context().Value(keyRequestID).(string); ok {
		return v
	}
	return ""
}

func principal(r *http.Request) *domain.Principal {
	p, _ := r.Context().Value(keyPrincipal).(*domain.Principal)
	return p
}

func currentUser(r *http.Request) *domain.User {
	u, _ := r.Context().Value(keyUser).(*domain.User)
	return u
}

// statusRecorder captures the response status for logs and metrics while
// preserving http.Flusher (needed by SSE) via Unwrap.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// clientIP returns the caller's address. Forwarded headers are honoured only
// when the API runs behind a trusted proxy (TRUST_PROXY_HEADERS=true), since
// otherwise clients could spoof them to evade rate limits and audit logs.
func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.TrustProxyHeaders {
		if v := strings.TrimSpace(r.Header.Get("X-Real-IP")); v != "" && net.ParseIP(v) != nil {
			return v
		}
		// Platforms such as Render append the client address to
		// X-Forwarded-For. Use the rightmost entry: earlier entries are
		// supplied by the client and can be forged.
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if v := strings.TrimSpace(parts[len(parts)-1]); net.ParseIP(v) != nil {
				return v
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// base wraps every request: request id, panic recovery, access log, metrics,
// security headers and write deadlines.
func (s *Server) base(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rid := r.Header.Get("X-Request-ID")
		if rid == "" || len(rid) > 64 || strings.ContainsAny(rid, "\r\n") {
			rid = uuid.NewString()
		}
		ri := &reqInfo{}
		ctx := context.WithValue(r.Context(), keyRequestID, rid)
		ctx = context.WithValue(ctx, keyInfo, ri)
		r = r.WithContext(ctx)
		rec := &statusRecorder{ResponseWriter: w}

		h := rec.Header()
		h.Set("X-Request-ID", rid)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		// API responses get a deny-everything CSP; the SPA (served by
		// static.go when STATIC_DIR is set) and Swagger UI set their own.
		if isAPIPath(r.URL.Path) && !strings.HasPrefix(r.URL.Path, "/api/docs") {
			h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		}
		if s.cfg.CookieSecure {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		if !strings.HasSuffix(r.URL.Path, "/events/stream") {
			// Bound normal requests; SSE streams manage their own lifetime.
			_ = http.NewResponseController(rec).SetWriteDeadline(time.Now().Add(30 * time.Second))
			h.Set("Cache-Control", "no-store")
		}

		defer func() {
			if p := recover(); p != nil {
				s.log.Error("panic serving request", "request_id", rid, "panic", fmt.Sprint(p), "stack", string(debug.Stack()))
				if rec.status == 0 {
					fail(rec, r, http.StatusInternalServerError, "internal_error", "Something went wrong.", nil)
				}
			}
			route := ri.route
			if route == "" {
				route = "unmatched"
			}
			status := rec.status
			if status == 0 {
				status = http.StatusOK
			}
			dur := time.Since(start)
			metrics.HTTPRequests.WithLabelValues(route, r.Method, strconv.Itoa(status/100)+"xx").Inc()
			metrics.HTTPDuration.WithLabelValues(route, r.Method).Observe(dur.Seconds())
			attrs := []any{"request_id", rid, "method", r.Method, "route", route, "status", status,
				"duration_ms", dur.Milliseconds(), "bytes", rec.bytes, "ip", s.clientIP(r)}
			if ri.userID != "" {
				attrs = append(attrs, "user_id", ri.userID)
			}
			if route == "GET /healthz" || route == "GET /readyz" || route == "GET /metrics" {
				return
			}
			if status >= 500 {
				s.log.Error("request", attrs...)
			} else {
				s.log.Info("request", attrs...)
			}
		}()
		next.ServeHTTP(rec, r)
	})
}

// cors allows credentialed requests only from explicitly configured origins.
// In the default deployment the SPA is served from the same origin (nginx
// proxies /api), so CORS is only needed for the Vite dev server.
func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && slices.Contains(s.cfg.AllowedOrigins, origin) {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Access-Control-Allow-Credentials", "true")
			h.Add("Vary", "Origin")
			if r.Method == http.MethodOptions {
				h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
				h.Set("Access-Control-Allow-Headers", "Content-Type, X-CSRF-Token, Idempotency-Key, X-Request-ID")
				h.Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func isSafeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// originCheck rejects state-changing requests whose Origin (or Referer) is
// neither this host nor an allowed origin — a defence-in-depth CSRF layer on
// top of SameSite cookies and the per-session CSRF token.
func (s *Server) originCheck(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isSafeMethod(r.Method) || !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		origin := r.Header.Get("Origin")
		if origin == "" {
			if ref := r.Header.Get("Referer"); ref != "" {
				if u, err := url.Parse(ref); err == nil {
					origin = u.Scheme + "://" + u.Host
				}
			}
		}
		if origin != "" && !slices.Contains(s.cfg.AllowedOrigins, origin) {
			u, err := url.Parse(origin)
			if err != nil || !strings.EqualFold(u.Host, r.Host) {
				fail(w, r, http.StatusForbidden, "origin_rejected", "Cross-origin request rejected.", nil)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// authenticate resolves the session cookie, if any, into a principal.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err == nil && c.Value != "" {
			p, u, err := s.auth.Authenticate(r.Context(), c.Value, s.clientIP(r), r.UserAgent())
			if err == nil {
				info(r).userID = p.UserID.String()
				ctx := context.WithValue(r.Context(), keyPrincipal, p)
				ctx = context.WithValue(ctx, keyUser, u)
				r = r.WithContext(ctx)
			} else if err != domain.ErrUnauthorized {
				s.failErr(w, r, err)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// authed requires a session and, for unsafe methods, a valid CSRF token bound
// to that session (synchronizer token pattern).
func (s *Server) authed(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := principal(r)
		if p == nil {
			fail(w, r, http.StatusUnauthorized, "unauthenticated", "Sign in to continue.", nil)
			return
		}
		if !isSafeMethod(r.Method) {
			tok := r.Header.Get(csrfHeader)
			if tok == "" || subtle.ConstantTimeCompare([]byte(tok), []byte(p.CSRFToken)) != 1 {
				fail(w, r, http.StatusForbidden, "csrf_failed", "Missing or invalid CSRF token. Reload the page and try again.", nil)
				return
			}
		}
		h(w, r)
	}
}

// role requires one of the given roles (and authentication).
func (s *Server) role(roles []domain.Role, h http.HandlerFunc) http.HandlerFunc {
	return s.authed(func(w http.ResponseWriter, r *http.Request) {
		if !slices.Contains(roles, principal(r).Role) {
			fail(w, r, http.StatusForbidden, "forbidden", "Your role does not allow this action.", nil)
			return
		}
		h(w, r)
	})
}

var (
	writers = []domain.Role{domain.RoleAdmin, domain.RoleOperator}
	admins  = []domain.Role{domain.RoleAdmin}
)

// limit applies a rate limit keyed by keyFn.
func (s *Server) limit(name string, n int, win time.Duration, keyFn func(*http.Request) string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ok, retry := s.limiter.Allow(r.Context(), name+":"+keyFn(r), n, win)
		if !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
			fail(w, r, http.StatusTooManyRequests, "rate_limited", "Too many requests. Try again shortly.", nil)
			return
		}
		h(w, r)
	}
}

func (s *Server) byIP(r *http.Request) string { return s.clientIP(r) }

func byUser(r *http.Request) string {
	if p := principal(r); p != nil {
		return p.UserID.String()
	}
	return "anon"
}
