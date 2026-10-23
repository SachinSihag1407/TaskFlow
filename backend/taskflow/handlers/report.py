"""report.generate: deterministic sample-data report.

The same payload always produces the same output (including its checksum), so
retries are naturally idempotent: re-running an attempt cannot produce a
different report, and the result is stored once by the fenced completion write.
"""

from __future__ import annotations

import asyncio
import hashlib
import random
import struct

from . import Execution, Handler, Payload, Permanent, decode, field, fnv1a64

REPORT_TYPES = ("sales_summary", "inventory", "user_activity")
DEFAULT_REGIONS = ["north-america", "europe", "asia-pacific", "latin-america", "africa"]


class ReportPayload(Payload):
    report_type: str = ""
    period_days: int = 0
    seed: int | None = None
    regions: list[str] | None = None


def round2(v: float) -> float:
    return int(v * 100 + 0.5) / 100


class Report(Handler):
    name = "report.generate"

    def info(self) -> dict:
        return {
            "name": self.name, "title": "Generate report",
            "description": "Aggregates deterministic sample data into a summary report "
                           "(totals, per-region breakdown, checksum).",
            "idempotency": "Deterministic: identical payloads always yield identical output, so retries cannot diverge.",
            "example_payload": {"report_type": "sales_summary", "period_days": 30, "seed": 42},
            "fields": [
                field("report_type", "string", "sales_summary, inventory or user_activity", True),
                field("period_days", "integer", "Days of data to aggregate (1-365)", True),
                field("seed", "integer", "Seed for the sample data generator (default 42)"),
                field("regions", "string[]", "Subset of regions to include"),
            ],
        }

    def _parse(self, payload) -> ReportPayload:
        p = decode(ReportPayload, payload)
        if p.report_type not in REPORT_TYPES:
            raise ValueError("report_type must be one of sales_summary, inventory, user_activity")
        if not 1 <= p.period_days <= 365:
            raise ValueError("period_days must be between 1 and 365")
        if p.regions and len(p.regions) > len(DEFAULT_REGIONS):
            raise ValueError("too many regions")
        for r in p.regions or []:
            if r not in DEFAULT_REGIONS:
                raise ValueError(f'unknown region "{r}"')
        return p

    def validate(self, payload) -> None:
        self._parse(payload)

    async def execute(self, x: Execution):
        try:
            p = self._parse(x.payload)
        except ValueError as e:
            raise Permanent(str(e)) from None
        seed = 42 if p.seed is None else p.seed
        regions = p.regions or DEFAULT_REGIONS
        rng = random.Random(f"{seed}:{fnv1a64(p.report_type.encode())}")

        x.log.info(f"generating {p.report_type} report over {p.period_days} day(s) for {len(regions)} region(s)")
        totals: dict[str, float] = {}
        grand = 0.0
        rows = 0
        digest = hashlib.sha256()
        for day in range(p.period_days):
            for region in regions:
                v = rng.randrange(100_000) / 100
                totals[region] = totals.get(region, 0.0) + v
                grand += v
                rows += 1
                digest.update(struct.pack("<Q", int(v * 100)))
            if day % 10 == 9:
                await asyncio.sleep(0.02)  # simulated I/O-bound aggregation; a cancellation point
        breakdown = sorted(
            ({"region": r, "total": round2(t), "share": round2((t / grand if grand > 0 else 0) * 100)}
             for r, t in totals.items()),
            key=lambda b: b["total"], reverse=True)
        x.log.info(f"aggregated {rows} rows")
        return {"report_type": p.report_type, "period_days": p.period_days, "rows": rows,
                "grand_total": round2(grand), "by_region": breakdown, "checksum": digest.hexdigest()[:16]}
