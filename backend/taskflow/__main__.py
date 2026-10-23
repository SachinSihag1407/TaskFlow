"""Entry point for every TaskFlow process:

    python -m taskflow api          REST API + SSE
    python -m taskflow scheduler    scheduling, retries, lease recovery, reconciliation
    python -m taskflow worker       job execution
    python -m taskflow all-in-one   migrate + API + scheduler + worker in one process (free-tier demo)
    python -m taskflow migrate      apply database migrations
    python -m taskflow seed         create demo users and jobs (development only)
    python -m taskflow loadtest     submit jobs through the API and report measurements
    python -m taskflow healthcheck  probe an HTTP health endpoint (container healthchecks)
"""

from __future__ import annotations

import asyncio
import sys
import urllib.request

from . import __version__
from .config import ConfigError

# The healthcheck runs every few seconds inside containers, so this module
# imports only the standard library; process runtimes live in taskflow.cli.
USAGE = "usage: python -m taskflow <api|scheduler|worker|all-in-one|migrate|seed|loadtest|healthcheck|version>"


def run_healthcheck(args: list[str]) -> None:
    url = args[0] if args else "http://127.0.0.1:8080/healthz"
    if not url.startswith(("http://127.0.0.1", "http://localhost")):
        raise SystemExit("healthcheck only probes local endpoints")
    with urllib.request.urlopen(url, timeout=3) as resp:  # noqa: S310 - local URL enforced above
        if resp.status != 200:
            raise SystemExit(f"unhealthy: HTTP {resp.status}")


def main(argv: list[str]) -> int:
    if not argv:
        print(USAGE, file=sys.stderr)
        return 2
    cmd, args = argv[0], argv[1:]
    try:
        if cmd in ("api", "worker", "scheduler", "all-in-one", "migrate", "seed"):
            from . import cli
            runner = {"api": cli.run_api, "worker": cli.run_worker, "scheduler": cli.run_scheduler,
                      "all-in-one": cli.run_all_in_one, "migrate": cli.run_migrate,
                      "seed": lambda: cli.run_seed(args)}[cmd]
            asyncio.run(runner())
        elif cmd == "loadtest":
            from .loadtest import main as loadtest_main
            return loadtest_main(args)
        elif cmd == "healthcheck":
            run_healthcheck(args)
        elif cmd == "version":
            print(__version__)
        else:
            print(USAGE, file=sys.stderr)
            return 2
    except ConfigError as e:
        print(f"taskflow {cmd}: configuration error: {e}", file=sys.stderr)
        return 1
    except KeyboardInterrupt:
        return 130
    except Exception as e:  # noqa: BLE001 - top-level reporting
        print(f"taskflow {cmd}: {e}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
