// Package queue implements TaskFlow's Redis-based dispatch layer.
//
// Redis is an accelerator, not a source of truth. PostgreSQL holds every
// run's authoritative status; Redis holds *hints* that a run is ready:
//
//   - ready  (ZSET): run ids ordered by priority, then FIFO. Workers pop from it.
//   - delayed (ZSET): run ids deferred because a concurrency limit was hit.
//   - pub/sub channels for lifecycle events (SSE fan-out) and cancellation.
//
// Because a worker must still win an atomic claim in PostgreSQL, duplicate or
// stale entries are harmless, and entries lost in a Redis restart are
// re-published by the scheduler's reconciler.
package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const (
	// priorityStride separates priority bands in the ready score. Unix
	// milliseconds (~1.8e12) fit below it, and 11 bands * 1e13 stays well
	// under 2^53 so float64 scores are exact.
	priorityStride = 1e13
)

// Queue wraps a Redis client. All keys and channels share a prefix so
// several deployments (or parallel test runs) can share one Redis.
type Queue struct {
	rdb           *redis.Client
	prefix        string
	keyReady      string
	keyDelayed    string
	channelEvents string
	channelCancel string
}

// Connect parses a redis:// URL and returns a Queue. It does not fail if Redis
// is down: TaskFlow keeps operating in degraded mode without it.
func Connect(url string) (*Queue, error) { return ConnectWithPrefix(url, "taskflow") }

// ConnectWithPrefix is Connect with a custom key prefix.
func ConnectWithPrefix(url, prefix string) (*Queue, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	opts.DialTimeout = 2 * time.Second
	opts.ReadTimeout = 3 * time.Second
	opts.WriteTimeout = 2 * time.Second
	opts.MaxRetries = 1
	return &Queue{
		rdb:           redis.NewClient(opts),
		prefix:        prefix,
		keyReady:      prefix + ":queue:ready",
		keyDelayed:    prefix + ":queue:delayed",
		channelEvents: prefix + ":events",
		channelCancel: prefix + ":cancel",
	}, nil
}

// Client exposes the underlying client (used by rate limiting).
func (q *Queue) Client() *redis.Client { return q.rdb }

// Prefix returns the key prefix shared by all of this queue's keys.
func (q *Queue) Prefix() string { return q.prefix }

func (q *Queue) Close() error { return q.rdb.Close() }

// Ping checks connectivity.
func (q *Queue) Ping(ctx context.Context) error { return q.rdb.Ping(ctx).Err() }

// Score encodes priority (1..10, higher first) and enqueue time (FIFO within a
// priority) into a single ZSET score; lower scores pop first.
func Score(priority int, queuedAt time.Time) float64 {
	if priority < 1 {
		priority = 1
	}
	if priority > 10 {
		priority = 10
	}
	return float64(10-priority)*priorityStride + float64(queuedAt.UnixMilli())
}

// Item is a run ready for dispatch.
type Item struct {
	RunID    uuid.UUID
	Priority int
	QueuedAt time.Time
}

// Publish adds runs to the ready set. ZADD NX keeps the original score if the
// run is already present, so re-publishing is idempotent.
func (q *Queue) Publish(ctx context.Context, items ...Item) error {
	if len(items) == 0 {
		return nil
	}
	members := make([]redis.Z, len(items))
	for i, it := range items {
		members[i] = redis.Z{Score: Score(it.Priority, it.QueuedAt), Member: it.RunID.String()}
	}
	return q.rdb.ZAddNX(ctx, q.keyReady, members...).Err()
}

// ErrEmpty means no run was available within the wait time.
var ErrEmpty = errors.New("queue empty")

// Pop blocks up to wait for the highest-priority ready run.
func (q *Queue) Pop(ctx context.Context, wait time.Duration) (uuid.UUID, error) {
	res, err := q.rdb.BZPopMin(ctx, wait, q.keyReady).Result()
	if errors.Is(err, redis.Nil) {
		return uuid.Nil, ErrEmpty
	}
	if err != nil {
		return uuid.Nil, err
	}
	s, _ := res.Member.(string)
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, fmt.Errorf("malformed queue member %q: %w", s, err)
	}
	return id, nil
}

// Defer parks a run in the delayed set until `delay` has passed. The member
// encodes the ready score so promotion preserves priority ordering.
func (q *Queue) Defer(ctx context.Context, it Item, delay time.Duration) error {
	member := it.RunID.String() + "|" + strconv.FormatFloat(Score(it.Priority, it.QueuedAt), 'f', 0, 64)
	readyAt := float64(time.Now().Add(delay).UnixMilli())
	return q.rdb.ZAdd(ctx, q.keyDelayed, redis.Z{Score: readyAt, Member: member}).Err()
}

// promoteScript atomically moves due delayed members into the ready set.
var promoteScript = redis.NewScript(`
local due = redis.call('ZRANGEBYSCORE', KEYS[1], '-inf', ARGV[1], 'LIMIT', 0, tonumber(ARGV[2]))
for _, m in ipairs(due) do
  local sep = string.find(m, '|', 1, true)
  if sep then
    redis.call('ZADD', KEYS[2], 'NX', string.sub(m, sep + 1), string.sub(m, 1, sep - 1))
  end
  redis.call('ZREM', KEYS[1], m)
end
return #due`)

// PromoteDelayed moves up to limit due delayed runs to the ready set.
func (q *Queue) PromoteDelayed(ctx context.Context, now time.Time, limit int) (int, error) {
	n, err := promoteScript.Run(ctx, q.rdb, []string{q.keyDelayed, q.keyReady}, now.UnixMilli(), limit).Int()
	return n, err
}

// Depth returns the number of entries in the ready and delayed sets.
func (q *Queue) Depth(ctx context.Context) (ready, delayed int64, err error) {
	pipe := q.rdb.Pipeline()
	r := pipe.ZCard(ctx, q.keyReady)
	d := pipe.ZCard(ctx, q.keyDelayed)
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, 0, err
	}
	return r.Val(), d.Val(), nil
}

// Remove deletes a run from the ready set (e.g. after cancellation).
func (q *Queue) Remove(ctx context.Context, runID uuid.UUID) error {
	return q.rdb.ZRem(ctx, q.keyReady, runID.String()).Err()
}

// Event is a lifecycle notification fanned out to SSE clients. It carries
// identifiers only; clients re-fetch authoritative state from the API.
type Event struct {
	Type     string `json:"type"` // run.updated | job.updated | worker.updated
	TenantID string `json:"tenant_id,omitempty"`
	JobID    string `json:"job_id,omitempty"`
	RunID    string `json:"run_id,omitempty"`
	WorkerID string `json:"worker_id,omitempty"`
	Status   string `json:"status,omitempty"`
	At       int64  `json:"at"`
}

// PublishEvent broadcasts a lifecycle event. Best effort.
func (q *Queue) PublishEvent(ctx context.Context, e Event) error {
	if e.At == 0 {
		e.At = time.Now().UnixMilli()
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return q.rdb.Publish(ctx, q.channelEvents, b).Err()
}

// SubscribeEvents returns a subscription to lifecycle events.
func (q *Queue) SubscribeEvents(ctx context.Context) *redis.PubSub {
	return q.rdb.Subscribe(ctx, q.channelEvents)
}

// ParseEvent decodes an event payload.
func ParseEvent(payload string) (Event, error) {
	var e Event
	err := json.Unmarshal([]byte(payload), &e)
	return e, err
}

// PublishCancel asks whichever worker runs runID to stop. Best effort: the
// lease-renewal loop also observes cancel_requested_at in PostgreSQL.
func (q *Queue) PublishCancel(ctx context.Context, runID uuid.UUID) error {
	return q.rdb.Publish(ctx, q.channelCancel, runID.String()).Err()
}

// SubscribeCancel returns a subscription to cancellation requests.
func (q *Queue) SubscribeCancel(ctx context.Context) *redis.PubSub {
	return q.rdb.Subscribe(ctx, q.channelCancel)
}
