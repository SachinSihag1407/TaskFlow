package main

import (
	"fmt"
	"sync"

	"github.com/taskflow/taskflow/internal/db"
	"github.com/taskflow/taskflow/internal/scheduler"
	"github.com/taskflow/taskflow/internal/worker"
)

// runAllInOne runs migrations, the API, the scheduler and one worker in a
// single process. It exists for hosting tiers that allow only one web
// service (e.g. Render's free plan). The components are the same code as the
// separate processes and coordinate through PostgreSQL exactly as they would
// across machines; only the deployment topology differs. Run separate
// processes (docker compose) to see multiple workers.
func runAllInOne() error {
	ctx, stop := signalContext()
	defer stop()
	d, err := bootstrap(ctx, "all-in-one")
	if err != nil {
		return err
	}
	defer d.close()

	if err := db.Migrate(ctx, d.pool, d.log); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	if d.cfg.SeedDemo {
		if _, err := seedDemo(ctx, d.cfg, d.pool, d.log, d.cfg.SeedPassword, true); err != nil {
			return fmt.Errorf("seed demo data: %w", err)
		}
	}

	reg, err := mustRegistry(d.cfg, d.pool).Subset(d.cfg.WorkerHandlers)
	if err != nil {
		return err
	}
	w := worker.New(worker.Config{
		Concurrency: d.cfg.WorkerConcurrency, Lease: d.cfg.LeaseDuration, Heartbeat: d.cfg.WorkerHeartbeat,
		ShutdownGrace: d.cfg.ShutdownGrace, PollInterval: d.cfg.WorkerPollInterval, Version: version,
	}, d.eng, d.pool, d.q, reg, d.log.With("role", "worker"))
	s := scheduler.New(scheduler.Config{
		Tick: d.cfg.SchedulerTick, BatchSize: d.cfg.SchedulerBatchSize,
		ReconcileInterval: d.cfg.ReconcileInterval, WorkerOfflineAfter: d.cfg.WorkerOfflineAfter,
	}, d.eng, d.pool, d.q, d.log.With("role", "scheduler"))

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := w.Run(ctx); err != nil {
			d.log.Error("worker stopped", "err", err)
			stop()
		}
	}()
	go func() {
		defer wg.Done()
		_ = s.Run(ctx)
	}()
	err = serveHTTP(ctx, d, newAPIServer(ctx, d))
	stop()
	wg.Wait() // let the worker drain in-flight attempts before exiting
	return err
}
