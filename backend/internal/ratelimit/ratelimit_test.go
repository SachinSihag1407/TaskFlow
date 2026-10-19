package ratelimit

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestLocalFallbackWhenRedisDown(t *testing.T) {
	// Nothing listens on port 1: every Redis call fails and the limiter must
	// still enforce limits in-process rather than failing open.
	l := New(redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 50 * time.Millisecond}), "t")
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow(ctx, "k", 3, time.Minute); !ok {
			t.Fatalf("request %d rejected under the limit", i+1)
		}
	}
	ok, retry := l.Allow(ctx, "k", 3, time.Minute)
	if ok || retry <= 0 {
		t.Fatalf("4th request allowed (ok=%v retry=%s)", ok, retry)
	}
	if ok, _ := l.Allow(ctx, "other", 3, time.Minute); !ok {
		t.Fatal("limits must be per key")
	}
}

func TestWindowResets(t *testing.T) {
	l := New(nil, "t")
	ctx := context.Background()
	l.Allow(ctx, "k", 1, 50*time.Millisecond)
	if ok, _ := l.Allow(ctx, "k", 1, 50*time.Millisecond); ok {
		t.Fatal("second request in window allowed")
	}
	time.Sleep(60 * time.Millisecond)
	if ok, _ := l.Allow(ctx, "k", 1, 50*time.Millisecond); !ok {
		t.Fatal("window did not reset")
	}
}
