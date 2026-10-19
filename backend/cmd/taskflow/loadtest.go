package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// runLoadtest submits jobs through the public API and measures what actually
// happened, using the execution records TaskFlow stores. It reports:
//
//   - submission throughput (API job creations per second);
//   - end-to-end throughput (completed runs per second, first submit to last finish);
//   - queue wait (attempt start minus run queued time) percentiles;
//   - execution latency (attempt duration) percentiles;
//   - failure and retry counts;
//   - worker utilisation (busy worker-seconds / available worker-seconds).
//
// No numbers are estimated: everything is computed from API responses.
func runLoadtest(args []string) error {
	fs := flag.NewFlagSet("loadtest", flag.ExitOnError)
	base := fs.String("url", "http://localhost:8080", "API base URL")
	email := fs.String("email", os.Getenv("LOADTEST_EMAIL"), "login email (or LOADTEST_EMAIL)")
	password := fs.String("password", os.Getenv("LOADTEST_PASSWORD"), "login password (or LOADTEST_PASSWORD)")
	jobs := fs.Int("jobs", 500, "number of jobs to submit")
	concurrency := fs.Int("concurrency", 20, "concurrent submitters")
	durationMS := fs.Int("duration-ms", 100, "simulated work per job (test.sleep)")
	failRate := fs.Float64("fail-rate", 0, "probability [0,1] that an attempt fails (deterministic per run)")
	maxAttempts := fs.Int("max-attempts", 3, "max attempts per job")
	timeout := fs.Duration("timeout", 10*time.Minute, "give up waiting after this long")
	jsonOut := fs.Bool("json", false, "print the report as JSON")
	_ = fs.Parse(args)
	if *email == "" || *password == "" {
		return errors.New("provide -email and -password (or LOADTEST_EMAIL / LOADTEST_PASSWORD)")
	}
	if *jobs < 1 || *concurrency < 1 {
		return errors.New("-jobs and -concurrency must be positive")
	}

	c, err := newAPIClient(*base)
	if err != nil {
		return err
	}
	ctx, stop := signalContext()
	defer stop()
	if err := c.login(ctx, *email, *password); err != nil {
		return fmt.Errorf("login: %w", err)
	}
	capacity, err := c.workerCapacity(ctx)
	if err != nil {
		return err
	}
	if capacity == 0 {
		return errors.New("no online workers; start workers before running a load test")
	}

	tag := fmt.Sprintf("loadtest-%d", time.Now().Unix())
	payload := fmt.Sprintf(`{"duration_ms":%d,"fail_probability":%g}`, *durationMS, *failRate)
	fmt.Fprintf(os.Stderr, "submitting %d jobs (%s) with %d submitters; worker capacity %d\n", *jobs, tag, *concurrency, capacity)

	var submitted, submitErrors atomic.Int64
	work := make(chan int)
	var wg sync.WaitGroup
	submitStart := time.Now()
	for i := 0; i < *concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := range work {
				body := map[string]any{
					"name": fmt.Sprintf("%s-%05d", tag, n), "handler": "test.sleep",
					"payload": json.RawMessage(payload), "schedule_type": "once",
					"max_attempts": *maxAttempts, "backoff_base_seconds": 1, "backoff_max_seconds": 5,
				}
				if err := c.post(ctx, "/api/v1/jobs", body, fmt.Sprintf("%s-%d", tag, n), nil); err != nil {
					submitErrors.Add(1)
					fmt.Fprintf(os.Stderr, "submit error: %v\n", err)
					continue
				}
				submitted.Add(1)
			}
		}()
	}
	for i := 0; i < *jobs; i++ {
		select {
		case work <- i:
		case <-ctx.Done():
		}
	}
	close(work)
	wg.Wait()
	submitDur := time.Since(submitStart)
	fmt.Fprintf(os.Stderr, "submitted %d jobs in %s (%d errors); waiting for completion…\n",
		submitted.Load(), submitDur.Round(time.Millisecond), submitErrors.Load())

	deadline := time.Now().Add(*timeout)
	for {
		pending, err := c.countJobs(ctx, tag, "SCHEDULED,QUEUED,RUNNING,RETRY_WAIT,CREATED")
		if err != nil {
			return err
		}
		if pending == 0 {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out with %d jobs still pending", pending)
		}
		fmt.Fprintf(os.Stderr, "  %d pending\n", pending)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}

	attempts, err := c.attempts(ctx, tag, submitStart.Add(-time.Minute))
	if err != nil {
		return err
	}
	failedJobs, err := c.countJobs(ctx, tag, "FAILED")
	if err != nil {
		return err
	}
	rep := buildReport(attempts, int(submitted.Load()), failedJobs, submitDur, submitStart, capacity)
	rep.Tag, rep.SubmitErrors = tag, int(submitErrors.Load())
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	rep.print()
	return nil
}

type attemptRec struct {
	Status     string     `json:"status"`
	QueuedAt   *time.Time `json:"queued_at"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
	DurationMS *int64     `json:"duration_ms"`
	Attempt    int        `json:"attempt_number"`
}

type report struct {
	Tag                string  `json:"tag"`
	JobsSubmitted      int     `json:"jobs_submitted"`
	SubmitErrors       int     `json:"submit_errors"`
	SubmitSeconds      float64 `json:"submit_seconds"`
	SubmitPerSecond    float64 `json:"submit_per_second"`
	Attempts           int     `json:"attempts"`
	Retries            int     `json:"retries"`
	FailedJobs         int     `json:"failed_jobs"`
	FailureRate        float64 `json:"job_failure_rate"`
	WallSeconds        float64 `json:"wall_seconds"`
	CompletedPerSecond float64 `json:"completed_per_second"`
	QueueWaitP50MS     float64 `json:"queue_wait_p50_ms"`
	QueueWaitP95MS     float64 `json:"queue_wait_p95_ms"`
	QueueWaitP99MS     float64 `json:"queue_wait_p99_ms"`
	ExecP50MS          float64 `json:"exec_p50_ms"`
	ExecP95MS          float64 `json:"exec_p95_ms"`
	ExecP99MS          float64 `json:"exec_p99_ms"`
	WorkerCapacity     int     `json:"worker_capacity"`
	WorkerUtilization  float64 `json:"worker_utilization"`
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return math.NaN()
	}
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	return sorted[idx]
}

func buildReport(as []attemptRec, submitted, failedJobs int, submitDur time.Duration, start time.Time, capacity int) *report {
	r := &report{JobsSubmitted: submitted, SubmitSeconds: submitDur.Seconds(), FailedJobs: failedJobs,
		WorkerCapacity: capacity, Attempts: len(as)}
	if submitDur > 0 {
		r.SubmitPerSecond = float64(submitted) / submitDur.Seconds()
	}
	if submitted > 0 {
		r.FailureRate = float64(failedJobs) / float64(submitted)
	}
	var waits, execs []float64
	var busy float64
	last := start
	for _, a := range as {
		if a.Attempt > 1 {
			r.Retries++
		}
		if a.QueuedAt != nil {
			waits = append(waits, float64(a.StartedAt.Sub(*a.QueuedAt).Milliseconds()))
		}
		if a.DurationMS != nil {
			execs = append(execs, float64(*a.DurationMS))
			busy += float64(*a.DurationMS) / 1000
		}
		if a.FinishedAt != nil && a.FinishedAt.After(last) {
			last = *a.FinishedAt
		}
	}
	sort.Float64s(waits)
	sort.Float64s(execs)
	r.WallSeconds = last.Sub(start).Seconds()
	if r.WallSeconds > 0 {
		r.CompletedPerSecond = float64(submitted) / r.WallSeconds
		r.WorkerUtilization = busy / (r.WallSeconds * float64(capacity))
	}
	r.QueueWaitP50MS, r.QueueWaitP95MS, r.QueueWaitP99MS = percentile(waits, .5), percentile(waits, .95), percentile(waits, .99)
	r.ExecP50MS, r.ExecP95MS, r.ExecP99MS = percentile(execs, .5), percentile(execs, .95), percentile(execs, .99)
	return r
}

func (r *report) print() {
	fmt.Printf(`
TaskFlow load test  %s
------------------------------------------------------------
Jobs submitted         %d  (%d submit errors)
Submission rate        %.1f jobs/s over %.1fs
Attempts / retries     %d / %d
Failed jobs            %d  (%.2f%%)
End-to-end throughput  %.1f jobs/s  (wall %.1fs)
Queue wait  p50/95/99  %.0f / %.0f / %.0f ms
Execution   p50/95/99  %.0f / %.0f / %.0f ms
Worker utilisation     %.1f%% of %d slots
`, r.Tag, r.JobsSubmitted, r.SubmitErrors, r.SubmitPerSecond, r.SubmitSeconds, r.Attempts, r.Retries,
		r.FailedJobs, r.FailureRate*100, r.CompletedPerSecond, r.WallSeconds,
		r.QueueWaitP50MS, r.QueueWaitP95MS, r.QueueWaitP99MS, r.ExecP50MS, r.ExecP95MS, r.ExecP99MS,
		r.WorkerUtilization*100, r.WorkerCapacity)
}

// apiClient is a minimal session-aware client for the TaskFlow API.
type apiClient struct {
	base string
	http *http.Client
	csrf string
}

func newAPIClient(base string) (*apiClient, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	return &apiClient{base: strings.TrimRight(base, "/"), http: &http.Client{Jar: jar, Timeout: 30 * time.Second}}, nil
}

func (c *apiClient) do(ctx context.Context, method, path string, body any, idemKey string, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.csrf != "" {
		req.Header.Set("X-CSRF-Token", c.csrf)
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (c *apiClient) post(ctx context.Context, path string, body any, idemKey string, out any) error {
	return c.do(ctx, http.MethodPost, path, body, idemKey, out)
}

func (c *apiClient) login(ctx context.Context, email, password string) error {
	var res struct {
		Data struct {
			CSRFToken string `json:"csrf_token"`
		} `json:"data"`
	}
	if err := c.post(ctx, "/api/v1/auth/login", map[string]string{"email": email, "password": password}, "", &res); err != nil {
		return err
	}
	c.csrf = res.Data.CSRFToken
	return nil
}

func (c *apiClient) workerCapacity(ctx context.Context) (int, error) {
	var res struct {
		Data []struct {
			Status      string `json:"status"`
			Concurrency int    `json:"concurrency"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/workers", nil, "", &res); err != nil {
		return 0, err
	}
	n := 0
	for _, w := range res.Data {
		if w.Status == "ONLINE" {
			n += w.Concurrency
		}
	}
	return n, nil
}

func (c *apiClient) countJobs(ctx context.Context, tag, statuses string) (int, error) {
	var res struct {
		Meta struct {
			Total int `json:"total"`
		} `json:"meta"`
	}
	q := url.Values{"q": {tag}, "status": {statuses}, "page_size": {"1"}}
	err := c.do(ctx, http.MethodGet, "/api/v1/jobs?"+q.Encode(), nil, "", &res)
	return res.Meta.Total, err
}

func (c *apiClient) attempts(ctx context.Context, tag string, since time.Time) ([]attemptRec, error) {
	var all []attemptRec
	for page := 1; ; page++ {
		var res struct {
			Data []attemptRec `json:"data"`
			Meta struct {
				TotalPages int `json:"total_pages"`
			} `json:"meta"`
		}
		q := url.Values{"q": {tag}, "since": {since.UTC().Format(time.RFC3339)}, "page_size": {"200"}, "page": {fmt.Sprint(page)}}
		if err := c.do(ctx, http.MethodGet, "/api/v1/executions?"+q.Encode(), nil, "", &res); err != nil {
			return nil, err
		}
		all = append(all, res.Data...)
		if page >= res.Meta.TotalPages {
			return all, nil
		}
	}
}
