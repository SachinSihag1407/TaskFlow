package api_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/taskflow/taskflow/internal/api"
	"github.com/taskflow/taskflow/internal/config"
	"github.com/taskflow/taskflow/internal/metrics"
	"github.com/taskflow/taskflow/internal/service"
	. "github.com/taskflow/taskflow/internal/testutil"
)

type harness struct {
	t   *testing.T
	env *Env
	srv *httptest.Server
	api *api.Server
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	q := Queue(t)
	env := NewEnv(t, q)
	cfg := &config.Config{Env: "development", MaxRequestBytes: 1 << 20, AllowRegistration: true, RegisterPerHour: 100, PlatformAdmins: []string{"platform@example.com"},
		SessionTTL: time.Hour, AllowedOrigins: []string{"http://localhost:5173"}}
	s := api.New(api.Deps{Config: cfg, Log: env.Log, Pool: env.Pool, Queue: q, Engine: env.Eng, Jobs: env.Jobs,
		Auth: service.NewAuth(env.Pool, env.Eng, time.Hour), Ops: service.NewOps(env.Pool, env.Eng, q, 20*time.Second, time.Minute),
		Handlers: env.Reg, Registry: metrics.Registry()})
	ctx, cancel := context.WithCancel(context.Background())
	go s.Hub().Run(ctx)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(func() { cancel(); srv.Close() })
	return &harness{t: t, env: env, srv: srv, api: s}
}

// client is a browser-like session: cookie jar plus the CSRF token.
type client struct {
	h    *harness
	http *http.Client
	csrf string
}

func (h *harness) client() *client {
	jar, _ := cookiejar.New(nil)
	return &client{h: h, http: &http.Client{Jar: jar, Timeout: 10 * time.Second}}
}

type resp struct {
	Status int
	Header http.Header
	Body   map[string]any
	Raw    []byte
}

func (c *client) do(method, path string, body any, headers ...string) resp {
	c.h.t.Helper()
	var rdr io.Reader
	if body != nil {
		switch b := body.(type) {
		case string:
			rdr = strings.NewReader(b)
		default:
			raw, _ := json.Marshal(b)
			rdr = bytes.NewReader(raw)
		}
	}
	req, _ := http.NewRequest(method, c.h.srv.URL+path, rdr)
	req.Header.Set("Content-Type", "application/json")
	if c.csrf != "" {
		req.Header.Set("X-CSRF-Token", c.csrf)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		if headers[i+1] == "" {
			req.Header.Del(headers[i])
		} else {
			req.Header.Set(headers[i], headers[i+1])
		}
	}
	res, err := c.http.Do(req)
	if err != nil {
		c.h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	r := resp{Status: res.StatusCode, Header: res.Header, Raw: raw}
	_ = json.Unmarshal(raw, &r.Body)
	return r
}

func (r resp) data() map[string]any {
	d, _ := r.Body["data"].(map[string]any)
	return d
}

func (r resp) errCode() string {
	e, _ := r.Body["error"].(map[string]any)
	s, _ := e["code"].(string)
	return s
}

func (c *client) register(email string) map[string]any {
	c.h.t.Helper()
	r := c.do("POST", "/api/v1/auth/register", map[string]string{
		"name": "Test User", "email": email, "password": "correct-horse-9", "workspace": "WS " + email})
	if r.Status != http.StatusCreated {
		c.h.t.Fatalf("register: %d %s", r.Status, r.Raw)
	}
	c.csrf = r.data()["csrf_token"].(string)
	return r.data()
}

var uniq = 0

func email() string {
	uniq++
	return fmt.Sprintf("user%d-%d@example.com", uniq, time.Now().UnixNano())
}

func jobBody(name string) map[string]any {
	return map[string]any{"name": name, "handler": "test.sleep", "payload": map[string]any{"duration_ms": 10}, "schedule_type": "once"}
}

func TestAuthFlowAndCSRF(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	if r := c.do("GET", "/api/v1/auth/me", nil); r.Status != 401 {
		t.Fatalf("me without session: %d", r.Status)
	}
	e := email()
	me := c.register(e)
	if me["user"].(map[string]any)["role"] != "admin" {
		t.Fatal("registrant should be admin of the new workspace")
	}
	if strings.Contains(string(c.do("GET", "/api/v1/auth/me", nil).Raw), "password") {
		t.Fatal("password hash leaked in /me")
	}

	// State-changing request without the CSRF token is rejected.
	token := c.csrf
	c.csrf = ""
	if r := c.do("POST", "/api/v1/jobs", jobBody("x")); r.Status != 403 || r.errCode() != "csrf_failed" {
		t.Fatalf("missing CSRF: %d %s", r.Status, r.Raw)
	}
	c.csrf = "wrong"
	if r := c.do("POST", "/api/v1/jobs", jobBody("x")); r.Status != 403 {
		t.Fatalf("wrong CSRF: %d", r.Status)
	}
	c.csrf = token
	if r := c.do("POST", "/api/v1/jobs", jobBody("x")); r.Status != 201 {
		t.Fatalf("with CSRF: %d %s", r.Status, r.Raw)
	}
	// Cross-site origin is rejected even with a valid token.
	if r := c.do("POST", "/api/v1/jobs", jobBody("x"), "Origin", "https://evil.example"); r.Status != 403 || r.errCode() != "origin_rejected" {
		t.Fatalf("foreign origin: %d %s", r.Status, r.Raw)
	}

	// Logout revokes the session server-side.
	if r := c.do("POST", "/api/v1/auth/logout", nil); r.Status != 204 {
		t.Fatalf("logout: %d", r.Status)
	}
	if r := c.do("GET", "/api/v1/auth/me", nil); r.Status != 401 {
		t.Fatalf("me after logout: %d", r.Status)
	}

	// Login: wrong password is generic, right password works.
	c2 := h.client()
	if r := c2.do("POST", "/api/v1/auth/login", map[string]string{"email": e, "password": "nope-nope-nope1"}); r.Status != 401 || r.errCode() != "invalid_credentials" {
		t.Fatalf("bad login: %d %s", r.Status, r.Raw)
	}
	if r := c2.do("POST", "/api/v1/auth/login", map[string]string{"email": "nobody@example.com", "password": "nope-nope-nope1"}); r.Status != 401 || r.errCode() != "invalid_credentials" {
		t.Fatalf("unknown user login must look identical: %d %s", r.Status, r.Raw)
	}
	r := c2.do("POST", "/api/v1/auth/login", map[string]string{"email": strings.ToUpper(e), "password": "correct-horse-9"})
	if r.Status != 200 {
		t.Fatalf("good login (case-insensitive email): %d %s", r.Status, r.Raw)
	}
	cookie := r.Header.Get("Set-Cookie")
	if !strings.Contains(cookie, "HttpOnly") || !strings.Contains(cookie, "SameSite=Lax") {
		t.Fatalf("session cookie flags: %s", cookie)
	}
}

func TestRegistrationValidation(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	r := c.do("POST", "/api/v1/auth/register", map[string]string{"name": "", "email": "bad", "password": "short"})
	if r.Status != 422 {
		t.Fatalf("status %d", r.Status)
	}
	fields := r.Body["error"].(map[string]any)["fields"].(map[string]any)
	for _, f := range []string{"name", "email", "password"} {
		if _, ok := fields[f]; !ok {
			t.Errorf("missing field error for %s", f)
		}
	}
	e := email()
	c.register(e)
	if r := h.client().do("POST", "/api/v1/auth/register", map[string]string{"name": "X", "email": e, "password": "correct-horse-9"}); r.Status != 422 {
		t.Fatalf("duplicate email: %d", r.Status)
	}
}

func TestLoginRateLimit(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	var last int
	for i := 0; i < 12; i++ {
		last = c.do("POST", "/api/v1/auth/login", map[string]string{"email": "x@example.com", "password": "wrong-password-1"}).Status
	}
	if last != 429 {
		t.Fatalf("expected 429 after repeated failures, got %d", last)
	}
}

func TestRBAC(t *testing.T) {
	h := newHarness(t)
	admin := h.client()
	admin.register(email())
	viewerEmail, operatorEmail := email(), email()
	for _, u := range []struct{ email, role string }{{viewerEmail, "viewer"}, {operatorEmail, "operator"}} {
		if r := admin.do("POST", "/api/v1/users", map[string]string{"name": u.role, "email": u.email, "password": "correct-horse-9", "role": u.role}); r.Status != 201 {
			t.Fatalf("create %s: %d %s", u.role, r.Status, r.Raw)
		}
	}
	login := func(e string) *client {
		c := h.client()
		r := c.do("POST", "/api/v1/auth/login", map[string]string{"email": e, "password": "correct-horse-9"})
		if r.Status != 200 {
			t.Fatalf("login %s: %d", e, r.Status)
		}
		c.csrf = r.data()["csrf_token"].(string)
		return c
	}
	viewer, operator := login(viewerEmail), login(operatorEmail)

	if r := viewer.do("GET", "/api/v1/jobs", nil); r.Status != 200 {
		t.Fatalf("viewer list: %d", r.Status)
	}
	if r := viewer.do("POST", "/api/v1/jobs", jobBody("v")); r.Status != 403 {
		t.Fatalf("viewer create: %d", r.Status)
	}
	created := operator.do("POST", "/api/v1/jobs", jobBody("o"))
	if created.Status != 201 {
		t.Fatalf("operator create: %d", created.Status)
	}
	id := created.data()["id"].(string)
	if r := viewer.do("POST", "/api/v1/jobs/"+id+"/cancel", nil); r.Status != 403 {
		t.Fatalf("viewer cancel: %d", r.Status)
	}
	for _, path := range []string{"/api/v1/audit", "/api/v1/users"} {
		if r := operator.do("GET", path, nil); r.Status != 403 {
			t.Fatalf("operator GET %s: %d", path, r.Status)
		}
	}
	if r := operator.do("PUT", "/api/v1/settings", map[string]any{}); r.Status != 403 {
		t.Fatalf("operator settings: %d", r.Status)
	}
	if r := admin.do("GET", "/api/v1/audit", nil); r.Status != 200 {
		t.Fatalf("admin audit: %d", r.Status)
	}
}

func TestTenantIsolationOverHTTP(t *testing.T) {
	h := newHarness(t)
	a, b := h.client(), h.client()
	a.register(email())
	b.register(email())
	r := a.do("POST", "/api/v1/jobs", jobBody("secret"))
	id := r.data()["id"].(string)
	for _, req := range []struct{ method, path string }{
		{"GET", "/api/v1/jobs/" + id}, {"POST", "/api/v1/jobs/" + id + "/cancel"},
		{"PUT", "/api/v1/jobs/" + id}, {"GET", "/api/v1/jobs/" + id + "/runs"},
	} {
		var body any
		var hdr []string
		if req.method == "PUT" {
			body, hdr = jobBody("stolen"), []string{"If-Match", "1"}
		}
		if res := b.do(req.method, req.path, body, hdr...); res.Status != 404 {
			t.Errorf("%s %s from other tenant: %d", req.method, req.path, res.Status)
		}
	}
	list := b.do("GET", "/api/v1/jobs", nil)
	if list.Body["meta"].(map[string]any)["total"].(float64) != 0 {
		t.Fatal("other tenant sees jobs in list")
	}
}

func TestIdempotentJobCreation(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	c.register(email())
	first := c.do("POST", "/api/v1/jobs", jobBody("idem"), "Idempotency-Key", "key-123")
	if first.Status != 201 {
		t.Fatalf("first: %d", first.Status)
	}
	second := c.do("POST", "/api/v1/jobs", jobBody("idem"), "Idempotency-Key", "key-123")
	if second.Status != 200 || second.Header.Get("Idempotent-Replayed") != "true" || second.data()["id"] != first.data()["id"] {
		t.Fatalf("replay: %d replayed=%q same=%v", second.Status, second.Header.Get("Idempotent-Replayed"), second.data()["id"] == first.data()["id"])
	}
	if r := c.do("POST", "/api/v1/jobs", jobBody("different"), "Idempotency-Key", "key-123"); r.Status != 422 || r.errCode() != "idempotency_key_reused" {
		t.Fatalf("reused key with different body: %d %s", r.Status, r.Raw)
	}
	list := c.do("GET", "/api/v1/jobs", nil)
	if list.Body["meta"].(map[string]any)["total"].(float64) != 1 {
		t.Fatal("duplicate job created")
	}
}

func TestJobValidationAndConcurrencyControl(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	c.register(email())
	bad := map[string]any{"name": "", "handler": "nope", "schedule_type": "cron", "cron_expr": "61 * * * *",
		"timezone": "Mars/Base", "priority": 99, "payload": map[string]any{}}
	r := c.do("POST", "/api/v1/jobs", bad)
	if r.Status != 422 {
		t.Fatalf("status %d", r.Status)
	}
	fields := r.Body["error"].(map[string]any)["fields"].(map[string]any)
	for _, f := range []string{"name", "handler", "timezone", "priority", "cron_expr"} {
		if _, ok := fields[f]; !ok {
			t.Errorf("no error for %s: %v", f, fields)
		}
	}
	if r := c.do("POST", "/api/v1/jobs", map[string]any{"name": "x", "handler": "test.sleep", "schedule_type": "once",
		"payload": map[string]any{"duration_ms": -1}}); r.Status != 422 {
		t.Fatalf("handler payload validation: %d", r.Status)
	}
	if r := c.do("POST", "/api/v1/jobs", `{"name":"x","bogus":1}`); r.Status != 400 || r.errCode() != "invalid_json" {
		t.Fatalf("unknown field: %d %s", r.Status, r.Raw)
	}
	big := `{"name":"` + strings.Repeat("a", 2<<20) + `"}`
	if r := c.do("POST", "/api/v1/jobs", big); r.Status != 413 {
		t.Fatalf("oversized body: %d", r.Status)
	}

	// Optimistic concurrency on update.
	cron := map[string]any{"name": "cron", "handler": "test.sleep", "schedule_type": "cron", "cron_expr": "0 * * * *",
		"timezone": "UTC", "payload": map[string]any{"duration_ms": 1}}
	j := c.do("POST", "/api/v1/jobs", cron).data()
	id := j["id"].(string)
	if r := c.do("PUT", "/api/v1/jobs/"+id, cron); r.Status != 428 {
		t.Fatalf("missing If-Match: %d", r.Status)
	}
	cron["name"] = "renamed"
	if r := c.do("PUT", "/api/v1/jobs/"+id, cron, "If-Match", "1"); r.Status != 200 || r.data()["version"].(float64) != 2 {
		t.Fatalf("update: %d %s", r.Status, r.Raw)
	}
	if r := c.do("PUT", "/api/v1/jobs/"+id, cron, "If-Match", "1"); r.Status != 409 || r.errCode() != "version_conflict" {
		t.Fatalf("stale update: %d %s", r.Status, r.Raw)
	}

	// Disallowed actions explain themselves.
	if r := c.do("POST", "/api/v1/jobs/"+id+"/resume", nil); r.Status != 409 || !strings.Contains(string(r.Raw), "not paused") {
		t.Fatalf("resume active job: %d %s", r.Status, r.Raw)
	}
	detail := c.do("GET", "/api/v1/jobs/"+id, nil).data()
	actions := detail["actions"].(map[string]any)
	if actions["resume"].(map[string]any)["allowed"] != false || actions["pause"].(map[string]any)["allowed"] != true {
		t.Fatalf("actions: %v", actions)
	}
	if len(detail["upcoming_runs"].([]any)) != 5 {
		t.Fatal("expected 5 upcoming runs for cron job")
	}
}

func TestSchedulePreview(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	c.register(email())
	r := c.do("POST", "/api/v1/schedules/preview", map[string]any{"cron_expr": "0 9 * * 1-5", "timezone": "Europe/Berlin", "count": 3})
	if r.Status != 200 || len(r.data()["next_runs"].([]any)) != 3 {
		t.Fatalf("preview: %d %s", r.Status, r.Raw)
	}
	if r := c.do("POST", "/api/v1/schedules/preview", map[string]any{"cron_expr": "bad", "timezone": "UTC"}); r.Status != 422 {
		t.Fatalf("bad cron: %d", r.Status)
	}
}

func TestDashboardAndHealthEndpoints(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	c.register(email())
	for _, p := range []string{"/api/v1/analytics/overview?range=1h", "/api/v1/system/health", "/api/v1/workers",
		"/api/v1/executions", "/api/v1/dead-letter", "/api/v1/schedules", "/api/v1/handlers", "/api/v1/events",
		"/api/v1/notifications", "/api/v1/settings", "/api/v1/workspace"} {
		if r := c.do("GET", p, nil); r.Status != 200 {
			t.Errorf("GET %s: %d %s", p, r.Status, r.Raw)
		}
	}
	ov := c.do("GET", "/api/v1/analytics/overview?range=1h", nil).data()
	// 1h at 2-minute buckets: 30 buckets, plus one partial leading bucket
	// because the window start is aligned down to a bucket boundary.
	if n := len(ov["throughput"].([]any)); ov["total_jobs"].(float64) != 0 || n < 30 || n > 31 {
		t.Fatalf("empty overview unexpected: total=%v buckets=%d", ov["total_jobs"], len(ov["throughput"].([]any)))
	}
	if r := h.client().do("GET", "/readyz", nil); r.Status != 200 {
		t.Fatalf("readyz: %d", r.Status)
	}
	if r := h.client().do("GET", "/metrics", nil); r.Status != 200 || !strings.Contains(string(r.Raw), "taskflow_http_requests_total") {
		t.Fatalf("metrics: %d", r.Status)
	}
}

func TestSSEStream(t *testing.T) {
	h := newHarness(t)
	c := h.client()
	c.register(email())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", h.srv.URL+"/api/v1/events/stream", nil)
	res, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	sc := bufio.NewScanner(res.Body)
	seenStatus := false
	go func() {
		time.Sleep(500 * time.Millisecond)
		c.do("POST", "/api/v1/jobs", jobBody("sse"))
	}()
	for sc.Scan() {
		line := sc.Text()
		if line == "event: status" {
			seenStatus = true
		}
		if line == "event: update" {
			if !seenStatus {
				t.Fatal("update before status")
			}
			return
		}
	}
	t.Fatal("no update event received")
}

func TestOpenAPICoversEveryRoute(t *testing.T) {
	spec, err := os.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	documented := map[string]bool{}
	pathRe := regexp.MustCompile(`^  (/\S*):\s*$`)
	methodRe := regexp.MustCompile(`^    (get|post|put|patch|delete):\s*$`)
	current := ""
	for _, line := range strings.Split(string(spec), "\n") {
		if m := pathRe.FindStringSubmatch(line); m != nil {
			current = m[1]
			continue
		}
		if m := methodRe.FindStringSubmatch(line); m != nil && current != "" {
			documented[strings.ToUpper(m[1])+" "+current] = true
		}
		if strings.HasPrefix(line, "components:") {
			break
		}
	}
	cfg := &config.Config{MaxRequestBytes: 1 << 20}
	s := api.New(api.Deps{Config: cfg, Log: Logger(), Registry: metrics.Registry()})
	for _, route := range s.Routes() {
		if !documented[route] {
			t.Errorf("route %q is not documented in openapi.yaml", route)
		}
		delete(documented, route)
	}
	for extra := range documented {
		t.Errorf("openapi.yaml documents %q, which is not a registered route", extra)
	}
}

func TestWorkerDrainRequiresPlatformAdmin(t *testing.T) {
	h := newHarness(t)
	worker := h.env.NewWorker(t)

	tenantAdmin := h.client()
	me := tenantAdmin.register(email())
	if me["platform_admin"] != false {
		t.Fatal("a self-registered admin must not be a platform admin")
	}
	if r := tenantAdmin.do("POST", "/api/v1/workers/"+worker.String()+"/drain", nil); r.Status != 403 {
		t.Fatalf("workspace admin drained a shared worker: %d", r.Status)
	}

	platform := h.client()
	pme := platform.do("POST", "/api/v1/auth/register", map[string]string{"name": "Ops", "email": "platform@example.com", "password": "correct-horse-9"})
	platform.csrf = pme.data()["csrf_token"].(string)
	if pme.data()["platform_admin"] != true {
		t.Fatal("configured platform admin not flagged")
	}
	if r := platform.do("POST", "/api/v1/workers/"+worker.String()+"/drain", nil); r.Status != 202 {
		t.Fatalf("platform admin drain: %d %s", r.Status, r.Raw)
	}
	events := tenantAdmin.do("GET", "/api/v1/events", nil)
	if strings.Contains(string(events.Raw), "platform@example.com") {
		t.Fatal("drain event leaked the actor email to another workspace")
	}
}
