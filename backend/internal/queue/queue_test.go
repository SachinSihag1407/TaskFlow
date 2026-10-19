package queue_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/taskflow/taskflow/internal/queue"
	"github.com/taskflow/taskflow/internal/testutil"
)

func TestScoreOrdersByPriorityThenFIFO(t *testing.T) {
	now := time.Now()
	if !(queue.Score(10, now) < queue.Score(5, now.Add(-time.Hour))) {
		t.Fatal("higher priority must pop first even if enqueued later")
	}
	if !(queue.Score(5, now) < queue.Score(5, now.Add(time.Millisecond))) {
		t.Fatal("same priority must be FIFO")
	}
	if queue.Score(0, now) != queue.Score(1, now) || queue.Score(99, now) != queue.Score(10, now) {
		t.Fatal("priority must be clamped to 1..10")
	}
}

func TestPublishPopDeferPromote(t *testing.T) {
	q := testutil.Queue(t)
	ctx := context.Background()
	now := time.Now()
	low, high, mid := uuid.New(), uuid.New(), uuid.New()
	if err := q.Publish(ctx,
		queue.Item{RunID: low, Priority: 1, QueuedAt: now},
		queue.Item{RunID: high, Priority: 9, QueuedAt: now.Add(time.Second)},
		queue.Item{RunID: mid, Priority: 5, QueuedAt: now}); err != nil {
		t.Fatal(err)
	}
	// Re-publishing is idempotent (ZADD NX).
	_ = q.Publish(ctx, queue.Item{RunID: high, Priority: 9, QueuedAt: now})
	if ready, _, _ := q.Depth(ctx); ready != 3 {
		t.Fatalf("depth %d, want 3", ready)
	}
	for _, want := range []uuid.UUID{high, mid, low} {
		got, err := q.Pop(ctx, time.Second)
		if err != nil || got != want {
			t.Fatalf("pop = %s, %v; want %s", got, err, want)
		}
	}
	if _, err := q.Pop(ctx, 100*time.Millisecond); !errors.Is(err, queue.ErrEmpty) {
		t.Fatalf("empty pop: %v", err)
	}

	id := uuid.New()
	if err := q.Defer(ctx, queue.Item{RunID: id, Priority: 5, QueuedAt: now}, 200*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if n, _ := q.PromoteDelayed(ctx, time.Now(), 10); n != 0 {
		t.Fatal("promoted before delay elapsed")
	}
	time.Sleep(250 * time.Millisecond)
	if n, err := q.PromoteDelayed(ctx, time.Now(), 10); n != 1 || err != nil {
		t.Fatalf("promote: %d %v", n, err)
	}
	if got, _ := q.Pop(ctx, time.Second); got != id {
		t.Fatal("deferred run not in ready set after promotion")
	}
}

func TestEventsPubSub(t *testing.T) {
	q := testutil.Queue(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ps := q.SubscribeEvents(ctx)
	defer ps.Close()
	if _, err := ps.Receive(ctx); err != nil {
		t.Fatal(err)
	}
	if err := q.PublishEvent(ctx, queue.Event{Type: "run.updated", RunID: "r1", Status: "RUNNING"}); err != nil {
		t.Fatal(err)
	}
	msg, err := ps.ReceiveMessage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ev, err := queue.ParseEvent(msg.Payload)
	if err != nil || ev.RunID != "r1" || ev.At == 0 {
		t.Fatalf("event %+v err %v", ev, err)
	}
}
