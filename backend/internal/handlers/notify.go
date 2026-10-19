package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/taskflow/taskflow/internal/db"
)

// Notify simulates notification delivery. Delivery is recorded in the
// notification_deliveries ledger with a unique (run_id, channel, recipient)
// key, so when an attempt is retried — or re-run after its worker crashed
// mid-way — recipients that were already notified are skipped. This is the
// standard pattern for making an external side effect idempotent under
// at-least-once execution: dedupe on a key that is stable across attempts.
type Notify struct {
	DB db.DBTX
}

func (Notify) Name() string { return "notification.send" }

type notifyPayload struct {
	Channel    string   `json:"channel"`
	Recipients []string `json:"recipients"`
	Subject    string   `json:"subject"`
	Body       string   `json:"body,omitempty"`
}

var (
	e164Re  = regexp.MustCompile(`^\+[1-9][0-9]{7,14}$`)
	slackRe = regexp.MustCompile(`^[#@][a-z0-9][a-z0-9._-]{0,79}$`)
)

func (Notify) Info() Info {
	return Info{
		Name:        "notification.send",
		Title:       "Send notification",
		Description: "Simulates delivering a message over email, SMS or Slack to a list of recipients.",
		Idempotency: "Each delivery is recorded under a key unique per run and recipient; retries skip recipients already notified.",
		ExamplePayload: mustJSON(map[string]any{
			"channel": "email", "recipients": []string{"ops@example.com", "oncall@example.com"},
			"subject": "Nightly export finished", "body": "The nightly export completed successfully.",
		}),
		Fields: []Field{
			{Name: "channel", Type: "string", Required: true, Description: "email, sms or slack"},
			{Name: "recipients", Type: "string[]", Required: true, Description: "1-100 recipients valid for the channel"},
			{Name: "subject", Type: "string", Required: true, Description: "Subject line (max 200 characters)"},
			{Name: "body", Type: "string", Description: "Message body (max 5000 characters)"},
		},
	}
}

func (Notify) parse(payload json.RawMessage) (*notifyPayload, error) {
	var p notifyPayload
	if err := decodeStrict(payload, &p); err != nil {
		return nil, err
	}
	if p.Channel != "email" && p.Channel != "sms" && p.Channel != "slack" {
		return nil, errors.New("channel must be email, sms or slack")
	}
	if len(p.Recipients) < 1 || len(p.Recipients) > 100 {
		return nil, errors.New("recipients must contain between 1 and 100 entries")
	}
	seen := map[string]bool{}
	for i, r := range p.Recipients {
		r = strings.TrimSpace(r)
		p.Recipients[i] = r
		var ok bool
		switch p.Channel {
		case "email":
			addr, err := mail.ParseAddress(r)
			ok = err == nil && addr.Address == r && len(r) <= 254
		case "sms":
			ok = e164Re.MatchString(r)
		case "slack":
			ok = slackRe.MatchString(r)
		}
		if !ok {
			return nil, fmt.Errorf("recipient %d is not a valid %s address", i+1, p.Channel)
		}
		if seen[r] {
			return nil, fmt.Errorf("recipient %d is a duplicate", i+1)
		}
		seen[r] = true
	}
	if l := len(strings.TrimSpace(p.Subject)); l < 1 || l > 200 {
		return nil, errors.New("subject must be between 1 and 200 characters")
	}
	if len(p.Body) > 5000 {
		return nil, errors.New("body must be at most 5000 characters")
	}
	return &p, nil
}

func (n Notify) Validate(payload json.RawMessage) error {
	_, err := n.parse(payload)
	return err
}

// maskRecipient hides most of an address so logs don't collect PII.
func maskRecipient(r string) string {
	if at := strings.IndexByte(r, '@'); at > 0 {
		return r[:1] + "***" + r[at:]
	}
	if len(r) > 4 {
		return r[:2] + strings.Repeat("*", len(r)-4) + r[len(r)-2:]
	}
	return "***"
}

func (n Notify) Execute(ctx context.Context, x *Execution) (json.RawMessage, error) {
	p, err := n.parse(x.Payload)
	if err != nil {
		return nil, Permanent(err)
	}
	delivered, skipped := 0, 0
	for _, r := range p.Recipients {
		if err := sleepCtx(ctx, 15*time.Millisecond); err != nil { // simulated provider latency
			return nil, err
		}
		tag, err := n.DB.Exec(ctx, `INSERT INTO notification_deliveries (id, run_id, tenant_id, channel, recipient, subject)
			VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (run_id, channel, recipient) DO NOTHING`,
			uuid.New(), x.RunID, x.TenantID, p.Channel, r, p.Subject)
		if err != nil {
			return nil, fmt.Errorf("record delivery: %w", err)
		}
		if tag.RowsAffected() == 1 {
			delivered++
			x.Log.Info(fmt.Sprintf("delivered via %s to %s", p.Channel, maskRecipient(r)))
		} else {
			skipped++
			x.Log.Info(fmt.Sprintf("skipped %s: already delivered by an earlier attempt", maskRecipient(r)))
		}
	}
	return mustJSON(map[string]any{
		"channel": p.Channel, "delivered": delivered, "deduplicated": skipped, "recipients": len(p.Recipients),
	}), nil
}
