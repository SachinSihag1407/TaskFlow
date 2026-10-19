package config

import (
	"strings"
	"testing"
)

func TestLoadDefaultsAndValidation(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.WorkerConcurrency != 4 || c.CookieSecure || c.LeaseDuration < 3*c.WorkerHeartbeat {
		t.Fatalf("unexpected defaults: %+v", c)
	}

	t.Setenv("TASKFLOW_ENV", "production")
	t.Setenv("WEBHOOK_ALLOW_PRIVATE_NETWORKS", "true")
	t.Setenv("LEASE_DURATION", "1s")
	t.Setenv("WORKER_CONCURRENCY", "abc")
	_, err = Load()
	if err == nil {
		t.Fatal("invalid configuration accepted")
	}
	for _, want := range []string{"WEBHOOK_ALLOW_PRIVATE_NETWORKS", "LEASE_DURATION", "WORKER_CONCURRENCY"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
}

func TestProductionDefaultsToSecureCookies(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("TASKFLOW_ENV", "production")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !c.CookieSecure {
		t.Fatal("production must default to Secure cookies")
	}
}

func TestDatabaseURLRequired(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("expected DATABASE_URL error, got %v", err)
	}
}

func TestRedisDisabledAndPort(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("REDIS_URL", "disabled")
	t.Setenv("PORT", "10000")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !c.RedisDisabled || c.RedisURL != "" || c.HTTPAddr != ":10000" {
		t.Fatalf("got disabled=%v url=%q addr=%q", c.RedisDisabled, c.RedisURL, c.HTTPAddr)
	}
	t.Setenv("SEED_DEMO", "true")
	t.Setenv("SEED_PASSWORD", "short")
	if _, err := Load(); err == nil {
		t.Fatal("SEED_DEMO with a weak password must be rejected")
	}
}
