"""Unit tests: state machine, cron/DST, missed-run planning, backoff."""

from datetime import UTC, datetime, timedelta

import pytest

from taskflow import cron
from taskflow import statemachine as sm
from taskflow.domain import JobStatus, RunStatus
from taskflow.retry import Policy


def ts(s: str) -> datetime:
    return datetime.fromisoformat(s.replace("Z", "+00:00")).astimezone(UTC)


def iso(ds: list[datetime]) -> list[str]:
    return [d.strftime("%Y-%m-%dT%H:%M:%SZ") for d in ds]


# ---- state machine -----------------------------------------------------------

LEGAL = {
    ("QUEUED", "RUNNING"), ("QUEUED", "CANCELLED"),
    ("RUNNING", "SUCCEEDED"), ("RUNNING", "RETRY_WAIT"), ("RUNNING", "FAILED"), ("RUNNING", "CANCELLED"),
    ("RETRY_WAIT", "QUEUED"), ("RETRY_WAIT", "CANCELLED"),
}


def test_transition_table_is_exactly_as_documented():
    for src in RunStatus:
        for dst in RunStatus:
            assert sm.can_transition(src, dst) == ((src.value, dst.value) in LEGAL), (src, dst)


def test_terminal_states_have_no_exits():
    for s in (RunStatus.SUCCEEDED, RunStatus.FAILED, RunStatus.CANCELLED):
        assert s.terminal and s not in sm.RUN_TRANSITIONS


def test_running_only_reachable_from_queued():
    assert sm.sources_for(RunStatus.RUNNING) == [RunStatus.QUEUED]
    with pytest.raises(sm.InvalidTransition):
        sm.validate(RunStatus.QUEUED, RunStatus.SUCCEEDED)


@pytest.mark.parametrize(
    "job,runs,want",
    [
        (sm.JobState(), sm.RunSummary(), JobStatus.CREATED),
        (sm.JobState(has_next_run=True), sm.RunSummary(), JobStatus.SCHEDULED),
        (sm.JobState(), sm.RunSummary(queued=1), JobStatus.QUEUED),
        (sm.JobState(), sm.RunSummary(running=1, queued=2), JobStatus.RUNNING),
        (sm.JobState(), sm.RunSummary(queued=1, retry_wait=1), JobStatus.QUEUED),
        (sm.JobState(), sm.RunSummary(retry_wait=1), JobStatus.RETRY_WAIT),
        (sm.JobState(), sm.RunSummary(last_terminal=RunStatus.SUCCEEDED), JobStatus.SUCCEEDED),
        (sm.JobState(), sm.RunSummary(last_terminal=RunStatus.FAILED), JobStatus.FAILED),
        (sm.JobState(recurring=True, has_next_run=True), sm.RunSummary(last_terminal=RunStatus.FAILED), JobStatus.SCHEDULED),
        (sm.JobState(paused=True), sm.RunSummary(running=1), JobStatus.PAUSED),
        (sm.JobState(cancelled=True, paused=True, has_next_run=True), sm.RunSummary(running=1), JobStatus.CANCELLED),
    ],
)
def test_derive_job_status(job, runs, want):
    assert sm.derive_job_status(job, runs) == want


def test_every_denied_action_has_a_reason():
    for status in JobStatus:
        for recurring in (True, False):
            for name, st in sm.job_actions(status, recurring, sm.RunSummary(), False).items():
                assert st["allowed"] or st.get("reason"), (status, recurring, name)


def test_failed_one_time_job_retry_once():
    assert sm.job_actions(JobStatus.FAILED, False, sm.RunSummary(), True)["retry"]["allowed"]
    assert not sm.job_actions(JobStatus.FAILED, False, sm.RunSummary(), False)["retry"]["allowed"]


# ---- cron ---------------------------------------------------------------------


@pytest.mark.parametrize(
    "expr,tz",
    [("", "UTC"), ("* * * *", "UTC"), ("0 0 * * * *", "UTC"), ("61 * * * *", "UTC"), ("0 0 30 2 *", "UTC"),
     ("CRON_TZ=UTC 0 * * * *", "UTC"), ("@every 5m", "UTC"), ("0 * * * *", ""), ("0 * * * *", "Local"),
     ("0 * * * *", "Mars/Olympus"), ("*/0 * * * *", "UTC")],
)
def test_invalid_cron(expr, tz):
    with pytest.raises(cron.CronError):
        cron.parse(expr, tz)


@pytest.mark.parametrize("expr", ["*/5 * * * *", "0 9 * * 1-5", "@daily", "@hourly", "15 3 1 * *", "0 0 * * SUN", "0 0 1 JAN *"])
def test_valid_cron(expr):
    cron.parse(expr, "Europe/Berlin")


def test_timezone_aware_and_utc():
    spec = cron.parse("0 9 * * *", "Asia/Kolkata")
    got = spec.next(ts("2026-06-01T00:00:00Z"))
    assert got == ts("2026-06-01T03:30:00Z") and got.tzinfo == UTC


def test_dst_spring_forward_fixed_time_shifts_forward():
    spec = cron.parse("30 2 * * *", "America/New_York")
    assert iso(spec.next_n(ts("2026-03-07T12:00:00Z"), 3)) == [
        "2026-03-08T07:30:00Z",  # 03:30 EDT: shifted forward, still runs that day
        "2026-03-09T06:30:00Z",
        "2026-03-10T06:30:00Z",
    ]


def test_dst_fall_back_fixed_time_runs_once():
    spec = cron.parse("30 1 * * *", "America/New_York")
    assert iso(spec.next_n(ts("2026-10-31T12:00:00Z"), 3)) == [
        "2026-11-01T05:30:00Z",  # 01:30 EDT, first pass only
        "2026-11-02T06:30:00Z",
        "2026-11-03T06:30:00Z",
    ]
    assert spec.next(ts("2026-11-01T06:10:00Z")) == ts("2026-11-02T06:30:00Z")


def test_dst_hourly_follows_real_time():
    spec = cron.parse("0 * * * *", "America/New_York")
    assert iso(spec.next_n(ts("2026-11-01T04:30:00Z"), 3)) == [
        "2026-11-01T05:00:00Z", "2026-11-01T06:00:00Z", "2026-11-01T07:00:00Z"]
    assert iso(spec.next_n(ts("2026-03-08T05:30:00Z"), 2)) == ["2026-03-08T06:00:00Z", "2026-03-08T07:00:00Z"]


def test_dom_dow_or_semantics():
    # "1st of the month OR any Monday"
    spec = cron.parse("0 0 1 * MON", "UTC")
    assert iso(spec.next_n(ts("2026-06-01T00:00:00Z"), 2)) == ["2026-06-08T00:00:00Z", "2026-06-15T00:00:00Z"]


def test_plan_cron_policies():
    spec = cron.parse("*/10 * * * *", "UTC")
    due, now = ts("2026-01-01T00:00:00Z"), ts("2026-01-01T01:00:30Z")
    p = cron.plan_cron(spec, due, now, "run_once")
    assert iso(p.enqueue) == ["2026-01-01T01:00:00Z"] and p.skipped == 6 and p.next == ts("2026-01-01T01:10:00Z")
    p = cron.plan_cron(spec, due, now, "skip")
    assert len(p.enqueue) == 1 and p.skipped == 6
    assert len(cron.plan_cron(spec, due, ts("2026-01-01T01:05:00Z"), "skip").enqueue) == 0
    p = cron.plan_cron(spec, due, now, "run_all")
    assert len(p.enqueue) == 7 and p.enqueue == sorted(p.enqueue)
    p = cron.plan_cron(spec, due, due + timedelta(hours=24), "run_all")
    assert len(p.enqueue) == cron.MAX_CATCH_UP and p.skipped == 145 - cron.MAX_CATCH_UP


def test_long_outage_terminates():
    spec = cron.parse("* * * * *", "UTC")
    due = ts("2026-01-01T00:00:00Z")
    p = cron.plan_cron(spec, due, due + timedelta(days=365), "run_once")
    assert len(p.enqueue) == 1 and p.next > due + timedelta(days=365)


# ---- retry --------------------------------------------------------------------


def test_backoff_grows_and_caps():
    p = Policy(base=10, max=300)
    for attempt, ceiling in [(1, 10), (2, 20), (3, 40), (5, 160), (6, 300), (60, 300)]:
        assert p.bounds(attempt) == (ceiling / 2, ceiling)
        assert p.delay(attempt, lambda: 0) == ceiling / 2


def test_jitter_spreads_retries():
    p = Policy(base=10, max=3600)
    seen = {p.delay(3) for _ in range(200)}
    assert len(seen) > 150 and all(20 <= d <= 40 for d in seen)
