#!/usr/bin/env bash
# Reproducible failure drills against a running Docker Compose stack.
#
#   ./scripts/failure-drills.sh            # all drills
#   ./scripts/failure-drills.sh crash      # one drill: crash | redis | restart
#
# Requires: docker compose stack running (see README), curl, jq, and demo
# users (docker compose run --rm seed). Environment:
#   BASE_URL (default http://localhost:8080), EMAIL, PASSWORD (=SEED_PASSWORD)
set -euo pipefail

BASE_URL=${BASE_URL:-http://localhost:8080}
EMAIL=${EMAIL:-admin@example.com}
PASSWORD=${PASSWORD:?set PASSWORD to the seed password}
COMPOSE=${COMPOSE:-docker compose}
JAR=$(mktemp)
trap 'rm -f "$JAR"' EXIT

log() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
ok() { printf '   \033[32m✓\033[0m %s\n' "$*"; }
fail() { printf '   \033[31m✗ %s\033[0m\n' "$*"; exit 1; }

CSRF=$(curl -sf -c "$JAR" -H 'Content-Type: application/json' \
  -d "{\"email\":\"$EMAIL\",\"password\":\"$PASSWORD\"}" "$BASE_URL/api/v1/auth/login" | jq -r .data.csrf_token)
[ -n "$CSRF" ] && [ "$CSRF" != null ] || fail "login failed"

api() { curl -sf -b "$JAR" -H 'Content-Type: application/json' -H "X-CSRF-Token: $CSRF" "$@"; }

create_job() { # name payload max_attempts
  api -d "{\"name\":\"$1\",\"handler\":\"test.sleep\",\"schedule_type\":\"once\",\"payload\":$2,\"max_attempts\":$3,\"backoff_base_seconds\":2,\"backoff_max_seconds\":5}" \
    "$BASE_URL/api/v1/jobs" | jq -r .data.id
}

job_status() { api "$BASE_URL/api/v1/jobs/$1" | jq -r .data.job.status; }

wait_status() { # job_id status timeout_s
  local t=0
  until [ "$(job_status "$1")" = "$2" ]; do
    sleep 1; t=$((t + 1))
    if [ "$t" -ge "$3" ]; then fail "job $1 did not reach $2 within $3s (now $(job_status "$1"))"; fi
  done
  return 0
}

drill_crash() {
  log "Drill 1: worker crash (SIGKILL) during execution"
  local id run worker cid
  id=$(create_job "drill: crash recovery" '{"duration_ms":20000}' 3)
  wait_status "$id" RUNNING 30
  run=$(api "$BASE_URL/api/v1/jobs/$id" | jq -r '.data.runs[0]')
  worker=$(jq -r .worker_id <<<"$run")
  host=$(api "$BASE_URL/api/v1/workers/$worker" | jq -r .data.worker.hostname)
  ok "job running on worker ${worker:0:8} (container $host)"
  cid=$(docker ps -q --filter "id=$host" | head -1)
  [ -n "$cid" ] || fail "could not find container $host"
  docker kill -s KILL "$cid" >/dev/null
  ok "killed container $host with SIGKILL (no graceful shutdown)"
  echo "   waiting for the lease to expire (LEASE_DURATION, default 30s) and another worker to retry…"
  wait_status "$id" SUCCEEDED 120
  api "$BASE_URL/api/v1/jobs/$id" | jq -r '.data.attempts | sort_by(.attempt_number)[] | "   attempt \(.attempt_number): \(.status) \(.error_kind // "") on \(.worker_hostname)"'
  ok "run recovered: first attempt LOST, retried to SUCCEEDED on a surviving worker"
  $COMPOSE up -d worker >/dev/null 2>&1 && ok "worker container restarted"
}

drill_redis() {
  log "Drill 2: Redis outage"
  $COMPOSE stop redis >/dev/null 2>&1
  ok "redis stopped"
  local id
  id=$(create_job "drill: redis outage" '{"duration_ms":500}' 2)
  wait_status "$id" SUCCEEDED 60
  ok "job executed without Redis (workers fall back to polling PostgreSQL)"
  api "$BASE_URL/api/v1/system/health" | jq -r '.data.components[] | select(.name=="redis") | "   redis component: \(.status) — \(.detail)"'
  $COMPOSE start redis >/dev/null 2>&1
  ok "redis restarted"
  sleep 12
  id=$(create_job "drill: after redis restart" '{"duration_ms":200}' 2)
  wait_status "$id" SUCCEEDED 30
  ok "queue-based dispatch resumed after Redis returned"
}

drill_restart() {
  log "Drill 3: scheduler restart does not duplicate occurrences"
  local id before after
  id=$(api -d '{"name":"drill: every minute","handler":"test.sleep","schedule_type":"cron","cron_expr":"* * * * *","timezone":"UTC","payload":{"duration_ms":100},"overlap_policy":"allow","max_concurrency":5}' \
    "$BASE_URL/api/v1/jobs" | jq -r .data.id)
  $COMPOSE restart scheduler >/dev/null 2>&1
  $COMPOSE restart scheduler >/dev/null 2>&1
  sleep 70
  before=$(api "$BASE_URL/api/v1/jobs/$id/runs?page_size=200" | jq '[.data[].scheduled_for] | length')
  after=$(api "$BASE_URL/api/v1/jobs/$id/runs?page_size=200" | jq '[.data[].scheduled_for] | unique | length')
  [ "$before" = "$after" ] || fail "duplicate occurrences: $before runs, $after unique"
  ok "$before run(s), all with distinct scheduled_for despite scheduler restarts"
  api -X POST "$BASE_URL/api/v1/jobs/$id/cancel" >/dev/null && ok "drill job cancelled"
}

case "${1:-all}" in
  crash) drill_crash ;;
  redis) drill_redis ;;
  restart) drill_restart ;;
  all) drill_crash; drill_redis; drill_restart ;;
  *) echo "usage: $0 [crash|redis|restart|all]"; exit 2 ;;
esac
log "All requested drills passed"
