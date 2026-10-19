package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/taskflow/taskflow/internal/config"
	"github.com/taskflow/taskflow/internal/domain"
	"github.com/taskflow/taskflow/internal/engine"
	"github.com/taskflow/taskflow/internal/logging"
	"github.com/taskflow/taskflow/internal/service"
	"github.com/taskflow/taskflow/internal/store"
)

// runSeed creates a demo workspace with three users and a set of demo jobs.
// It is idempotent: re-running it does not duplicate users or jobs. The
// password comes from SEED_PASSWORD; if unset, a random one is generated and
// printed once. Nothing in the repository contains a usable credential.
func runSeed(args []string) error {
	fs := flag.NewFlagSet("seed", flag.ExitOnError)
	withJobs := fs.Bool("jobs", true, "create demo jobs")
	_ = fs.Parse(args)

	ctx, stop := signalContext()
	defer stop()
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.Env == "production" {
		return errors.New("refusing to seed demo data when TASKFLOW_ENV=production")
	}
	log := logging.New(cfg.LogLevel, "seed")
	pool, err := connectWithRetry(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer pool.Close()

	password := os.Getenv("SEED_PASSWORD")
	generated := false
	if password == "" {
		b := make([]byte, 12)
		_, _ = rand.Read(b)
		password = "Tf-" + base64.RawURLEncoding.EncodeToString(b)
		generated = true
	}

	created, err := seedDemo(ctx, cfg, pool, log, password, *withJobs)
	if err != nil {
		return err
	}
	if created {
		fmt.Println("Created demo users: admin@example.com, operator@example.com, viewer@example.com")
		if generated {
			fmt.Printf("Generated password (shown once): %s\n", password)
		} else {
			fmt.Println("Password: value of SEED_PASSWORD")
		}
	} else {
		fmt.Println("Demo users already exist; skipping user creation.")
	}
	return nil
}

// seedDemo creates the demo workspace, its three users and the demo jobs.
// It is idempotent and reports whether the users were newly created.
func seedDemo(ctx context.Context, cfg *config.Config, pool *pgxpool.Pool, log *slog.Logger, password string, withJobs bool) (bool, error) {
	eng := engine.New(pool, nil, log) // the scheduler publishes demo runs when they become due
	auth := service.NewAuth(pool, eng, cfg.SessionTTL)

	adminEmail := "admin@example.com"
	created := false
	admin, err := store.GetUserByEmail(ctx, pool, adminEmail)
	if errors.Is(err, domain.ErrNotFound) {
		admin, err = auth.SeedUser(ctx, "Acme Engineering", "Ada Admin", adminEmail, password, domain.RoleAdmin, nil)
		if err != nil {
			return false, err
		}
		if _, err := auth.SeedUser(ctx, "", "Omar Operator", "operator@example.com", password, domain.RoleOperator, &admin.TenantID); err != nil {
			return false, err
		}
		if _, err := auth.SeedUser(ctx, "", "Vera Viewer", "viewer@example.com", password, domain.RoleViewer, &admin.TenantID); err != nil {
			return false, err
		}
		created = true
	} else if err != nil {
		return false, err
	}

	if !withJobs {
		return created, nil
	}
	p := &domain.Principal{UserID: admin.ID, TenantID: admin.TenantID, Email: admin.Email, Role: domain.RoleAdmin}
	reg := mustRegistry(cfg, pool)
	jobs := service.NewJobs(pool, eng, reg, log)
	n := 0
	for _, d := range demoJobs() {
		if !jobs.HandlerSupported(d.in.Handler) {
			continue // handler disabled in this deployment (ENABLED_HANDLERS)
		}
		_, replayed, err := jobs.Create(ctx, p, d.in, "seed:"+d.key)
		if err != nil {
			return created, fmt.Errorf("seed job %q: %w", d.in.Name, err)
		}
		if !replayed {
			n++
		}
	}
	log.Info("demo data ready", "users_created", created, "jobs_created", n)
	return created, nil
}

type demoJob struct {
	key string
	in  service.JobInput
}

func ip(v int) *int       { return &v }
func sp(v string) *string { return &v }

func demoJobs() []demoJob {
	return []demoJob{
		{"nightly-sales", service.JobInput{Name: "Nightly sales report", Description: "Aggregates the previous day's sales by region.",
			Handler: "report.generate", Payload: []byte(`{"report_type":"sales_summary","period_days":1,"seed":2026}`),
			ScheduleType: "cron", CronExpr: sp("0 2 * * *"), Timezone: "Europe/Berlin", MissedRunPolicy: "run_once", Priority: ip(6)}},
		{"inventory-snapshot", service.JobInput{Name: "Hourly inventory snapshot", Description: "Inventory levels across all regions.",
			Handler: "report.generate", Payload: []byte(`{"report_type":"inventory","period_days":7}`),
			ScheduleType: "cron", CronExpr: sp("0 * * * *"), Timezone: "UTC", MissedRunPolicy: "skip"}},
		{"heartbeat", service.JobInput{Name: "Pipeline heartbeat", Description: "Lightweight job every minute to keep the dashboard live.",
			Handler: "test.sleep", Payload: []byte(`{"duration_ms":800}`),
			ScheduleType: "cron", CronExpr: sp("* * * * *"), Timezone: "UTC", MissedRunPolicy: "skip", Priority: ip(3)}},
		{"partner-sync", service.JobInput{Name: "Flaky partner sync", Description: "Fails its first two attempts to demonstrate retries with backoff.",
			Handler: "test.sleep", Payload: []byte(`{"duration_ms":1500,"fail_attempts":2}`),
			ScheduleType: "cron", CronExpr: sp("*/10 * * * *"), Timezone: "UTC", MaxAttempts: ip(4),
			BackoffBaseSeconds: ip(5), BackoffMaxSeconds: ip(60)}},
		{"customer-import", service.JobInput{Name: "Process customer import", Description: "20k records in checkpointed chunks.",
			Handler: "batch.process", Payload: []byte(`{"total_records":20000,"chunk_size":1000,"delay_ms_per_chunk":300,"fail_at_record":12500}`),
			ScheduleType: "once", MaxAttempts: ip(3), BackoffBaseSeconds: ip(5), BackoffMaxSeconds: ip(30), Priority: ip(7)}},
		{"oncall-notify", service.JobInput{Name: "Notify on-call about release", Handler: "notification.send",
			Payload:      []byte(`{"channel":"email","recipients":["oncall@example.com","release@example.com"],"subject":"Release 2.4 deployed","body":"All services healthy."}`),
			ScheduleType: "delayed", DelaySeconds: ip(30)}},
		{"broken-export", service.JobInput{Name: "Legacy export (broken)", Description: "Always fails permanently; lands in the dead-letter queue.",
			Handler: "test.sleep", Payload: []byte(`{"duration_ms":500,"fail_attempts":25,"permanent":true}`),
			ScheduleType: "once", MaxAttempts: ip(3)}},
		{"reindex", service.JobInput{Name: "Search reindex", Description: "Long-running job with progress logs.",
			Handler: "test.sleep", Payload: []byte(`{"duration_ms":45000}`),
			ScheduleType: "delayed", DelaySeconds: ip(10), TimeoutSeconds: ip(120)}},
		{"status-webhook", service.JobInput{Name: "Status page ping", Description: "Calls a public endpoint every 15 minutes.",
			Handler: "http.webhook", Payload: []byte(`{"url":"https://example.com/","method":"GET","timeout_seconds":10}`),
			ScheduleType: "cron", CronExpr: sp("*/15 * * * *"), Timezone: "UTC", MaxAttempts: ip(3)}},
	}
}
