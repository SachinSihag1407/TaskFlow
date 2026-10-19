// Package ratelimit implements fixed-window rate limiting backed by Redis,
// with an in-process fallback so that a Redis outage does not disable
// protection of sensitive endpoints (it only makes limits per-instance).
package ratelimit

import (
	"context"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Limiter checks request rates.
type Limiter struct {
	rdb    *redis.Client
	prefix string

	mu    sync.Mutex
	local map[string]*window
	last  time.Time
}

type window struct {
	count int
	reset time.Time
}

func New(rdb *redis.Client, prefix string) *Limiter {
	return &Limiter{rdb: rdb, prefix: prefix, local: map[string]*window{}}
}

var incrScript = redis.NewScript(`
local n = redis.call('INCR', KEYS[1])
if n == 1 then redis.call('PEXPIRE', KEYS[1], ARGV[1]) end
return {n, redis.call('PTTL', KEYS[1])}`)

// Allow records one hit for key and reports whether it is within limit per
// window, plus how long until the window resets.
func (l *Limiter) Allow(ctx context.Context, key string, limit int, win time.Duration) (bool, time.Duration) {
	if l.rdb != nil {
		c, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
		res, err := incrScript.Run(c, l.rdb, []string{l.prefix + ":rl:" + key}, win.Milliseconds()).Int64Slice()
		cancel()
		if err == nil && len(res) == 2 {
			ttl := time.Duration(res[1]) * time.Millisecond
			if ttl < 0 {
				ttl = win
			}
			return res[0] <= int64(limit), ttl
		}
	}
	return l.allowLocal(key, limit, win)
}

func (l *Limiter) allowLocal(key string, limit int, win time.Duration) (bool, time.Duration) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.last) > time.Minute { // periodic cleanup of expired windows
		for k, w := range l.local {
			if now.After(w.reset) {
				delete(l.local, k)
			}
		}
		l.last = now
	}
	w, ok := l.local[key]
	if !ok || now.After(w.reset) {
		w = &window{reset: now.Add(win)}
		l.local[key] = w
	}
	w.count++
	return w.count <= limit, w.reset.Sub(now)
}
