"""Cron parsing and evaluation with timezone and daylight-saving support.

Supports the standard 5-field syntax (minute hour day-of-month month
day-of-week) with ``*``, lists, ranges, steps and month/day names, plus
descriptors (@hourly, @daily, @weekly, @monthly, @yearly).

Daylight saving follows Vixie cron / cronie:

* Fixed-hour schedules (``30 2 * * *``) use wall-clock time. A time that
  does not exist because clocks jumped forward runs at the equivalent instant
  after the jump (02:30 -> 03:30). A time that occurs twice because clocks
  fell back runs once, on its first occurrence. A daily job runs once a day.
* Schedules that fire every hour (``0 * * * *``, ``@hourly``) follow real
  elapsed time: they run in both passes of a repeated hour.

All returned datetimes are timezone-aware UTC.
"""

from __future__ import annotations

import functools
from dataclasses import dataclass
from datetime import UTC, date, datetime, timedelta
from zoneinfo import ZoneInfo, ZoneInfoNotFoundError

# Occurrences later than this are "missed" and handled by the missed-run policy.
MISFIRE_GRACE = timedelta(minutes=1)
# run_all never enqueues more than this many missed occurrences at once.
MAX_CATCH_UP = 50
# Bound for very long outages.
_MAX_SCAN = 10_000
# Expressions that never fire (e.g. Feb 30) are detected within this window.
_SEARCH_DAYS = 366 * 5

_DESCRIPTORS = {
    "@yearly": "0 0 1 1 *",
    "@annually": "0 0 1 1 *",
    "@monthly": "0 0 1 * *",
    "@weekly": "0 0 * * 0",
    "@daily": "0 0 * * *",
    "@midnight": "0 0 * * *",
    "@hourly": "0 * * * *",
}
_MONTHS = {m: i + 1 for i, m in enumerate("JAN FEB MAR APR MAY JUN JUL AUG SEP OCT NOV DEC".split())}
_DAYS = {d: i for i, d in enumerate("SUN MON TUE WED THU FRI SAT".split())}


class CronError(ValueError):
    pass


def load_timezone(tz: str) -> ZoneInfo:
    """Validates an explicit IANA timezone name."""
    if not tz:
        raise CronError("timezone is required")
    if tz.lower() == "local":
        raise CronError("timezone must be an IANA name such as UTC or Europe/Berlin")
    try:
        return ZoneInfo(tz)
    except (ZoneInfoNotFoundError, ValueError):
        raise CronError(f"unknown timezone {tz!r}") from None


def _parse_field(text: str, lo: int, hi: int, names: dict[str, int] | None = None) -> tuple[frozenset[int], bool]:
    """Returns the set of values a field matches and whether it was a bare '*'."""
    values: set[int] = set()
    for part in text.split(","):
        if not part:
            raise CronError(f"empty element in {text!r}")
        step = 1
        if "/" in part:
            part, step_s = part.split("/", 1)
            if not step_s.isdigit() or int(step_s) < 1:
                raise CronError(f"invalid step in {text!r}")
            step = int(step_s)

        def value(s: str) -> int:
            s = s.upper()
            if names and s in names:
                return names[s]
            if not s.isdigit():
                raise CronError(f"invalid value {s!r}")
            return int(s)

        if part == "*":
            start, end = lo, hi
        elif "-" in part:
            a, b = part.split("-", 1)
            start, end = value(a), value(b)
        else:
            start = value(part)
            end = hi if step > 1 else start
        if start < lo or end > hi or start > end:
            raise CronError(f"value out of range in {text!r} (allowed {lo}-{hi})")
        values.update(range(start, end + 1, step))
    return frozenset(values), text == "*"


@dataclass(frozen=True)
class CronSpec:
    expr: str
    tz: ZoneInfo
    minutes: frozenset[int]
    hours: frozenset[int]
    doms: frozenset[int]
    months: frozenset[int]
    dows: frozenset[int]
    dom_star: bool
    dow_star: bool

    @property
    def every_hour(self) -> bool:
        return len(self.hours) == 24

    def _day_matches(self, d: date) -> bool:
        if d.month not in self.months:
            return False
        dom_ok = d.day in self.doms
        dow_ok = (d.isoweekday() % 7) in self.dows  # Sunday = 0
        # Standard cron: when both fields are restricted, either may match.
        if self.dom_star and self.dow_star:
            return True
        if self.dom_star:
            return dow_ok
        if self.dow_star:
            return dom_ok
        return dom_ok or dow_ok

    @functools.lru_cache(maxsize=128)  # noqa: B019 - specs are few and immutable
    def _instants_on(self, d: date) -> list[datetime]:
        """All instants on local date ``d`` at which the expression fires."""
        out: list[datetime] = []
        for h in sorted(self.hours):
            for m in sorted(self.minutes):
                wall = datetime(d.year, d.month, d.day, h, m)
                if self.every_hour:
                    # Real time: a repeated wall time fires in both passes;
                    # a skipped wall time does not exist and fires never.
                    for fold in (0, 1):
                        inst = wall.replace(tzinfo=self.tz, fold=fold)
                        if _roundtrips(inst, wall):
                            out.append(inst.astimezone(UTC))
                else:
                    out.append(_wall_to_instant(wall, self.tz))
        return sorted(set(out))

    def next(self, after: datetime) -> datetime | None:
        """First occurrence strictly after ``after``, or None if it never fires."""
        after = after.astimezone(UTC)
        start = after.astimezone(self.tz).date() - timedelta(days=1)
        for i in range(_SEARCH_DAYS):
            d = start + timedelta(days=i)
            if not self._day_matches(d):
                continue
            for inst in self._instants_on(d):
                if inst > after:
                    return inst
        return None

    def next_n(self, after: datetime, n: int) -> list[datetime]:
        out: list[datetime] = []
        t = after
        for _ in range(n):
            nxt = self.next(t)
            if nxt is None:
                break
            out.append(nxt)
            t = nxt
        return out


def _roundtrips(aware: datetime, wall: datetime) -> bool:
    back = aware.astimezone(UTC).astimezone(aware.tzinfo)
    return back.replace(tzinfo=None) == wall


def _wall_to_instant(wall: datetime, tz: ZoneInfo) -> datetime:
    """Maps a wall-clock reading to one instant: the earlier one when it is
    repeated, and shifted forward by the gap when it does not exist."""
    first = wall.replace(tzinfo=tz, fold=0)
    if _roundtrips(first, wall):
        return first.astimezone(UTC)
    # In a gap: interpret the wall time with the offset in force *before* the
    # transition, which lands the same distance past the jump.
    before = (wall - timedelta(hours=12)).replace(tzinfo=tz).utcoffset()
    return (wall - before).replace(tzinfo=UTC)


def parse(expr: str, tz: str) -> CronSpec:
    """Parses and validates a cron expression bound to a timezone."""
    expr = (expr or "").strip()
    if not expr:
        raise CronError("cron expression is required")
    if len(expr) > 100:
        raise CronError("cron expression is too long")
    upper = expr.upper()
    if upper.startswith(("CRON_TZ=", "TZ=")):
        raise CronError("set the timezone field instead of a TZ prefix")
    if expr.startswith("@every"):
        raise CronError("@every is not supported; use a cron expression")
    zone = load_timezone(tz)
    body = _DESCRIPTORS.get(expr.lower(), expr)
    if body.startswith("@"):
        raise CronError(f"unknown descriptor {expr!r}")
    fields = body.split()
    if len(fields) != 5:
        raise CronError("expected 5 fields: minute hour day-of-month month day-of-week")
    minutes, _ = _parse_field(fields[0], 0, 59)
    hours, _ = _parse_field(fields[1], 0, 23)
    doms, dom_star = _parse_field(fields[2], 1, 31)
    months, _ = _parse_field(fields[3], 1, 12, _MONTHS)
    dows, dow_star = _parse_field(fields[4], 0, 7, _DAYS)
    if 7 in dows:  # 7 is an alias for Sunday
        dows = (dows - {7}) | {0}
    spec = CronSpec(expr, zone, minutes, hours, doms, months, frozenset(dows), dom_star, dow_star)
    if spec.next(datetime.now(UTC)) is None:
        raise CronError("cron expression never fires")
    return spec


@dataclass
class Plan:
    """The scheduler's decision for one due recurring job."""

    enqueue: list[datetime]
    skipped: int
    next: datetime | None


def plan_cron(spec: CronSpec, due: datetime, now: datetime, policy: str) -> Plan:
    """Decides which occurrences to enqueue, given that next_run_at (``due``)
    is at or before ``now``.

    * skip:     run occurrences within MISFIRE_GRACE; drop older ones.
    * run_once: coalesce every due occurrence into one run (the latest).
    * run_all:  run every due occurrence, capped at MAX_CATCH_UP (latest kept).
    """
    occ: list[datetime] = []
    t: datetime | None = due
    scanned = 0
    while t is not None and t <= now and scanned < _MAX_SCAN:
        occ.append(t)
        t = spec.next(t)
        scanned += 1
    nxt = spec.next(now) if scanned >= _MAX_SCAN else t
    plan = Plan(enqueue=[], skipped=0, next=nxt)
    if not occ:
        return plan
    if policy == "skip":
        for o in occ:
            if now - o <= MISFIRE_GRACE:
                plan.enqueue.append(o)
            else:
                plan.skipped += 1
    elif policy == "run_all":
        if len(occ) > MAX_CATCH_UP:
            plan.skipped = len(occ) - MAX_CATCH_UP
            occ = occ[-MAX_CATCH_UP:]
        plan.enqueue = occ
    else:  # run_once
        plan.enqueue = [occ[-1]]
        plan.skipped = len(occ) - 1
    return plan
