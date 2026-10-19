// Package testutil provides integration-test fixtures backed by real
// PostgreSQL and Redis. Tests are skipped unless TASKFLOW_TEST_DATABASE_URL
// (and, where needed, TASKFLOW_TEST_REDIS_URL) are set; `make test-integration`
// and CI start both with Docker.
//
// Each test gets its own freshly migrated database and its own Redis key
// prefix, so tests and packages can run in parallel without interfering.
package testutil

import (
	"context"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/taskflow/taskflow/internal/db"
	"github.com/taskflow/taskflow/internal/domain"
	"github.com/taskflow/taskflow/internal/engine"
	"github.com/taskflow/taskflow/internal/handlers"
	"github.com/taskflow/taskflow/internal/queue"
	"github.com/taskflow/taskflow/internal/service"
	"github.com/taskflow/taskflow/internal/store"
)

// Logger returns a logger that discards output unless TASKFLOW_TEST_LOG=1.
func Logger() *slog.Logger {
	if os.Getenv("TASKFLOW_TEST_LOG") == "1" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// DatabaseURL returns a URL for a new, migrated, per-test database.
func DatabaseURL(t testing.TB) string {
	t.Helper()
	base := os.Getenv("TASKFLOW_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TASKFLOW_TEST_DATABASE_URL not set; skipping integration test")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect admin db: %v", err)
	}
	// The name is generated locally (hex only), never from input.
	name := "tf_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:20]
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = admin.Exec(c, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		_ = admin.Close(c)
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse db url: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

// DB returns a pool connected to a new migrated database.
func DB(t testing.TB) *pgxpool.Pool {
	t.Helper()
	dsn := DatabaseURL(t)
	ctx := context.Background()
	pool, err := db.Connect(ctx, dsn, 30)
	if err != nil {
		t.Fatalf("connect test db: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool, Logger()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

// RedisURL returns the test Redis URL or skips.
func RedisURL(t testing.TB) string {
	t.Helper()
	u := os.Getenv("TASKFLOW_TEST_REDIS_URL")
	if u == "" {
		t.Skip("TASKFLOW_TEST_REDIS_URL not set; skipping integration test")
	}
	return u
}

// Queue returns a Redis queue with a unique key prefix, cleaned up afterwards.
func Queue(t testing.TB) *queue.Queue {
	t.Helper()
	prefix := "tftest:" + uuid.NewString()[:8]
	q, err := queue.ConnectWithPrefix(RedisURL(t), prefix)
	if err != nil {
		t.Fatalf("connect redis: %v", err)
	}
	if err := q.Ping(context.Background()); err != nil {
		t.Fatalf("ping redis: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		iter := q.Client().Scan(ctx, 0, prefix+":*", 100).Iterator()
		for iter.Next(ctx) {
			q.Client().Del(ctx, iter.Val())
		}
		_ = q.Close()
	})
	return q
}

// DeadQueue returns a queue pointing at a port where nothing listens, to
// simulate a Redis outage.
func DeadQueue(t testing.TB) *queue.Queue {
	t.Helper()
	q, err := queue.ConnectWithPrefix("redis://127.0.0.1:1/0", "tfdead")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Close() })
	return q
}

// Clock is a controllable time source for engine tests.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

func NewClock(t time.Time) *Clock { return &Clock{now: t.UTC()} }

func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// Env bundles the components most tests need.
type Env struct {
	Pool  *pgxpool.Pool
	Queue *queue.Queue // nil when built without Redis
	Eng   *engine.Engine
	Jobs  *service.Jobs
	Reg   *handlers.Registry
	Admin *domain.Principal
	Log   *slog.Logger
}

// NewEnv builds an Env. Pass a queue (or nil) to control Redis usage.
func NewEnv(t testing.TB, q *queue.Queue) *Env {
	t.Helper()
	pool := DB(t)
	log := Logger()
	var pub engine.Publisher
	if q != nil {
		pub = q
	}
	eng := engine.New(pool, pub, log)
	eng.Rand = func() float64 { return 0.5 }
	reg := handlers.Builtin(pool, true, 64<<10)
	env := &Env{Pool: pool, Queue: q, Eng: eng, Reg: reg, Log: log,
		Jobs: service.NewJobs(pool, eng, reg, log)}
	env.Admin = env.NewTenant(t, "Acme", domain.RoleAdmin)
	return env
}

// NewTenant creates a workspace with one user of the given role.
func (e *Env) NewTenant(t testing.TB, name string, role domain.Role) *domain.Principal {
	t.Helper()
	ctx := context.Background()
	tn := &domain.Tenant{ID: uuid.New(), Name: name, CreatedAt: time.Now().UTC()}
	if err := store.InsertTenant(ctx, e.Pool, tn); err != nil {
		t.Fatal(err)
	}
	return e.NewUser(t, tn.ID, role)
}

// NewUser adds a user to a workspace. The password hash is a placeholder:
// these principals are used directly, never through login.
func (e *Env) NewUser(t testing.TB, tenantID uuid.UUID, role domain.Role) *domain.Principal {
	t.Helper()
	u := &domain.User{ID: uuid.New(), TenantID: tenantID, Email: uuid.NewString()[:8] + "@example.com",
		Name: "Test User", PasswordHash: "x", Role: role, CreatedAt: time.Now().UTC()}
	if err := store.InsertUser(context.Background(), e.Pool, u); err != nil {
		t.Fatal(err)
	}
	return &domain.Principal{UserID: u.ID, TenantID: tenantID, Email: u.Email, Role: role}
}

// NewWorker registers a worker row (attempts reference workers by FK).
func (e *Env) NewWorker(t testing.TB) uuid.UUID {
	t.Helper()
	w := &domain.Worker{ID: uuid.New(), Hostname: "test-host", PID: 1, Version: "test", Concurrency: 4,
		Handlers: e.Reg.Names(), StartedAt: time.Now().UTC()}
	if err := store.RegisterWorker(context.Background(), e.Pool, w); err != nil {
		t.Fatal(err)
	}
	return w.ID
}

// UseClock makes the engine and job service read time from c.
func (e *Env) UseClock(c *Clock) {
	e.Eng.Now = c.Now
	e.Jobs.SetClock(c.Now)
}

// Ptr returns a pointer to v.
func Ptr[T any](v T) *T { return &v }

// SleepJob is a convenience input for a test.sleep job.
func SleepJob(name string, payload string) service.JobInput {
	return service.JobInput{Name: name, Handler: "test.sleep", Payload: []byte(payload), ScheduleType: "once"}
}

// MustCreate creates a job or fails the test.
func (e *Env) MustCreate(t testing.TB, in service.JobInput) *domain.Job {
	t.Helper()
	j, _, err := e.Jobs.Create(context.Background(), e.Admin, in, "")
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	return j
}

// Job reloads a job.
func (e *Env) Job(t testing.TB, id uuid.UUID) *domain.Job {
	t.Helper()
	j, err := store.GetJob(context.Background(), e.Pool, e.Admin.TenantID, id)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// Runs returns all runs of a job, newest first.
func (e *Env) Runs(t testing.TB, jobID uuid.UUID) []*domain.Run {
	t.Helper()
	runs, _, err := store.ListRuns(context.Background(), e.Pool, store.RunFilter{TenantID: e.Admin.TenantID, JobID: &jobID, Page: store.Page{PageSize: 200}})
	if err != nil {
		t.Fatal(err)
	}
	return runs
}

// Run reloads a run.
func (e *Env) Run(t testing.TB, id uuid.UUID) *domain.Run {
	t.Helper()
	r, err := store.GetRunUnscoped(context.Background(), e.Pool, id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Attempts returns a run's attempts, oldest first.
func (e *Env) Attempts(t testing.TB, runID uuid.UUID) []*domain.Attempt {
	t.Helper()
	as, _, err := store.ListAttempts(context.Background(), e.Pool, store.AttemptFilter{TenantID: e.Admin.TenantID, RunID: &runID, Sort: "started_at", Page: store.Page{PageSize: 100}})
	if err != nil {
		t.Fatal(err)
	}
	return as
}

// Eventually polls cond until it returns true or the timeout elapses.
func Eventually(t testing.TB, timeout time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s: %s", timeout, msg)
}
