package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Batch processes a range of records in chunks and checkpoints after each
// chunk. A retried attempt resumes from the last checkpoint instead of
// reprocessing everything, and because each record's contribution is a pure
// function of its index, reprocessing a partially completed chunk after a
// crash yields the same accumulator — the handler is idempotent per chunk.
type Batch struct{}

func (Batch) Name() string { return "batch.process" }

type batchPayload struct {
	TotalRecords    int    `json:"total_records"`
	ChunkSize       int    `json:"chunk_size,omitempty"`
	Operation       string `json:"operation,omitempty"`
	DelayMSPerChunk int    `json:"delay_ms_per_chunk,omitempty"`
	// FailAtRecord simulates a transient failure (first attempt only) when
	// processing reaches this record, to demonstrate checkpoint resume.
	FailAtRecord *int `json:"fail_at_record,omitempty"`
}

type batchCheckpoint struct {
	Processed   int    `json:"processed"`
	Accumulator uint64 `json:"accumulator"`
}

func (Batch) Info() Info {
	return Info{
		Name:        "batch.process",
		Title:       "Process batch",
		Description: "Processes records in chunks with a checkpoint after each chunk; retries resume where the last attempt stopped.",
		Idempotency: "Checkpointed and deterministic per record: a retry resumes from the last saved chunk and produces the same result.",
		ExamplePayload: mustJSON(map[string]any{
			"total_records": 5000, "chunk_size": 500, "operation": "checksum", "delay_ms_per_chunk": 200,
		}),
		Fields: []Field{
			{Name: "total_records", Type: "integer", Required: true, Description: "Records to process (1-1,000,000)"},
			{Name: "chunk_size", Type: "integer", Description: "Records per checkpoint (1-10,000, default 500)"},
			{Name: "operation", Type: "string", Description: "checksum (default) or sum_squares"},
			{Name: "delay_ms_per_chunk", Type: "integer", Description: "Simulated work per chunk in ms (0-5000)"},
			{Name: "fail_at_record", Type: "integer", Description: "Simulate a transient failure at this record on the first attempt"},
		},
	}
}

func (Batch) parse(payload json.RawMessage) (*batchPayload, error) {
	var p batchPayload
	if err := decodeStrict(payload, &p); err != nil {
		return nil, err
	}
	if p.ChunkSize == 0 {
		p.ChunkSize = 500
	}
	if p.Operation == "" {
		p.Operation = "checksum"
	}
	switch {
	case p.TotalRecords < 1 || p.TotalRecords > 1_000_000:
		return nil, errors.New("total_records must be between 1 and 1,000,000")
	case p.ChunkSize < 1 || p.ChunkSize > 10_000:
		return nil, errors.New("chunk_size must be between 1 and 10,000")
	case p.Operation != "checksum" && p.Operation != "sum_squares":
		return nil, errors.New("operation must be checksum or sum_squares")
	case p.DelayMSPerChunk < 0 || p.DelayMSPerChunk > 5000:
		return nil, errors.New("delay_ms_per_chunk must be between 0 and 5000")
	case p.FailAtRecord != nil && (*p.FailAtRecord < 1 || *p.FailAtRecord > p.TotalRecords):
		return nil, errors.New("fail_at_record must be within total_records")
	}
	return &p, nil
}

func (b Batch) Validate(payload json.RawMessage) error {
	_, err := b.parse(payload)
	return err
}

func recordValue(op string, i int) uint64 {
	v := uint64(i)
	if op == "sum_squares" {
		return v * v
	}
	// splitmix64-style mixing for a checksum-like accumulator.
	v += 0x9e3779b97f4a7c15
	v = (v ^ (v >> 30)) * 0xbf58476d1ce4e5b9
	v = (v ^ (v >> 27)) * 0x94d049bb133111eb
	return v ^ (v >> 31)
}

func (b Batch) Execute(ctx context.Context, x *Execution) (json.RawMessage, error) {
	p, err := b.parse(x.Payload)
	if err != nil {
		return nil, Permanent(err)
	}
	var cp batchCheckpoint
	if len(x.Checkpoint) > 0 {
		if err := json.Unmarshal(x.Checkpoint, &cp); err != nil {
			x.Log.Warn("ignoring unreadable checkpoint")
			cp = batchCheckpoint{}
		}
	}
	resumedFrom := cp.Processed
	if resumedFrom > 0 {
		x.Log.Info(fmt.Sprintf("resuming from checkpoint at record %d of %d", resumedFrom, p.TotalRecords))
	} else {
		x.Log.Info(fmt.Sprintf("processing %d records in chunks of %d", p.TotalRecords, p.ChunkSize))
	}
	for cp.Processed < p.TotalRecords {
		end := cp.Processed + p.ChunkSize
		if end > p.TotalRecords {
			end = p.TotalRecords
		}
		acc := cp.Accumulator
		for i := cp.Processed + 1; i <= end; i++ {
			if p.FailAtRecord != nil && i == *p.FailAtRecord && x.Attempt == 1 {
				return nil, fmt.Errorf("simulated transient failure at record %d", i)
			}
			acc += recordValue(p.Operation, i)
		}
		if err := sleepCtx(ctx, time.Duration(p.DelayMSPerChunk)*time.Millisecond); err != nil {
			return nil, err
		}
		cp = batchCheckpoint{Processed: end, Accumulator: acc}
		if err := x.SaveCheckpoint(ctx, cp); err != nil {
			return nil, fmt.Errorf("save checkpoint: %w", err)
		}
		x.Log.Info(fmt.Sprintf("checkpoint: %d/%d records", end, p.TotalRecords))
	}
	return mustJSON(map[string]any{
		"processed":    cp.Processed,
		"operation":    p.Operation,
		"accumulator":  fmt.Sprintf("%016x", cp.Accumulator),
		"resumed_from": resumedFrom,
	}), nil
}
