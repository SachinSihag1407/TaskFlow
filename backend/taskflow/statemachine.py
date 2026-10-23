"""Single source of truth for TaskFlow's lifecycle rules.

* which run transitions are legal,
* how a job's aggregate status is derived from its runs,
* which user actions are currently permitted (with a reason when not).

The engine calls into this module; no other code decides whether a status
change is valid.
"""

from __future__ import annotations

from dataclasses import dataclass

from .domain import JobStatus, RunStatus

# Every legal run status change.
#
#   QUEUED     -> RUNNING (claimed) | CANCELLED
#   RUNNING    -> SUCCEEDED | RETRY_WAIT (retryable failure / lease lost) |
#                 FAILED (attempts exhausted / permanent error) | CANCELLED
#   RETRY_WAIT -> QUEUED (backoff elapsed) | CANCELLED
#
# SUCCEEDED, FAILED and CANCELLED are terminal. A dead-lettered (FAILED) run
# is never revived; replay creates a new run linked through replay_of.
RUN_TRANSITIONS: dict[RunStatus, frozenset[RunStatus]] = {
    RunStatus.QUEUED: frozenset({RunStatus.RUNNING, RunStatus.CANCELLED}),
    RunStatus.RUNNING: frozenset({RunStatus.SUCCEEDED, RunStatus.RETRY_WAIT, RunStatus.FAILED, RunStatus.CANCELLED}),
    RunStatus.RETRY_WAIT: frozenset({RunStatus.QUEUED, RunStatus.CANCELLED}),
}


class InvalidTransition(Exception):
    def __init__(self, src: RunStatus, dst: RunStatus):
        super().__init__(f"invalid run transition {src} -> {dst}")
        self.src, self.dst = src, dst


def can_transition(src: RunStatus | str, dst: RunStatus | str) -> bool:
    return RunStatus(dst) in RUN_TRANSITIONS.get(RunStatus(src), frozenset())


def validate(src: RunStatus | str, dst: RunStatus | str) -> None:
    if not can_transition(src, dst):
        raise InvalidTransition(RunStatus(src), RunStatus(dst))


def sources_for(dst: RunStatus) -> list[RunStatus]:
    """Every status from which ``dst`` can be reached."""
    return [src for src, dsts in RUN_TRANSITIONS.items() if dst in dsts]


@dataclass
class RunSummary:
    """In-flight run counts of a job, plus its last terminal outcome."""

    running: int = 0
    queued: int = 0
    retry_wait: int = 0
    last_terminal: RunStatus | None = None

    @property
    def active(self) -> int:
        return self.running + self.queued + self.retry_wait


@dataclass
class JobState:
    recurring: bool = False
    paused: bool = False
    cancelled: bool = False
    has_next_run: bool = False


def derive_job_status(job: JobState, runs: RunSummary) -> JobStatus:
    """Computes a job's aggregate status deterministically.

    1. CANCELLED and PAUSED are operator decisions and take precedence.
    2. Otherwise the most advanced in-flight run wins: RUNNING > QUEUED > RETRY_WAIT.
    3. Otherwise, if another occurrence is scheduled, SCHEDULED.
    4. Otherwise the outcome of the last finished run.
    5. A job with no runs and no schedule is CREATED.
    """
    if job.cancelled:
        return JobStatus.CANCELLED
    if job.paused:
        return JobStatus.PAUSED
    if runs.running:
        return JobStatus.RUNNING
    if runs.queued:
        return JobStatus.QUEUED
    if runs.retry_wait:
        return JobStatus.RETRY_WAIT
    if job.has_next_run:
        return JobStatus.SCHEDULED
    if runs.last_terminal is not None:
        return {
            RunStatus.SUCCEEDED: JobStatus.SUCCEEDED,
            RunStatus.FAILED: JobStatus.FAILED,
            RunStatus.CANCELLED: JobStatus.CANCELLED,
        }.get(RunStatus(runs.last_terminal), JobStatus.CREATED)
    return JobStatus.CREATED


EDIT, PAUSE, RESUME, CANCEL, RETRY, RUN_NOW = "edit", "pause", "resume", "cancel", "retry", "run_now"


def _allow() -> dict:
    return {"allowed": True}


def _deny(reason: str) -> dict:
    return {"allowed": False, "reason": reason}


def job_actions(status: JobStatus | str, recurring: bool, runs: RunSummary, last_run_replayable: bool) -> dict[str, dict]:
    """Which job-level actions are valid now. The same rules are enforced
    server-side when the action is invoked; this lets clients explain
    unavailable actions instead of guessing."""
    status = JobStatus(status)
    finished_once = not recurring and status in (JobStatus.SUCCEEDED, JobStatus.FAILED)
    a: dict[str, dict] = {}

    if status == JobStatus.CANCELLED:
        a[EDIT] = _deny("Cancelled jobs cannot be edited.")
    elif finished_once:
        a[EDIT] = _deny("This one-time job has already finished. Create a new job instead.")
    elif not recurring and runs.active > 0:
        a[EDIT] = _deny("A run is in progress and uses the configuration captured when it was queued.")
    else:
        a[EDIT] = _allow()

    if status == JobStatus.PAUSED:
        a[PAUSE], a[RESUME] = _deny("The job is already paused."), _allow()
    elif status == JobStatus.CANCELLED:
        a[PAUSE], a[RESUME] = _deny("Cancelled jobs cannot be paused."), _deny("Cancelled jobs cannot be resumed.")
    elif finished_once:
        a[PAUSE], a[RESUME] = _deny("This one-time job has already finished."), _deny("The job is not paused.")
    elif not recurring and status == JobStatus.RUNNING:
        a[PAUSE] = _deny("The job is executing. Cancel it instead, or wait for it to finish.")
        a[RESUME] = _deny("The job is not paused.")
    else:
        a[PAUSE], a[RESUME] = _allow(), _deny("The job is not paused.")

    if status == JobStatus.CANCELLED:
        a[CANCEL] = _deny("The job is already cancelled.")
    elif finished_once:
        a[CANCEL] = _deny("This one-time job has already finished.")
    else:
        a[CANCEL] = _allow()

    if recurring:
        a[RETRY] = _deny("Recurring jobs retry automatically. Replay individual failed runs from the dead-letter queue.")
    elif status != JobStatus.FAILED:
        a[RETRY] = _deny("Only failed jobs can be retried.")
    elif not last_run_replayable:
        a[RETRY] = _deny("The failed run has already been retried.")
    else:
        a[RETRY] = _allow()

    if not recurring:
        a[RUN_NOW] = _deny("Run now is available for recurring jobs.")
    elif status == JobStatus.CANCELLED:
        a[RUN_NOW] = _deny("Cancelled jobs cannot be run.")
    elif status == JobStatus.PAUSED:
        a[RUN_NOW] = _deny("Resume the job before running it.")
    else:
        a[RUN_NOW] = _allow()
    return a
