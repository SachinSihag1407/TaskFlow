package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/taskflow/taskflow/internal/db"
	"github.com/taskflow/taskflow/internal/domain"
)

// InsertEvent appends to the operational event stream.
func InsertEvent(ctx context.Context, q db.DBTX, e *domain.Event) error {
	var data any
	if len(e.Data) > 0 {
		data = e.Data
	}
	return q.QueryRow(ctx, `
		INSERT INTO system_events (tenant_id, job_id, run_id, worker_id, type, severity, message, data, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id`,
		e.TenantID, e.JobID, e.RunID, e.WorkerID, e.Type, e.Severity, e.Message, data, e.CreatedAt).Scan(&e.ID)
}

// EventFilter drives event listings.
type EventFilter struct {
	TenantID   uuid.UUID
	JobID      *uuid.UUID
	RunID      *uuid.UUID
	WorkerID   *uuid.UUID
	Severities []string
	After      *time.Time
	// IncludePlatform includes tenant-less events (worker lifecycle).
	IncludePlatform bool
	Limit           int
}

// ListEvents returns events newest first.
func ListEvents(ctx context.Context, q db.DBTX, f EventFilter) ([]*domain.Event, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 50
	}
	tenantCond := "e.tenant_id = $1"
	if f.IncludePlatform {
		tenantCond = "(e.tenant_id = $1 OR e.tenant_id IS NULL)"
	}
	where := []string{tenantCond}
	args := []any{f.TenantID}
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, strings.ReplaceAll(cond, "?", fmt.Sprintf("$%d", len(args))))
	}
	if f.JobID != nil {
		add("e.job_id = ?", *f.JobID)
	}
	if f.RunID != nil {
		add("e.run_id = ?", *f.RunID)
	}
	if f.WorkerID != nil {
		add("e.worker_id = ?", *f.WorkerID)
	}
	if len(f.Severities) > 0 {
		add("e.severity = ANY(?)", f.Severities)
	}
	if f.After != nil {
		add("e.created_at > ?", *f.After)
	}
	args = append(args, f.Limit)
	rows, err := q.Query(ctx, fmt.Sprintf(`SELECT e.id, e.tenant_id, e.job_id, e.run_id, e.worker_id, e.type,
		e.severity, e.message, e.data, e.created_at FROM system_events e WHERE %s
		ORDER BY e.created_at DESC, e.id DESC LIMIT $%d`, strings.Join(where, " AND "), len(args)), args...)
	if err != nil {
		return nil, err
	}
	return collect(rows, func(row pgx.Row) (*domain.Event, error) {
		var e domain.Event
		var data []byte
		err := row.Scan(&e.ID, &e.TenantID, &e.JobID, &e.RunID, &e.WorkerID, &e.Type, &e.Severity, &e.Message, &data, &e.CreatedAt)
		if len(data) > 0 {
			e.Data = json.RawMessage(data)
		}
		return &e, err
	})
}

// CountEventsSince counts events (used for the notification badge).
func CountEventsSince(ctx context.Context, q db.DBTX, tenantID uuid.UUID, severities []string, since time.Time) (int, error) {
	var n int
	err := q.QueryRow(ctx, `SELECT count(*) FROM system_events WHERE tenant_id = $1 AND severity = ANY($2) AND created_at > $3`,
		tenantID, severities, since).Scan(&n)
	return n, err
}

// DeleteOldEvents applies per-tenant retention to the operational stream.
// Audit events are intentionally never deleted by this job.
func DeleteOldEvents(ctx context.Context, q db.DBTX, now time.Time) (int64, error) {
	tag, err := q.Exec(ctx, `
		DELETE FROM system_events e USING tenant_settings s
		WHERE e.tenant_id = s.tenant_id AND e.created_at < $1::timestamptz - make_interval(days => s.event_retention_days)`, now)
	if err != nil {
		return 0, err
	}
	n := tag.RowsAffected()
	tag, err = q.Exec(ctx, `DELETE FROM system_events WHERE tenant_id IS NULL AND created_at < $1`, now.AddDate(0, 0, -30))
	return n + tag.RowsAffected(), err
}

// InsertAudit records a security-relevant human action.
func InsertAudit(ctx context.Context, q db.DBTX, a *domain.AuditEvent) error {
	var meta any
	if len(a.Metadata) > 0 {
		meta = a.Metadata
	}
	return q.QueryRow(ctx, `
		INSERT INTO audit_events (tenant_id, actor_id, actor_email, action, target_type, target_id, ip, user_agent, metadata, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id`,
		a.TenantID, a.ActorID, a.ActorEmail, a.Action, a.TargetType, a.TargetID, a.IP, a.UserAgent, meta, a.CreatedAt).Scan(&a.ID)
}

// AuditFilter drives the audit log.
type AuditFilter struct {
	TenantID   uuid.UUID
	Action     string
	TargetType string
	TargetID   string
	Page       Page
}

// ListAudit returns audit events newest first.
func ListAudit(ctx context.Context, q db.DBTX, f AuditFilter) ([]*domain.AuditEvent, int, error) {
	f.Page = f.Page.Normalize()
	where := []string{"tenant_id = $1"}
	args := []any{f.TenantID}
	if f.Action != "" {
		args = append(args, f.Action+"%")
		where = append(where, fmt.Sprintf("action LIKE $%d", len(args)))
	}
	if f.TargetType != "" {
		args = append(args, f.TargetType)
		where = append(where, fmt.Sprintf("target_type = $%d", len(args)))
	}
	if f.TargetID != "" {
		args = append(args, f.TargetID)
		where = append(where, fmt.Sprintf("target_id = $%d", len(args)))
	}
	whereSQL := strings.Join(where, " AND ")
	var total int
	if err := q.QueryRow(ctx, "SELECT count(*) FROM audit_events WHERE "+whereSQL, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, f.Page.PageSize, f.Page.Offset())
	rows, err := q.Query(ctx, fmt.Sprintf(`SELECT id, tenant_id, actor_id, actor_email, action, target_type, target_id,
		ip, user_agent, metadata, created_at FROM audit_events WHERE %s ORDER BY created_at DESC, id DESC
		LIMIT $%d OFFSET $%d`, whereSQL, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	items, err := collect(rows, func(row pgx.Row) (*domain.AuditEvent, error) {
		var a domain.AuditEvent
		var meta []byte
		err := row.Scan(&a.ID, &a.TenantID, &a.ActorID, &a.ActorEmail, &a.Action, &a.TargetType, &a.TargetID,
			&a.IP, &a.UserAgent, &meta, &a.CreatedAt)
		if len(meta) > 0 {
			a.Metadata = json.RawMessage(meta)
		}
		return &a, err
	})
	return items, total, err
}
