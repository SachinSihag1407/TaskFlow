"""batch.process: chunked processing with checkpoints.

A retried attempt resumes from the last checkpoint instead of reprocessing
everything, and because each record's contribution is a pure function of its
index, reprocessing a partially completed chunk after a crash yields the same
accumulator — the handler is idempotent per chunk.
"""

from __future__ import annotations

import asyncio

from . import Execution, Handler, HandlerError, Payload, Permanent, decode, field

MASK = 0xFFFFFFFFFFFFFFFF


class BatchPayload(Payload):
    total_records: int = 0
    chunk_size: int = 0
    operation: str = ""
    delay_ms_per_chunk: int = 0
    # Simulates a transient failure (first attempt only) when processing
    # reaches this record, to demonstrate checkpoint resume.
    fail_at_record: int | None = None


def record_value(op: str, i: int) -> int:
    if op == "sum_squares":
        return (i * i) & MASK
    # splitmix64-style mixing for a checksum-like accumulator.
    v = (i + 0x9E3779B97F4A7C15) & MASK
    v = ((v ^ (v >> 30)) * 0xBF58476D1CE4E5B9) & MASK
    v = ((v ^ (v >> 27)) * 0x94D049BB133111EB) & MASK
    return v ^ (v >> 31)


class Batch(Handler):
    name = "batch.process"

    def info(self) -> dict:
        return {
            "name": self.name, "title": "Process batch",
            "description": "Processes records in chunks with a checkpoint after each chunk; "
                           "retries resume where the last attempt stopped.",
            "idempotency": "Checkpointed and deterministic per record: a retry resumes from the last saved chunk "
                           "and produces the same result.",
            "example_payload": {"total_records": 5000, "chunk_size": 500, "operation": "checksum",
                                "delay_ms_per_chunk": 200},
            "fields": [
                field("total_records", "integer", "Records to process (1-1,000,000)", True),
                field("chunk_size", "integer", "Records per checkpoint (1-10,000, default 500)"),
                field("operation", "string", "checksum (default) or sum_squares"),
                field("delay_ms_per_chunk", "integer", "Simulated work per chunk in ms (0-5000)"),
                field("fail_at_record", "integer", "Simulate a transient failure at this record on the first attempt"),
            ],
        }

    def _parse(self, payload) -> BatchPayload:
        p = decode(BatchPayload, payload)
        p.chunk_size = p.chunk_size or 500
        p.operation = p.operation or "checksum"
        if not 1 <= p.total_records <= 1_000_000:
            raise ValueError("total_records must be between 1 and 1,000,000")
        if not 1 <= p.chunk_size <= 10_000:
            raise ValueError("chunk_size must be between 1 and 10,000")
        if p.operation not in ("checksum", "sum_squares"):
            raise ValueError("operation must be checksum or sum_squares")
        if not 0 <= p.delay_ms_per_chunk <= 5000:
            raise ValueError("delay_ms_per_chunk must be between 0 and 5000")
        if p.fail_at_record is not None and not 1 <= p.fail_at_record <= p.total_records:
            raise ValueError("fail_at_record must be within total_records")
        return p

    def validate(self, payload) -> None:
        self._parse(payload)

    async def execute(self, x: Execution):
        try:
            p = self._parse(x.payload)
        except ValueError as e:
            raise Permanent(str(e)) from None
        processed, acc = 0, 0
        cp = x.checkpoint
        if cp is not None:
            if (isinstance(cp, dict) and isinstance(cp.get("processed"), int)
                    and isinstance(cp.get("accumulator"), int) and 0 <= cp["processed"] <= p.total_records):
                processed, acc = cp["processed"], cp["accumulator"] & MASK
            else:
                x.log.warning("ignoring unreadable checkpoint")
        resumed_from = processed
        if resumed_from > 0:
            x.log.info(f"resuming from checkpoint at record {resumed_from} of {p.total_records}")
        else:
            x.log.info(f"processing {p.total_records} records in chunks of {p.chunk_size}")
        while processed < p.total_records:
            end = min(processed + p.chunk_size, p.total_records)
            for i in range(processed + 1, end + 1):
                if p.fail_at_record is not None and i == p.fail_at_record and x.attempt == 1:
                    raise HandlerError(f"simulated transient failure at record {i}")
                acc = (acc + record_value(p.operation, i)) & MASK
            await asyncio.sleep(p.delay_ms_per_chunk / 1000)
            processed = end
            try:
                await x.save_checkpoint({"processed": processed, "accumulator": acc})
            except Exception as e:  # noqa: BLE001 - lease lost or DB error: this attempt cannot continue
                raise HandlerError(f"save checkpoint: {e}") from None
            x.log.info(f"checkpoint: {end}/{p.total_records} records")
        return {"processed": processed, "operation": p.operation, "accumulator": f"{acc:016x}",
                "resumed_from": resumed_from}
