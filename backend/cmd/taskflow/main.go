// Command taskflow is the single binary for every TaskFlow process:
//
//	taskflow api         REST API + SSE
//	taskflow scheduler   scheduling, retries, lease recovery, reconciliation
//	taskflow worker      job execution
//	taskflow all-in-one  migrate + API + scheduler + worker in one process (free-tier demo)
//	taskflow migrate     apply database migrations
//	taskflow seed        create demo users and jobs (development only)
//	taskflow loadtest    submit jobs through the API and report measurements
//	taskflow healthcheck probe an HTTP health endpoint (container healthchecks)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata" // embed the IANA database so timezones work in minimal images

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/taskflow/taskflow/internal/adminhttp"
	"github.com/taskflow/taskflow/internal/api"
	"github.com/taskflow/taskflow/internal/config"
	"github.com/taskflow/taskflow/internal/db"
	"github.com/taskflow/taskflow/internal/engine"
	"github.com/taskflow/taskflow/internal/handlers"
	"github.com/taskflow/taskflow/internal/logging"
	"github.com/taskflow/taskflow/internal/metrics"
	"github.com/taskflow/taskflow/internal/queue"
	"github.com/taskflow/taskflow/internal/scheduler"
	"github.com/taskflow/taskflow/internal/service"
	"github.com/taskflow/taskflow/internal/worker"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "api":
		err = runAPI()
	case "worker":
		err = runWorker()
	case "all-in-one":
		err = runAllInOne()
	case "scheduler":
		err = runScheduler()
	case "migrate":
		err = runMigrate()
	case "seed":
		err = runSeed(args)
	case "loadtest":
		err = runLoadtest(args)
	case "healthcheck":
		err = runHealthcheck(args)
	case "version":
		fmt.Println(version)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "taskflow %s: %v\n", cmd, err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: taskflow <api|scheduler|worker|all-in-one|migrate|seed|loadtest|healthcheck|version> [flags]")
}

// signalContext is cancelled on SIGINT/SIGTERM.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

type deps struct {
	cfg  *config.Config
	log  *slog.Logger
	pool *pgxpool.Pool
	q    *queue.Queue
	eng  *engine.Engine
}

func bootstrap(ctx context.Context, component string) (*deps, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	log := logging.New(cfg.LogLevel, component).With("version", version)
	if _, err := handlers.NewRegistry(handlers.Report{}, handlers.Batch{}, handlers.Notify{}, &handlers.Webhook{}, handlers.Sleep{}).Subset(cfg.EnabledHandlers); err != nil {
		return nil, fmt.Errorf("ENABLED_HANDLERS: %w", err)
	}
	pool, err := connectWithRetry(ctx, cfg, log)
	if err != nil {
		return nil, err
	}
	d := &deps{cfg: cfg, log: log, pool: pool}
	if cfg.RedisDisabled {
		// Pass a nil interface (not a typed nil *Queue) so the engine skips publishing.
		log.Info("redis disabled; workers poll PostgreSQL and the UI polls for updates")
		d.eng = engine.New(pool, nil, log)
		return d, nil
	}
	q, err := queue.Connect(cfg.RedisURL)
	if err != nil {
		pool.Close()
		return nil, err
	}
	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	if err := q.Ping(pingCtx); err != nil {
		// Not fatal: every component degrades gracefully without Redis.
		log.Warn("redis unavailable at startup; continuing in degraded mode", "err", err)
	}
	cancel()
	d.q, d.eng = q, engine.New(pool, q, log)
	return d, nil
}

// connectWithRetry tolerates the database starting after the process.
func connectWithRetry(ctx context.Context, cfg *config.Config, log *slog.Logger) (*pgxpool.Pool, error) {
	var lastErr error
	for i := 0; i < 30; i++ {
		pool, err := db.Connect(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
		if err == nil {
			return pool, nil
		}
		lastErr = err
		log.Warn("database not reachable yet; retrying", "attempt", i+1, "err", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return nil, fmt.Errorf("database unreachable: %w", lastErr)
}

func (d *deps) close() {
	if d.q != nil {
		_ = d.q.Close()
	}
	d.pool.Close()
}

func runAPI() error {
	ctx, stop := signalContext()
	defer stop()
	d, err := bootstrap(ctx, "api")
	if err != nil {
		return err
	}
	defer d.close()

	return serveHTTP(ctx, d, newAPIServer(ctx, d))
}

// newAPIServer wires the API and starts its SSE hub.
func newAPIServer(ctx context.Context, d *deps) *http.Server {
	reg := mustRegistry(d.cfg, d.pool)
	srv := api.New(api.Deps{
		Config: d.cfg, Log: d.log, Pool: d.pool, Queue: d.q, Engine: d.eng,
		Jobs:     service.NewJobs(d.pool, d.eng, reg, d.log),
		Auth:     service.NewAuth(d.pool, d.eng, d.cfg.SessionTTL),
		Ops:      service.NewOps(d.pool, d.eng, d.q, d.cfg.WorkerStaleAfter, d.cfg.WorkerOfflineAfter),
		Handlers: reg,
		Registry: metrics.Registry(),
	})
	go srv.Hub().Run(ctx)
	return &http.Server{
		Addr: d.cfg.HTTPAddr, Handler: srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 120 * time.Second,
		MaxHeaderBytes: 64 << 10,
	}
}

// serveHTTP runs the server until ctx is cancelled, then shuts down gracefully.
func serveHTTP(ctx context.Context, d *deps, httpSrv *http.Server) error {
	errCh := make(chan error, 1)
	go func() {
		d.log.Info("api listening", "addr", d.cfg.HTTPAddr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	d.log.Info("shutting down api")
	shutCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return httpSrv.Shutdown(shutCtx)
}

func runWorker() error {
	ctx, stop := signalContext()
	defer stop()
	d, err := bootstrap(ctx, "worker")
	if err != nil {
		return err
	}
	defer d.close()

	reg, err := mustRegistry(d.cfg, d.pool).Subset(d.cfg.WorkerHandlers)
	if err != nil {
		return err
	}
	w := worker.New(worker.Config{
		Concurrency: d.cfg.WorkerConcurrency, Lease: d.cfg.LeaseDuration, Heartbeat: d.cfg.WorkerHeartbeat,
		ShutdownGrace: d.cfg.ShutdownGrace, PollInterval: d.cfg.WorkerPollInterval, Version: version,
	}, d.eng, d.pool, d.q, reg, d.log)

	adminCtx, stopAdmin := context.WithCancel(context.Background())
	defer stopAdmin()
	go adminhttp.Serve(adminCtx, d.cfg.AdminAddr, metrics.Registry(), func(c context.Context) error { return d.pool.Ping(c) }, d.log)
	return w.Run(ctx)
}

func runScheduler() error {
	ctx, stop := signalContext()
	defer stop()
	d, err := bootstrap(ctx, "scheduler")
	if err != nil {
		return err
	}
	defer d.close()

	s := scheduler.New(scheduler.Config{
		Tick: d.cfg.SchedulerTick, BatchSize: d.cfg.SchedulerBatchSize,
		ReconcileInterval: d.cfg.ReconcileInterval, WorkerOfflineAfter: d.cfg.WorkerOfflineAfter,
	}, d.eng, d.pool, d.q, d.log)
	adminCtx, stopAdmin := context.WithCancel(context.Background())
	defer stopAdmin()
	go adminhttp.Serve(adminCtx, d.cfg.AdminAddr, metrics.Registry(), func(c context.Context) error { return d.pool.Ping(c) }, d.log)
	return s.Run(ctx)
}

func runMigrate() error {
	ctx, stop := signalContext()
	defer stop()
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := logging.New(cfg.LogLevel, "migrate")
	pool, err := connectWithRetry(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool, log); err != nil {
		return err
	}
	log.Info("migrations up to date")
	return nil
}

func runHealthcheck(args []string) error {
	url := "http://127.0.0.1:8080/healthz"
	if len(args) > 0 {
		url = args[0]
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unhealthy: HTTP %d", resp.StatusCode)
	}
	return nil
}

// mustRegistry returns the built-in handlers restricted to ENABLED_HANDLERS.
// Config validation guarantees the names are valid.
func mustRegistry(cfg *config.Config, pool *pgxpool.Pool) *handlers.Registry {
	reg, err := handlers.Builtin(pool, cfg.AllowPrivateWebhooks, cfg.WebhookMaxRespBytes).Subset(cfg.EnabledHandlers)
	if err != nil {
		panic(err)
	}
	return reg
}
