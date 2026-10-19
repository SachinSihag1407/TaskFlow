package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"sort"
	"time"
)

// Report generates a report from deterministic sample data. The same payload
// always produces the same output (including its checksum), so retries are
// naturally idempotent: re-running an attempt cannot produce a different
// report, and the result is stored once by the fenced completion write.
type Report struct{}

func (Report) Name() string { return "report.generate" }

type reportPayload struct {
	ReportType string   `json:"report_type"`
	PeriodDays int      `json:"period_days"`
	Seed       *int64   `json:"seed,omitempty"`
	Regions    []string `json:"regions,omitempty"`
}

var reportTypes = map[string]bool{"sales_summary": true, "inventory": true, "user_activity": true}
var defaultRegions = []string{"north-america", "europe", "asia-pacific", "latin-america", "africa"}

func (Report) Info() Info {
	return Info{
		Name:        "report.generate",
		Title:       "Generate report",
		Description: "Aggregates deterministic sample data into a summary report (totals, per-region breakdown, checksum).",
		Idempotency: "Deterministic: identical payloads always yield identical output, so retries cannot diverge.",
		ExamplePayload: mustJSON(map[string]any{
			"report_type": "sales_summary", "period_days": 30, "seed": 42,
		}),
		Fields: []Field{
			{Name: "report_type", Type: "string", Required: true, Description: "sales_summary, inventory or user_activity"},
			{Name: "period_days", Type: "integer", Required: true, Description: "Days of data to aggregate (1-365)"},
			{Name: "seed", Type: "integer", Description: "Seed for the sample data generator (default 42)"},
			{Name: "regions", Type: "string[]", Description: "Subset of regions to include"},
		},
	}
}

func (Report) parse(payload json.RawMessage) (*reportPayload, error) {
	var p reportPayload
	if err := decodeStrict(payload, &p); err != nil {
		return nil, err
	}
	if !reportTypes[p.ReportType] {
		return nil, errors.New("report_type must be one of sales_summary, inventory, user_activity")
	}
	if p.PeriodDays < 1 || p.PeriodDays > 365 {
		return nil, errors.New("period_days must be between 1 and 365")
	}
	if len(p.Regions) > len(defaultRegions) {
		return nil, errors.New("too many regions")
	}
	for _, r := range p.Regions {
		found := false
		for _, d := range defaultRegions {
			if r == d {
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("unknown region %q", r)
		}
	}
	return &p, nil
}

func (r Report) Validate(payload json.RawMessage) error {
	_, err := r.parse(payload)
	return err
}

func (r Report) Execute(ctx context.Context, x *Execution) (json.RawMessage, error) {
	p, err := r.parse(x.Payload)
	if err != nil {
		return nil, Permanent(err)
	}
	seed := int64(42)
	if p.Seed != nil {
		seed = *p.Seed
	}
	regions := p.Regions
	if len(regions) == 0 {
		regions = defaultRegions
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(p.ReportType))
	rng := rand.New(rand.NewPCG(uint64(seed), h.Sum64()))

	x.Log.Info(fmt.Sprintf("generating %s report over %d day(s) for %d region(s)", p.ReportType, p.PeriodDays, len(regions)))
	totals := map[string]float64{}
	var grand float64
	rows := 0
	digest := sha256.New()
	buf := make([]byte, 8)
	for day := 0; day < p.PeriodDays; day++ {
		for _, region := range regions {
			v := float64(rng.IntN(100_000)) / 100
			totals[region] += v
			grand += v
			rows++
			binary.LittleEndian.PutUint64(buf, uint64(v*100))
			digest.Write(buf)
		}
		// Simulate I/O-bound aggregation work, honouring cancellation.
		if day%10 == 9 {
			if err := sleepCtx(ctx, 20*time.Millisecond); err != nil {
				return nil, err
			}
		}
	}
	type regionTotal struct {
		Region string  `json:"region"`
		Total  float64 `json:"total"`
		Share  float64 `json:"share"`
	}
	breakdown := make([]regionTotal, 0, len(totals))
	for region, t := range totals {
		share := 0.0
		if grand > 0 {
			share = t / grand
		}
		breakdown = append(breakdown, regionTotal{Region: region, Total: round2(t), Share: round2(share * 100)})
	}
	sort.Slice(breakdown, func(i, j int) bool { return breakdown[i].Total > breakdown[j].Total })
	x.Log.Info(fmt.Sprintf("aggregated %d rows", rows))
	return mustJSON(map[string]any{
		"report_type": p.ReportType,
		"period_days": p.PeriodDays,
		"rows":        rows,
		"grand_total": round2(grand),
		"by_region":   breakdown,
		"checksum":    hex.EncodeToString(digest.Sum(nil))[:16],
	}), nil
}

func round2(v float64) float64 { return float64(int64(v*100+0.5)) / 100 }
