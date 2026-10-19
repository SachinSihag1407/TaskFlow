// Package config loads process configuration from environment variables.
// Secrets are only ever read from the environment; nothing sensitive has a
// default value.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Env         string // "development" or "production"
	LogLevel    string
	DatabaseURL string
	DBMaxConns  int32
	RedisURL    string
	// RedisDisabled runs without Redis (REDIS_URL=disabled): workers poll
	// PostgreSQL and live updates fall back to polling. Used for the
	// single-process free-tier demo.
	RedisDisabled bool
	// StaticDir, when set, makes the API also serve the built frontend from
	// this directory (single-origin deployments without nginx).
	StaticDir string
	// SeedDemo creates the demo workspace on startup (all-in-one mode); it
	// requires an explicit SeedPassword.
	SeedDemo     bool
	SeedPassword string

	// API
	HTTPAddr          string
	AllowedOrigins    []string
	CookieSecure      bool
	SessionTTL        time.Duration
	MetricsToken      string // optional bearer token protecting /metrics on the API
	MaxRequestBytes   int64
	AllowRegistration bool
	TrustProxyHeaders bool // honour X-Real-IP (only behind a trusted proxy)
	RegisterPerHour   int  // registration attempts allowed per IP per hour
	// PlatformAdmins may act on shared infrastructure (e.g. drain workers).
	// Workspace admins cannot: workers serve every tenant.
	PlatformAdmins []string

	// Worker
	WorkerConcurrency    int
	WorkerHeartbeat      time.Duration
	LeaseDuration        time.Duration
	ShutdownGrace        time.Duration
	AdminAddr            string // health + metrics listener for worker/scheduler
	WorkerPollInterval   time.Duration
	AllowPrivateWebhooks bool // test/dev escape hatch for the SSRF guard
	WebhookMaxRespBytes  int64
	WorkerHandlers       []string // empty = all enabled handlers (per-worker subset)
	// EnabledHandlers limits which handlers the whole deployment offers
	// (API validation, handler list and workers). Empty = all built-ins.
	EnabledHandlers []string

	// Scheduler
	SchedulerTick      time.Duration
	ReconcileInterval  time.Duration
	WorkerStaleAfter   time.Duration
	WorkerOfflineAfter time.Duration
	SchedulerBatchSize int
}

// Load reads the environment and validates the result.
func Load() (*Config, error) {
	c := &Config{
		Env:          getEnv("TASKFLOW_ENV", "development"),
		LogLevel:     getEnv("LOG_LEVEL", "info"),
		DatabaseURL:  os.Getenv("DATABASE_URL"),
		RedisURL:     getEnv("REDIS_URL", "redis://localhost:6379/0"),
		HTTPAddr:     getEnv("HTTP_ADDR", httpAddrDefault()),
		StaticDir:    os.Getenv("STATIC_DIR"),
		SeedPassword: os.Getenv("SEED_PASSWORD"),
		AdminAddr:    getEnv("ADMIN_ADDR", ":9090"),
		MetricsToken: os.Getenv("METRICS_TOKEN"),
	}
	var errs []error
	c.DBMaxConns = int32(getInt("DB_MAX_CONNS", 20, &errs))
	switch strings.ToLower(strings.TrimSpace(c.RedisURL)) {
	case "disabled", "off", "none":
		c.RedisDisabled, c.RedisURL = true, ""
	}
	c.SeedDemo = getBool("SEED_DEMO", false, &errs)
	if c.SeedDemo && len(c.SeedPassword) < 10 {
		errs = append(errs, errors.New("SEED_DEMO requires SEED_PASSWORD of at least 10 characters"))
	}
	c.AllowedOrigins = splitList(getEnv("CORS_ALLOWED_ORIGINS", "http://localhost:5173,http://localhost:3000"))
	c.CookieSecure = getBool("COOKIE_SECURE", c.Env == "production", &errs)
	c.SessionTTL = getDuration("SESSION_TTL", 7*24*time.Hour, &errs)
	c.MaxRequestBytes = int64(getInt("MAX_REQUEST_BYTES", 1<<20, &errs))
	c.AllowRegistration = getBool("ALLOW_REGISTRATION", true, &errs)
	c.TrustProxyHeaders = getBool("TRUST_PROXY_HEADERS", false, &errs)
	c.RegisterPerHour = getInt("RATE_LIMIT_REGISTER_PER_HOUR", 5, &errs)
	for _, e := range splitList(os.Getenv("PLATFORM_ADMIN_EMAILS")) {
		c.PlatformAdmins = append(c.PlatformAdmins, strings.ToLower(e))
	}

	c.WorkerConcurrency = getInt("WORKER_CONCURRENCY", 4, &errs)
	c.WorkerHeartbeat = getDuration("WORKER_HEARTBEAT_INTERVAL", 5*time.Second, &errs)
	c.LeaseDuration = getDuration("LEASE_DURATION", 30*time.Second, &errs)
	c.ShutdownGrace = getDuration("SHUTDOWN_GRACE", 30*time.Second, &errs)
	c.WorkerPollInterval = getDuration("WORKER_POLL_INTERVAL", 2*time.Second, &errs)
	c.AllowPrivateWebhooks = getBool("WEBHOOK_ALLOW_PRIVATE_NETWORKS", false, &errs)
	c.WebhookMaxRespBytes = int64(getInt("WEBHOOK_MAX_RESPONSE_BYTES", 64<<10, &errs))
	c.WorkerHandlers = splitList(os.Getenv("WORKER_HANDLERS"))
	c.EnabledHandlers = splitList(os.Getenv("ENABLED_HANDLERS"))

	c.SchedulerTick = getDuration("SCHEDULER_TICK", time.Second, &errs)
	c.ReconcileInterval = getDuration("RECONCILE_INTERVAL", 10*time.Second, &errs)
	c.WorkerStaleAfter = getDuration("WORKER_STALE_AFTER", 20*time.Second, &errs)
	c.WorkerOfflineAfter = getDuration("WORKER_OFFLINE_AFTER", 2*time.Minute, &errs)
	c.SchedulerBatchSize = getInt("SCHEDULER_BATCH_SIZE", 200, &errs)

	if c.DatabaseURL == "" {
		errs = append(errs, errors.New("DATABASE_URL is required"))
	}
	if c.WorkerConcurrency < 1 || c.WorkerConcurrency > 256 {
		errs = append(errs, errors.New("WORKER_CONCURRENCY must be between 1 and 256"))
	}
	if c.LeaseDuration < 3*c.WorkerHeartbeat {
		errs = append(errs, errors.New("LEASE_DURATION must be at least 3x WORKER_HEARTBEAT_INTERVAL"))
	}
	if c.WorkerOfflineAfter <= c.WorkerStaleAfter {
		errs = append(errs, errors.New("WORKER_OFFLINE_AFTER must exceed WORKER_STALE_AFTER"))
	}
	if c.Env == "production" && c.AllowPrivateWebhooks {
		errs = append(errs, errors.New("WEBHOOK_ALLOW_PRIVATE_NETWORKS must not be enabled in production"))
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return c, nil
}

// httpAddrDefault honours PORT, which hosting platforms such as Render set.
func httpAddrDefault() string {
	if p := os.Getenv("PORT"); p != "" {
		return ":" + p
	}
	return ":8080"
}

func getEnv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func getInt(key string, def int, errs *[]error) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s: invalid integer %q", key, v))
		return def
	}
	return n
}

func getBool(key string, def bool, errs *[]error) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s: invalid boolean %q", key, v))
		return def
	}
	return b
}

func getDuration(key string, def time.Duration, errs *[]error) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		*errs = append(*errs, fmt.Errorf("%s: invalid duration %q", key, v))
		return def
	}
	return d
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
