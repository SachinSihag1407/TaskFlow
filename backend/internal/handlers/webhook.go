package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/taskflow/taskflow/internal/logging"
	"github.com/taskflow/taskflow/internal/netguard"
)

// Webhook calls a user-configured HTTP endpoint through a client that blocks
// internal and metadata addresses. Every attempt of a run sends the same
// Idempotency-Key header (the run id) so receivers can deduplicate: a webhook
// receiver is an external system TaskFlow cannot roll back, so idempotency
// has to be cooperative.
type Webhook struct {
	Client       *http.Client
	AllowPrivate bool
	MaxBodyBytes int64
}

// NewWebhook builds the handler with an SSRF-safe client.
func NewWebhook(allowPrivate bool, maxBody int64) *Webhook {
	if maxBody <= 0 {
		maxBody = 64 << 10
	}
	return &Webhook{
		Client:       netguard.NewClient(netguard.Options{AllowPrivate: allowPrivate, Timeout: 2 * time.Minute}),
		AllowPrivate: allowPrivate,
		MaxBodyBytes: maxBody,
	}
}

func (*Webhook) Name() string { return "http.webhook" }

type webhookPayload struct {
	URL            string            `json:"url"`
	Method         string            `json:"method,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
	Body           json.RawMessage   `json:"body,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
	SuccessStatus  []int             `json:"success_status,omitempty"`
}

var headerNameRe = regexp.MustCompile(`^[A-Za-z0-9!#$%&'*+.^_|~-]{1,64}$`)

var forbiddenHeaders = map[string]bool{
	"host": true, "content-length": true, "transfer-encoding": true, "connection": true, "upgrade": true,
	"te": true, "trailer": true, "keep-alive": true, "proxy-authorization": true, "proxy-connection": true,
	"idempotency-key": true, "x-taskflow-run-id": true, "x-taskflow-attempt": true,
}

func (*Webhook) Info() Info {
	return Info{
		Name:        "http.webhook",
		Title:       "HTTP webhook",
		Description: "Sends an HTTP request to a public endpoint. Private, loopback and cloud-metadata addresses are blocked.",
		Idempotency: "Sends Idempotency-Key: <run id> on every attempt so the receiver can deduplicate retries.",
		ExamplePayload: mustJSON(map[string]any{
			"url": "https://example.com/hooks/taskflow", "method": "POST",
			"headers": map[string]string{"Content-Type": "application/json"},
			"body":    map[string]any{"event": "report.ready"}, "timeout_seconds": 10,
		}),
		Fields: []Field{
			{Name: "url", Type: "string", Required: true, Description: "Public http(s) URL"},
			{Name: "method", Type: "string", Description: "GET, POST (default), PUT, PATCH or DELETE"},
			{Name: "headers", Type: "object", Description: "Request headers (max 20); values of credential headers are never logged"},
			{Name: "body", Type: "any", Description: "JSON value (strings are sent verbatim)"},
			{Name: "timeout_seconds", Type: "integer", Description: "Request timeout, 1-120 (default 30)"},
			{Name: "success_status", Type: "integer[]", Description: "Status codes treated as success (default 200-299)"},
		},
	}
}

func (w *Webhook) parse(payload json.RawMessage) (*webhookPayload, error) {
	var p webhookPayload
	if err := decodeStrict(payload, &p); err != nil {
		return nil, err
	}
	if err := netguard.ValidateURL(p.URL, w.AllowPrivate); err != nil {
		return nil, fmt.Errorf("url: %v", err)
	}
	p.Method = strings.ToUpper(p.Method)
	if p.Method == "" {
		p.Method = http.MethodPost
	}
	switch p.Method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return nil, errors.New("method must be GET, POST, PUT, PATCH or DELETE")
	}
	if len(p.Headers) > 20 {
		return nil, errors.New("at most 20 headers are allowed")
	}
	for k, v := range p.Headers {
		if !headerNameRe.MatchString(k) {
			return nil, fmt.Errorf("header name %q is not valid", k)
		}
		if forbiddenHeaders[strings.ToLower(k)] {
			return nil, fmt.Errorf("header %q cannot be set", k)
		}
		if len(v) > 2048 || strings.ContainsAny(v, "\r\n") {
			return nil, fmt.Errorf("header %q has an invalid value", k)
		}
	}
	if p.TimeoutSeconds == 0 {
		p.TimeoutSeconds = 30
	}
	if p.TimeoutSeconds < 1 || p.TimeoutSeconds > 120 {
		return nil, errors.New("timeout_seconds must be between 1 and 120")
	}
	if len(p.Body) > 32<<10 {
		return nil, errors.New("body must be at most 32 KB")
	}
	for _, s := range p.SuccessStatus {
		if s < 100 || s > 599 {
			return nil, errors.New("success_status entries must be valid HTTP status codes")
		}
	}
	return &p, nil
}

func (w *Webhook) Validate(payload json.RawMessage) error {
	_, err := w.parse(payload)
	return err
}

// safeURL renders a URL for logs without query values or credentials.
func safeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "[invalid url]"
	}
	u.User = nil
	if u.RawQuery != "" {
		u.RawQuery = "…"
	}
	return u.String()
}

func (w *Webhook) Execute(ctx context.Context, x *Execution) (json.RawMessage, error) {
	p, err := w.parse(x.Payload)
	if err != nil {
		return nil, Permanent(err)
	}
	var body io.Reader
	contentType := ""
	if len(p.Body) > 0 && string(p.Body) != "null" {
		var s string
		if json.Unmarshal(p.Body, &s) == nil {
			body = strings.NewReader(s) // JSON string: send verbatim
			contentType = "text/plain; charset=utf-8"
		} else {
			body = bytes.NewReader(p.Body)
			contentType = "application/json"
		}
	}
	reqCtx, cancel := context.WithTimeout(ctx, time.Duration(p.TimeoutSeconds)*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, p.Method, p.URL, body)
	if err != nil {
		return nil, Permanent(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range p.Headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("User-Agent", "TaskFlow-Webhook/1.0")
	req.Header.Set("Idempotency-Key", x.IdempotencyKey)
	req.Header.Set("X-TaskFlow-Run-Id", x.RunID.String())
	req.Header.Set("X-TaskFlow-Attempt", strconv.Itoa(x.Attempt))

	target := safeURL(p.URL)
	x.Log.Info(fmt.Sprintf("%s %s (timeout %ds)", p.Method, target, p.TimeoutSeconds))
	start := time.Now()
	resp, err := w.Client.Do(req)
	if err != nil {
		msg := logging.Redact(err.Error())
		if errors.Is(err, netguard.ErrBlocked) {
			return nil, Permanentf("request blocked: %s", msg)
		}
		if ctx.Err() != nil {
			return nil, ctx.Err() // job timeout / cancellation, not a webhook error
		}
		return nil, fmt.Errorf("request failed: %s", msg)
	}
	defer resp.Body.Close()
	limited := io.LimitReader(resp.Body, w.MaxBodyBytes)
	respBody, _ := io.ReadAll(limited)
	elapsed := time.Since(start)
	x.Log.Info(fmt.Sprintf("response %d in %s (%d bytes read)", resp.StatusCode, elapsed.Round(time.Millisecond), len(respBody)))

	snippet := string(respBody)
	if len(snippet) > 2048 {
		snippet = snippet[:2048] + "…"
	}
	result := mustJSON(map[string]any{
		"status_code": resp.StatusCode, "duration_ms": elapsed.Milliseconds(), "response_snippet": snippet,
	})
	if isSuccess(resp.StatusCode, p.SuccessStatus) {
		return result, nil
	}
	errMsg := fmt.Errorf("endpoint returned HTTP %d", resp.StatusCode)
	switch {
	case resp.StatusCode == http.StatusRequestTimeout, resp.StatusCode == http.StatusTooEarly,
		resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode >= 500:
		return nil, errMsg // transient
	default:
		return nil, Permanent(errMsg) // other 4xx: retrying will not help
	}
}

func isSuccess(code int, allowed []int) bool {
	if len(allowed) == 0 {
		return code >= 200 && code < 300
	}
	for _, c := range allowed {
		if c == code {
			return true
		}
	}
	return false
}
