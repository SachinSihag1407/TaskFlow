package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/taskflow/taskflow/internal/domain"
	"github.com/taskflow/taskflow/internal/store"
	. "github.com/taskflow/taskflow/internal/testutil"
)

func TestRetentionDeletesOnlyOldEventsAndKeepsAudit(t *testing.T) {
	env := NewEnv(t, nil)
	ctx := context.Background()
	now := time.Now().UTC()
	tid := env.Admin.TenantID
	for _, age := range []time.Duration{40 * 24 * time.Hour, time.Hour} {
		e := &domain.Event{TenantID: &tid, Type: "test", Severity: domain.SeverityInfo, Message: "m", CreatedAt: now.Add(-age)}
		if err := store.InsertEvent(ctx, env.Pool, e); err != nil {
			t.Fatal(err)
		}
	}
	if err := env.Eng.Audit(ctx, env.Pool, env.Admin, "job.create", "job", "x", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Pool.Exec(ctx, `UPDATE audit_events SET created_at = $1`, now.AddDate(-1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	n, err := store.DeleteOldEvents(ctx, env.Pool, now)
	if err != nil {
		t.Fatalf("retention query failed: %v", err)
	}
	if n != 1 {
		t.Fatalf("deleted %d events, want 1 (default retention 30 days)", n)
	}
	audit, total, _ := store.ListAudit(ctx, env.Pool, store.AuditFilter{TenantID: tid})
	if total != 1 || len(audit) != 1 {
		t.Fatal("audit events must never be deleted by retention")
	}
}

func TestListJobsFilteringSortingAndSearch(t *testing.T) {
	env := NewEnv(t, nil)
	ctx := context.Background()
	for _, name := range []string{"alpha report", "beta_export", "gamma 100%"} {
		env.MustCreate(t, SleepJob(name, `{"duration_ms":1}`))
	}
	items, total, err := store.ListJobs(ctx, env.Pool, store.JobFilter{TenantID: env.Admin.TenantID, Sort: "name"})
	if err != nil || total != 3 || items[0].Name != "alpha report" {
		t.Fatalf("sort by name: total=%d err=%v first=%q", total, err, items[0].Name)
	}
	// LIKE metacharacters in the query are matched literally.
	for q, want := range map[string]int{"report": 1, "_": 1, "%": 1, "ALPHA": 1, "zzz": 0} {
		_, n, err := store.ListJobs(ctx, env.Pool, store.JobFilter{TenantID: env.Admin.TenantID, Query: q})
		if err != nil || n != want {
			t.Errorf("query %q: got %d (err %v), want %d", q, n, err, want)
		}
	}
	_, n, _ := store.ListJobs(ctx, env.Pool, store.JobFilter{TenantID: env.Admin.TenantID, Statuses: []string{"SCHEDULED"}, Page: store.Page{Page: 1, PageSize: 2}})
	if n != 3 {
		t.Fatalf("status filter total %d", n)
	}
}
