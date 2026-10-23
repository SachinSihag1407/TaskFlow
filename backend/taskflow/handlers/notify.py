"""notification.send: simulated delivery with a dedupe ledger.

Delivery is recorded in notification_deliveries with a unique (run_id,
channel, recipient) key, so when an attempt is retried — or re-run after its
worker crashed mid-way — recipients already notified are skipped. This is the
standard pattern for making an external side effect idempotent under
at-least-once execution: dedupe on a key that is stable across attempts.
"""

from __future__ import annotations

import asyncio
import re
import uuid

from . import Execution, Handler, Payload, Permanent, decode, field

E164 = re.compile(r"^\+[1-9][0-9]{7,14}$")
SLACK = re.compile(r"^[#@][a-z0-9][a-z0-9._-]{0,79}$")
# Deliberately conservative: a single address, no display name or comments.
EMAIL = re.compile(r"^[A-Za-z0-9.!#$%&'*+/=?^_`{|}~-]+@[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?"
                   r"(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?)+$")


class NotifyPayload(Payload):
    channel: str = ""
    recipients: list[str] = []
    subject: str = ""
    body: str = ""


def mask_recipient(r: str) -> str:
    """Hides most of an address so logs don't collect PII."""
    at = r.find("@")
    if at > 0:
        return r[:1] + "***" + r[at:]
    if len(r) > 4:
        return r[:2] + "*" * (len(r) - 4) + r[-2:]
    return "***"


class Notify(Handler):
    name = "notification.send"

    def __init__(self, pool):
        self.pool = pool

    def info(self) -> dict:
        return {
            "name": self.name, "title": "Send notification",
            "description": "Simulates delivering a message over email, SMS or Slack to a list of recipients.",
            "idempotency": "Each delivery is recorded under a key unique per run and recipient; "
                           "retries skip recipients already notified.",
            "example_payload": {"channel": "email", "recipients": ["ops@example.com", "oncall@example.com"],
                                "subject": "Nightly export finished",
                                "body": "The nightly export completed successfully."},
            "fields": [
                field("channel", "string", "email, sms or slack", True),
                field("recipients", "string[]", "1-100 recipients valid for the channel", True),
                field("subject", "string", "Subject line (max 200 characters)", True),
                field("body", "string", "Message body (max 5000 characters)"),
            ],
        }

    def _parse(self, payload) -> NotifyPayload:
        p = decode(NotifyPayload, payload)
        if p.channel not in ("email", "sms", "slack"):
            raise ValueError("channel must be email, sms or slack")
        if not 1 <= len(p.recipients) <= 100:
            raise ValueError("recipients must contain between 1 and 100 entries")
        seen: set[str] = set()
        cleaned = []
        for i, r in enumerate(p.recipients, 1):
            r = r.strip()
            if p.channel == "email":
                ok = len(r) <= 254 and EMAIL.match(r) is not None
            elif p.channel == "sms":
                ok = E164.match(r) is not None
            else:
                ok = SLACK.match(r) is not None
            if not ok:
                raise ValueError(f"recipient {i} is not a valid {p.channel} address")
            if r in seen:
                raise ValueError(f"recipient {i} is a duplicate")
            seen.add(r)
            cleaned.append(r)
        p.recipients = cleaned
        if not 1 <= len(p.subject.strip()) <= 200:
            raise ValueError("subject must be between 1 and 200 characters")
        if len(p.body) > 5000:
            raise ValueError("body must be at most 5000 characters")
        return p

    def validate(self, payload) -> None:
        self._parse(payload)

    async def execute(self, x: Execution):
        try:
            p = self._parse(x.payload)
        except ValueError as e:
            raise Permanent(str(e)) from None
        delivered = skipped = 0
        for r in p.recipients:
            await asyncio.sleep(0.015)  # simulated provider latency
            async with self.pool.connection() as conn:
                cur = await conn.execute(
                    """INSERT INTO notification_deliveries (id, run_id, tenant_id, channel, recipient, subject)
                    VALUES (%s, %s, %s, %s, %s, %s) ON CONFLICT (run_id, channel, recipient) DO NOTHING""",
                    (uuid.uuid4(), x.run_id, x.tenant_id, p.channel, r, p.subject))
            if cur.rowcount == 1:
                delivered += 1
                x.log.info(f"delivered via {p.channel} to {mask_recipient(r)}")
            else:
                skipped += 1
                x.log.info(f"skipped {mask_recipient(r)}: already delivered by an earlier attempt")
        return {"channel": p.channel, "delivered": delivered, "deduplicated": skipped, "recipients": len(p.recipients)}
