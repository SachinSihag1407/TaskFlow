package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

type captureLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *captureLogger) add(m string)             { l.mu.Lock(); l.lines = append(l.lines, m); l.mu.Unlock() }
func (l *captureLogger) Info(m string, _ ...any)  { l.add(m) }
func (l *captureLogger) Warn(m string, _ ...any)  { l.add(m) }
func (l *captureLogger) Error(m string, _ ...any) { l.add(m) }

func exec(payload string, attempt int) (*Execution, *captureLogger) {
	l := &captureLogger{}
	id := uuid.New()
	return &Execution{RunID: id, AttemptID: uuid.New(), Attempt: attempt, MaxAttempts: 3,
		Payload: json.RawMessage(payload), IdempotencyKey: id.String(), Log: l,
		SaveCheckpoint: func(context.Context, any) error { return nil }}, l
}

func TestReportIsDeterministic(t *testing.T) {
	r := Report{}
	p := `{"report_type":"sales_summary","period_days":30,"seed":7}`
	x1, _ := exec(p, 1)
	x2, _ := exec(p, 2)
	a, err := r.Execute(context.Background(), x1)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := r.Execute(context.Background(), x2)
	if string(a) != string(b) {
		t.Fatal("identical payloads produced different reports")
	}
	for _, bad := range []string{`{}`, `{"report_type":"x","period_days":1}`, `{"report_type":"inventory","period_days":0}`,
		`{"report_type":"inventory","period_days":3,"unknown":1}`, `{"report_type":"inventory","period_days":3,"regions":["mars"]}`} {
		if r.Validate(json.RawMessage(bad)) == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestSleepFailureModes(t *testing.T) {
	s := Sleep{}
	x, _ := exec(`{"duration_ms":0,"fail_attempts":1}`, 1)
	if _, err := s.Execute(context.Background(), x); err == nil || IsPermanent(err) {
		t.Fatalf("attempt 1 should fail retryably, got %v", err)
	}
	x, _ = exec(`{"duration_ms":0,"fail_attempts":1}`, 2)
	if _, err := s.Execute(context.Background(), x); err != nil {
		t.Fatalf("attempt 2 should succeed, got %v", err)
	}
	x, _ = exec(`{"duration_ms":0,"fail_attempts":5,"permanent":true}`, 1)
	if _, err := s.Execute(context.Background(), x); !IsPermanent(err) {
		t.Fatalf("expected permanent error, got %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	x, _ = exec(`{"duration_ms":10000}`, 1)
	start := time.Now()
	if _, err := s.Execute(ctx, x); err == nil || time.Since(start) > time.Second {
		t.Fatalf("sleep must honour context cancellation (err=%v, took %s)", err, time.Since(start))
	}
}

func TestNotifyValidation(t *testing.T) {
	n := Notify{}
	bad := []string{
		`{"channel":"fax","recipients":["a@b.co"],"subject":"x"}`,
		`{"channel":"email","recipients":[],"subject":"x"}`,
		`{"channel":"email","recipients":["not-an-email"],"subject":"x"}`,
		`{"channel":"email","recipients":["a@b.co","a@b.co"],"subject":"x"}`,
		`{"channel":"sms","recipients":["12345"],"subject":"x"}`,
		`{"channel":"slack","recipients":["general"],"subject":"x"}`,
		`{"channel":"email","recipients":["a@b.co"],"subject":""}`,
	}
	for _, p := range bad {
		if n.Validate(json.RawMessage(p)) == nil {
			t.Errorf("accepted %s", p)
		}
	}
	if err := n.Validate(json.RawMessage(`{"channel":"sms","recipients":["+14155550123"],"subject":"x"}`)); err != nil {
		t.Errorf("rejected valid sms payload: %v", err)
	}
	if m := maskRecipient("alice@example.com"); strings.Contains(m, "alice") {
		t.Errorf("recipient not masked: %s", m)
	}
}

func TestWebhookSendsIdempotencyKeyAndClassifiesStatus(t *testing.T) {
	var gotKey, gotAttempt, gotAuth string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey, gotAttempt, gotAuth = r.Header.Get("Idempotency-Key"), r.Header.Get("X-TaskFlow-Attempt"), r.Header.Get("Authorization")
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"received":true}`))
	}))
	defer srv.Close()

	wh := NewWebhook(true, 1024)
	payload := `{"url":"` + srv.URL + `/hook?token=secret123","headers":{"Authorization":"Bearer s3cr3t"},"body":{"a":1}}`
	x, logs := exec(payload, 2)
	if _, err := wh.Execute(context.Background(), x); err != nil {
		t.Fatal(err)
	}
	if gotKey != x.RunID.String() || gotAttempt != "2" || gotAuth != "Bearer s3cr3t" {
		t.Fatalf("headers not sent correctly: key=%q attempt=%q", gotKey, gotAttempt)
	}
	for _, l := range logs.lines {
		if strings.Contains(l, "secret123") || strings.Contains(l, "s3cr3t") {
			t.Fatalf("secret leaked into attempt logs: %q", l)
		}
	}

	cases := map[int]bool{500: false, 503: false, 429: false, 408: false, 400: true, 404: true, 422: true}
	for code, permanent := range cases {
		status = code
		x, _ := exec(payload, 1)
		_, err := wh.Execute(context.Background(), x)
		if err == nil || IsPermanent(err) != permanent {
			t.Errorf("HTTP %d: err=%v permanent=%v, want permanent=%v", code, err, IsPermanent(err), permanent)
		}
	}
}

func TestWebhookBlocksPrivateDestinations(t *testing.T) {
	wh := NewWebhook(false, 1024)
	for _, u := range []string{"http://169.254.169.254/latest/meta-data/", "http://localhost:8080/", "http://10.0.0.1/"} {
		if wh.Validate(json.RawMessage(`{"url":"`+u+`"}`)) == nil {
			t.Errorf("payload with %s accepted", u)
		}
	}
	// Header injection and reserved headers are rejected.
	for _, p := range []string{
		`{"url":"https://example.com","headers":{"Host":"evil"}}`,
		`{"url":"https://example.com","headers":{"X-A":"b\r\nX-Injected: 1"}}`,
		`{"url":"https://example.com","headers":{"Idempotency-Key":"mine"}}`,
		`{"url":"https://example.com","method":"CONNECT"}`,
	} {
		if wh.Validate(json.RawMessage(p)) == nil {
			t.Errorf("accepted %s", p)
		}
	}
}

func TestRegistrySubset(t *testing.T) {
	r := NewRegistry(Report{}, Sleep{})
	sub, err := r.Subset([]string{"test.sleep"})
	if err != nil || len(sub.Names()) != 1 {
		t.Fatalf("subset: %v %v", sub, err)
	}
	if _, err := r.Subset([]string{"nope"}); err == nil {
		t.Fatal("unknown handler accepted")
	}
}
