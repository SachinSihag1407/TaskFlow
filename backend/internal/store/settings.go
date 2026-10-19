package store

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/taskflow/taskflow/internal/db"
	"github.com/taskflow/taskflow/internal/domain"
)

// GetSettings loads a tenant's settings.
func GetSettings(ctx context.Context, q db.DBTX, tenantID uuid.UUID) (*domain.Settings, error) {
	var s domain.Settings
	err := q.QueryRow(ctx, `SELECT tenant_id, default_max_attempts, default_timeout_seconds, default_backoff_base_seconds,
		default_backoff_max_seconds, default_priority, default_timezone, max_concurrent_runs, event_retention_days, updated_at
		FROM tenant_settings WHERE tenant_id = $1`, tenantID).Scan(&s.TenantID, &s.DefaultMaxAttempts,
		&s.DefaultTimeoutSeconds, &s.DefaultBackoffBaseSeconds, &s.DefaultBackoffMaxSeconds, &s.DefaultPriority,
		&s.DefaultTimezone, &s.MaxConcurrentRuns, &s.EventRetentionDays, &s.UpdatedAt)
	if db.IsNoRows(err) {
		return nil, domain.ErrNotFound
	}
	return &s, err
}

// UpdateSettings persists a tenant's settings.
func UpdateSettings(ctx context.Context, q db.DBTX, s *domain.Settings, by uuid.UUID, now time.Time) error {
	_, err := q.Exec(ctx, `UPDATE tenant_settings SET default_max_attempts=$2, default_timeout_seconds=$3,
		default_backoff_base_seconds=$4, default_backoff_max_seconds=$5, default_priority=$6, default_timezone=$7,
		max_concurrent_runs=$8, event_retention_days=$9, updated_at=$10, updated_by=$11 WHERE tenant_id=$1`,
		s.TenantID, s.DefaultMaxAttempts, s.DefaultTimeoutSeconds, s.DefaultBackoffBaseSeconds,
		s.DefaultBackoffMaxSeconds, s.DefaultPriority, s.DefaultTimezone, s.MaxConcurrentRuns,
		s.EventRetentionDays, now, by)
	return err
}

// TenantConcurrencyLimit returns the tenant's max concurrent runs (0 = unlimited).
func TenantConcurrencyLimit(ctx context.Context, q db.DBTX, tenantID uuid.UUID) (int, error) {
	var n int
	err := q.QueryRow(ctx, `SELECT max_concurrent_runs FROM tenant_settings WHERE tenant_id = $1`, tenantID).Scan(&n)
	if db.IsNoRows(err) {
		return 0, nil
	}
	return n, err
}
