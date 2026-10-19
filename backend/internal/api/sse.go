package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/taskflow/taskflow/internal/metrics"
	"github.com/taskflow/taskflow/internal/queue"
)

const (
	maxSSEClients   = 1000
	maxSSEPerUser   = 10
	sseHeartbeat    = 15 * time.Second
	sseClientBuffer = 64
)

// Hub fans lifecycle events from one Redis subscription out to connected
// Server-Sent Events clients. Events carry identifiers only; each client
// re-fetches whatever it displays through the normal (authorized) API, so the
// stream never carries data a client could not otherwise read. Clients only
// receive events for their own workspace (plus tenant-less platform events).
type Hub struct {
	q   *queue.Queue
	log *slog.Logger

	mu      sync.RWMutex
	clients map[*sseClient]struct{}
	live    atomic.Bool
}

type sseClient struct {
	user   string
	tenant string
	ch     chan []byte
	status chan bool
}

func NewHub(q *queue.Queue, log *slog.Logger) *Hub {
	return &Hub{q: q, log: log, clients: map[*sseClient]struct{}{}}
}

// Live reports whether the hub is currently receiving events from Redis.
func (h *Hub) Live() bool { return h.live.Load() }

// Clients returns the number of connected clients.
func (h *Hub) Clients() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// Run subscribes to Redis and dispatches events until ctx is cancelled,
// re-subscribing with backoff when Redis is unavailable.
func (h *Hub) Run(ctx context.Context) {
	if h.q == nil {
		return
	}
	backoff := time.Second
	for ctx.Err() == nil {
		ps := h.q.SubscribeEvents(ctx)
		if _, err := ps.Receive(ctx); err != nil {
			_ = ps.Close()
			h.setLive(false)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
		h.setLive(true)
		h.log.Info("sse hub subscribed to redis events")
		ch := ps.Channel()
		// go-redis reconnects pub/sub silently; an explicit ping makes outages
		// visible so clients can be told to fall back to polling.
		ping := time.NewTicker(5 * time.Second)
	recv:
		for {
			select {
			case <-ctx.Done():
				ping.Stop()
				_ = ps.Close()
				return
			case <-ping.C:
				pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
				err := h.q.Ping(pctx)
				cancel()
				if err != nil {
					h.log.Warn("redis unreachable; live updates degraded", "err", err)
					break recv
				}
			case msg, ok := <-ch:
				if !ok {
					break recv
				}
				h.dispatch(msg.Payload)
			}
		}
		ping.Stop()
		_ = ps.Close()
		h.setLive(false)
	}
}

func (h *Hub) setLive(v bool) {
	if h.live.Swap(v) == v {
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		select {
		case c.status <- v:
		default:
		}
	}
}

func (h *Hub) dispatch(payload string) {
	ev, err := queue.ParseEvent(payload)
	if err != nil {
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		if ev.TenantID != "" && ev.TenantID != c.tenant {
			continue
		}
		select {
		case c.ch <- []byte(payload):
		default:
			// Slow client: drop the event rather than block everyone. The
			// client's periodic refetch keeps its view eventually correct.
		}
	}
}

func (h *Hub) add(user, tenant string) (*sseClient, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.clients) >= maxSSEClients {
		return nil, false
	}
	perUser := 0
	for c := range h.clients {
		if c.user == user {
			perUser++
		}
	}
	if perUser >= maxSSEPerUser {
		return nil, false
	}
	c := &sseClient{user: user, tenant: tenant, ch: make(chan []byte, sseClientBuffer), status: make(chan bool, 4)}
	h.clients[c] = struct{}{}
	metrics.SSEClients.Set(float64(len(h.clients)))
	return c, true
}

func (h *Hub) remove(c *sseClient) {
	h.mu.Lock()
	delete(h.clients, c)
	metrics.SSEClients.Set(float64(len(h.clients)))
	h.mu.Unlock()
}

// stream serves GET /api/v1/events/stream.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{}) // long-lived stream
	p := principal(r)
	c, ok := s.hub.add(p.UserID.String(), p.TenantID.String())
	if !ok {
		fail(w, r, http.StatusServiceUnavailable, "too_many_streams", "Too many live connections; falling back to polling.", nil)
		return
	}
	defer s.hub.remove(c)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // disable nginx buffering
	w.WriteHeader(http.StatusOK)

	send := func(event string, data []byte) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	status := func(live bool) []byte {
		b, _ := json.Marshal(map[string]bool{"live": live})
		return b
	}
	// retry: tells EventSource how long to wait before reconnecting.
	_, _ = fmt.Fprint(w, "retry: 5000\n\n")
	if !send("status", status(s.hub.Live())) {
		return
	}
	hb := time.NewTicker(sseHeartbeat)
	defer hb.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-c.ch:
			if !send("update", msg) {
				return
			}
		case live := <-c.status:
			if !send("status", status(live)) {
				return
			}
		case <-hb.C:
			// Streams outlive the request that authenticated them; end the
			// stream once the session is revoked, expired or the user disabled.
			if !s.sessionStillValid(r) {
				return
			}
			_ = rc.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		}
	}
}

func (s *Server) sessionStillValid(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 3*time.Second)
	defer cancel()
	_, _, err = s.auth.Authenticate(ctx, c.Value, s.clientIP(r), r.UserAgent())
	return err == nil
}
