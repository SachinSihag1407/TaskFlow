# Developer shortcuts. Docker Compose drives the full stack; the Python and
# Node targets run against local toolchains (backend: `make venv` first).

COMPOSE ?= docker compose
PY ?= backend/.venv/bin/python
TEST_DB ?= postgres://taskflow:taskflow@localhost:5432/taskflow_test?sslmode=disable
TEST_REDIS ?= redis://localhost:6379/1

.PHONY: help venv up down logs ps seed migrate scale test test-backend test-integration test-frontend e2e lint fmt build loadtest drills test-deps

help: ## List targets
	@grep -E '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "}; {printf "  %-18s %s\n", $$1, $$2}'

venv: ## Create backend/.venv with runtime and dev dependencies
	python3.12 -m venv backend/.venv && $(PY) -m pip install -r backend/requirements-dev.txt

up: ## Build and start the full stack
	$(COMPOSE) up -d --build --wait

down: ## Stop the stack (keeps volumes)
	$(COMPOSE) down

logs: ## Follow logs of all services
	$(COMPOSE) logs -f --tail=100

ps: ## Show service status
	$(COMPOSE) ps

migrate: ## Apply database migrations
	$(COMPOSE) run --rm migrate

seed: ## Create demo users and jobs
	$(COMPOSE) run --rm seed

scale: ## Run N workers: make scale N=4
	$(COMPOSE) up -d --scale worker=$(or $(N),3) worker

test-deps: ## Start throwaway PostgreSQL + Redis for integration tests
	docker run -d --rm --name tf-test-pg -e POSTGRES_USER=taskflow -e POSTGRES_PASSWORD=taskflow -e POSTGRES_DB=taskflow_test -p 5432:5432 postgres:16-alpine
	docker run -d --rm --name tf-test-redis -p 6379:6379 redis:7.4-alpine

test-backend: ## Backend unit tests (integration tests skip without databases)
	cd backend && ../$(PY) -m pytest -q

test-integration: ## Backend tests incl. integration against real PostgreSQL + Redis (needs test-deps)
	cd backend && TASKFLOW_TEST_DATABASE_URL="$(TEST_DB)" TASKFLOW_TEST_REDIS_URL="$(TEST_REDIS)" ../$(PY) -m pytest -q

test-frontend: ## Frontend unit tests
	cd frontend && npm test

test: test-integration test-frontend ## All automated tests except e2e

e2e: ## Playwright against the running stack (make up seed first)
	cd frontend && E2E_BASE_URL=http://localhost:8080 npx playwright test

drills: ## Failure drills against the running stack: make drills PASSWORD=<seed password>
	BASE_URL=http://localhost:8080 PASSWORD="$(PASSWORD)" bash scripts/failure-drills.sh

loadtest: ## Load test: make loadtest PASSWORD=<seed password> JOBS=1000
	cd backend && ../$(PY) -m taskflow loadtest --url http://localhost:8080 --email admin@example.com --password "$(PASSWORD)" --jobs $(or $(JOBS),1000)

lint: ## Linters and format checks
	cd backend && ../$(PY) -m ruff check taskflow tests
	cd frontend && npm run lint && npm run format:check && npm run typecheck

fmt: ## Format code
	cd backend && ../$(PY) -m ruff check --fix taskflow tests
	cd frontend && npm run format

build: ## Byte-compile the backend and build the frontend bundle
	cd backend && ../$(PY) -m compileall -q taskflow
	cd frontend && npm run build
