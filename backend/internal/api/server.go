// Package api is TaskFlow's HTTP layer: routing, middleware, request
// validation and response formatting. Business logic lives in package
// service; this package only translates between HTTP and services.
package api

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/taskflow/taskflow/internal/config"
	"github.com/taskflow/taskflow/internal/engine"
	"github.com/taskflow/taskflow/internal/handlers"
	"github.com/taskflow/taskflow/internal/queue"
	"github.com/taskflow/taskflow/internal/ratelimit"
	"github.com/taskflow/taskflow/internal/service"
)

//go:embed openapi.yaml
var openapiSpec []byte

// Server holds the API's dependencies.
type Server struct {
	cfg      *config.Config
	log      *slog.Logger
	pool     *pgxpool.Pool
	q        *queue.Queue
	eng      *engine.Engine
	jobs     *service.Jobs
	auth     *service.Auth
	ops      *service.Ops
	handlers *handlers.Registry
	limiter  *ratelimit.Limiter
	hub      *Hub
	reg      *prometheus.Registry
	mux      *http.ServeMux
	routes   []string
}

// Deps are the collaborators needed to build a Server.
type Deps struct {
	Config   *config.Config
	Log      *slog.Logger
	Pool     *pgxpool.Pool
	Queue    *queue.Queue
	Engine   *engine.Engine
	Jobs     *service.Jobs
	Auth     *service.Auth
	Ops      *service.Ops
	Handlers *handlers.Registry
	Registry *prometheus.Registry
}

func New(d Deps) *Server {
	s := &Server{cfg: d.Config, log: d.Log, pool: d.Pool, q: d.Queue, eng: d.Engine, jobs: d.Jobs,
		auth: d.Auth, ops: d.Ops, handlers: d.Handlers, reg: d.Registry, mux: http.NewServeMux()}
	if d.Queue != nil {
		s.limiter = ratelimit.New(d.Queue.Client(), d.Queue.Prefix())
	} else {
		s.limiter = ratelimit.New(nil, "taskflow")
	}
	s.hub = NewHub(d.Queue, d.Log)
	s.registerRoutes()
	return s
}

// Hub exposes the SSE hub so main can run it.
func (s *Server) Hub() *Hub { return s.hub }

// Handler returns the root handler with the middleware chain applied.
func (s *Server) Handler() http.Handler {
	return s.base(s.cors(s.originCheck(s.authenticate(s.mux))))
}

// Routes lists every registered "METHOD /path" pattern (used by the OpenAPI
// contract test).
func (s *Server) Routes() []string { return s.routes }

// handle registers a route and records its pattern for logs and metrics.
func (s *Server) handle(pattern string, h http.HandlerFunc) {
	s.routes = append(s.routes, pattern)
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		info(r).route = pattern
		if r.ContentLength > s.cfg.MaxRequestBytes {
			fail(w, r, http.StatusRequestEntityTooLarge, "body_too_large", "Request body is too large.", nil)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxRequestBytes)
		h(w, r)
	})
}

func (s *Server) registerRoutes() {
	ipMinute := func(name string, n int, h http.HandlerFunc) http.HandlerFunc {
		return s.limit(name, n, time.Minute, s.byIP, h)
	}
	userMinute := func(name string, n int, h http.HandlerFunc) http.HandlerFunc {
		return s.limit(name, n, time.Minute, byUser, h)
	}

	// Probes and docs
	s.handle("GET /healthz", s.healthz)
	s.handle("GET /readyz", s.readyz)
	s.handle("GET /metrics", s.metrics)
	s.handle("GET /api/openapi.yaml", s.openapi)
	s.handle("GET /api/docs", s.docs)

	// Auth
	s.handle("GET /api/v1/auth/config", s.authConfig)
	s.handle("POST /api/v1/auth/register", s.limit("register", max(1, s.cfg.RegisterPerHour), time.Hour, s.byIP, s.register))
	s.handle("POST /api/v1/auth/login", ipMinute("login", 10, s.login))
	s.handle("POST /api/v1/auth/logout", s.authed(s.logout))
	s.handle("GET /api/v1/auth/me", s.authed(s.me))
	s.handle("POST /api/v1/auth/password", s.authed(ipMinute("password", 5, s.changePassword)))

	// Jobs
	s.handle("GET /api/v1/handlers", s.authed(s.listHandlers))
	s.handle("GET /api/v1/jobs", s.authed(s.listJobs))
	s.handle("POST /api/v1/jobs", s.role(writers, userMinute("job_create", 3000, s.createJob)))
	s.handle("POST /api/v1/jobs/bulk", s.role(writers, userMinute("job_bulk", 60, s.bulkJobs)))
	s.handle("GET /api/v1/jobs/{id}", s.authed(s.getJob))
	s.handle("PUT /api/v1/jobs/{id}", s.role(writers, s.updateJob))
	s.handle("GET /api/v1/jobs/{id}/runs", s.authed(s.listJobRuns))
	s.handle("POST /api/v1/jobs/{id}/pause", s.role(writers, s.jobAction("pause")))
	s.handle("POST /api/v1/jobs/{id}/resume", s.role(writers, s.jobAction("resume")))
	s.handle("POST /api/v1/jobs/{id}/cancel", s.role(writers, s.jobAction("cancel")))
	s.handle("POST /api/v1/jobs/{id}/retry", s.role(writers, s.retryJob))
	s.handle("POST /api/v1/jobs/{id}/run", s.role(writers, s.runNow))

	// Schedules
	s.handle("GET /api/v1/schedules", s.authed(s.listSchedules))
	s.handle("POST /api/v1/schedules/preview", s.authed(userMinute("preview", 120, s.previewSchedule)))

	// Runs, dead-letter queue, executions
	s.handle("GET /api/v1/runs/{id}", s.authed(s.getRun))
	s.handle("POST /api/v1/runs/{id}/cancel", s.role(writers, s.cancelRun))
	s.handle("POST /api/v1/runs/{id}/replay", s.role(writers, s.replayRun))
	s.handle("GET /api/v1/dead-letter", s.authed(s.listDeadLetter))
	s.handle("POST /api/v1/dead-letter/replay", s.role(writers, userMinute("dlq_replay", 30, s.bulkReplay)))
	s.handle("GET /api/v1/executions", s.authed(s.listExecutions))
	s.handle("GET /api/v1/executions/{id}", s.authed(s.getExecution))
	s.handle("GET /api/v1/executions/{id}/logs", s.authed(s.executionLogs))

	// Workers
	s.handle("GET /api/v1/workers", s.authed(s.listWorkers))
	s.handle("GET /api/v1/workers/{id}", s.authed(s.getWorker))
	s.handle("POST /api/v1/workers/{id}/drain", s.role(admins, s.drainWorker))

	// Observability
	s.handle("GET /api/v1/analytics/overview", s.authed(s.overview))
	s.handle("GET /api/v1/system/health", s.authed(s.systemHealth))
	s.handle("GET /api/v1/events", s.authed(s.listEvents))
	s.handle("GET /api/v1/events/stream", s.authed(s.stream))
	s.handle("GET /api/v1/notifications", s.authed(s.notifications))
	s.handle("POST /api/v1/notifications/seen", s.authed(s.notificationsSeen))

	// Workspace administration
	s.handle("GET /api/v1/workspace", s.authed(s.workspace))
	s.handle("GET /api/v1/settings", s.authed(s.getSettings))
	s.handle("PUT /api/v1/settings", s.role(admins, s.updateSettings))
	s.handle("GET /api/v1/audit", s.role(admins, s.listAudit))
	s.handle("GET /api/v1/users", s.role(admins, s.listUsers))
	s.handle("POST /api/v1/users", s.role(admins, userMinute("user_create", 20, s.createUser)))
	s.handle("PATCH /api/v1/users/{id}", s.role(admins, s.updateUser))

	static := s.staticHandler()
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if static == nil || isAPIPath(r.URL.Path) {
			fail(w, r, http.StatusNotFound, "not_found", "No such endpoint.", nil)
			return
		}
		static.ServeHTTP(w, r)
	})
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// readyz reports ready only when PostgreSQL is reachable. Redis is optional:
// the API keeps serving (and workers keep executing) without it.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	c, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.pool.Ping(c); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "postgres": "down"})
		return
	}
	redis := "up"
	switch {
	case s.q == nil:
		redis = "disabled"
	case s.q.Ping(c) != nil:
		redis = "down"
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready", "postgres": "up", "redis": redis})
}

// metrics exposes Prometheus metrics, optionally behind a bearer token.
func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	if s.cfg.MetricsToken != "" {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.MetricsToken)) != 1 {
			fail(w, r, http.StatusUnauthorized, "unauthenticated", "Metrics require a bearer token.", nil)
			return
		}
	}
	promhttp.HandlerFor(s.reg, promhttp.HandlerOpts{}).ServeHTTP(w, r)
}

func (s *Server) openapi(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	_, _ = w.Write(openapiSpec)
}

// docs serves Swagger UI from a pinned CDN version, with a CSP that only
// allows scripts and styles from that CDN.
func (s *Server) docs(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src https://cdn.jsdelivr.net 'unsafe-inline'; "+
		"style-src https://cdn.jsdelivr.net 'unsafe-inline'; img-src 'self' data: https://cdn.jsdelivr.net; connect-src 'self'; frame-ancestors 'none'")
	_, _ = w.Write([]byte(`<!doctype html><html><head><meta charset="utf-8"><title>TaskFlow API</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5.17.14/swagger-ui.css"></head>
<body><div id="ui"></div>
<script src="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5.17.14/swagger-ui-bundle.js"></script>
<script>SwaggerUIBundle({url: "/api/openapi.yaml", dom_id: "#ui", withCredentials: true});</script>
</body></html>`))
}
