package worker

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/taskflow/taskflow/internal/db"
	"github.com/taskflow/taskflow/internal/domain"
	"github.com/taskflow/taskflow/internal/logging"
	"github.com/taskflow/taskflow/internal/store"
)

const maxLogMessage = 2000

// attemptLogger buffers a handler's log lines and flushes them to PostgreSQL
// periodically, so logs of long-running attempts are visible while they run.
// Lines are redacted and size-capped before storage.
type attemptLogger struct {
	db        db.DBTX
	attemptID uuid.UUID
	sys       *slog.Logger

	mu      sync.Mutex
	pending []domain.LogLine
	total   int
	dropped int
}

func newAttemptLogger(database db.DBTX, attemptID uuid.UUID, sys *slog.Logger) *attemptLogger {
	return &attemptLogger{db: database, attemptID: attemptID, sys: sys}
}

func (l *attemptLogger) add(level, msg string, args ...any) {
	if len(args) > 0 {
		var b strings.Builder
		b.WriteString(msg)
		for i := 0; i+1 < len(args); i += 2 {
			fmt.Fprintf(&b, " %v=%v", args[i], args[i+1])
		}
		msg = b.String()
	}
	msg = logging.Redact(msg)
	if len(msg) > maxLogMessage {
		msg = msg[:maxLogMessage] + "…"
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.total >= store.MaxLogLinesPerAttempt-1 {
		l.dropped++
		return
	}
	l.total++
	l.pending = append(l.pending, domain.LogLine{TS: time.Now().UTC(), Level: level, Message: msg})
}

func (l *attemptLogger) Info(msg string, args ...any)  { l.add("info", msg, args...) }
func (l *attemptLogger) Warn(msg string, args ...any)  { l.add("warn", msg, args...) }
func (l *attemptLogger) Error(msg string, args ...any) { l.add("error", msg, args...) }

// flush writes buffered lines. On failure the lines are kept for the next try.
func (l *attemptLogger) flush(ctx context.Context) {
	l.mu.Lock()
	batch := l.pending
	l.pending = nil
	l.mu.Unlock()
	if len(batch) == 0 {
		return
	}
	if err := store.InsertLogs(ctx, l.db, l.attemptID, batch); err != nil {
		l.sys.Warn("flush attempt logs failed", "attempt_id", l.attemptID, "err", err)
		l.mu.Lock()
		l.pending = append(batch, l.pending...)
		l.mu.Unlock()
	}
}

// close records the truncation notice (if any) and performs a final flush.
func (l *attemptLogger) close(ctx context.Context) {
	l.mu.Lock()
	if l.dropped > 0 {
		l.pending = append(l.pending, domain.LogLine{TS: time.Now().UTC(), Level: "warn",
			Message: fmt.Sprintf("%d further log line(s) dropped (limit %d per attempt)", l.dropped, store.MaxLogLinesPerAttempt)})
	}
	l.mu.Unlock()
	l.flush(ctx)
}
