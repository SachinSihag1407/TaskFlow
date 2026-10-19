# Developer shortcuts. Docker Compose drives the full stack; the Go and Node
# targets run against local toolchains.

COMPOSE ?= docker compose
TEST_DB ?= postgres://taskflow:taskflow@localhost:5432/taskflow_test?sslmode=disable
TEST_REDIS ?= redis://localhost:6379/1

.PHONY: help up down logs ps seed migrate scale test test-backend test-integration test-frontend e2e lint fmt build loadtest drills test-deps

help: ## List targets
	@grep -E '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "}; {printf "  %-18s %s\n", $$1, $$2}'

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

test-backend: ## Go unit tests (integration tests skip without databases)
	cd backend && go test ./...

test-integration: ## Go tests incl. integration, with the race detector (needs test-deps)
	cd backend && TASKFLOW_TEST_DATABASE_URL="$(TEST_DB)" TASKFLOW_TEST_REDIS_URL="$(TEST_REDIS)" go test -race -count=1 ./...

test-frontend: ## Frontend unit tests
	cd frontend && npm test

test: test-integration test-frontend ## All automated tests except e2e

e2e: ## Playwright against the running stack (make up seed first)
	cd frontend && E2E_BASE_URL=http://localhost:8080 npx playwright test

drills: ## Failure drills against the running stack: make drills PASSWORD=<seed password>
	BASE_URL=http://localhost:8080 PASSWORD="$(PASSWORD)" bash scripts/failure-drills.sh

loadtest: ## Load test: make loadtest PASSWORD=<seed password> JOBS=1000
	cd backend && go run ./cmd/taskflow loadtest -url http://localhost:8080 -email admin@example.com -password "$(PASSWORD)" -jobs $(or $(JOBS),1000)

lint: ## Linters and format checks
	cd backend && test -z "$$(gofmt -l .)" && go vet ./... && go run honnef.co/go/tools/cmd/staticcheck@2025.1.1 ./...
	cd frontend && npm run lint && npm run format:check && npm run typecheck

fmt: ## Format code
	cd backend && gofmt -w .
	cd frontend && npm run format

build: ## Build backend binary and frontend bundle
	cd backend && go build -o bin/taskflow ./cmd/taskflow
	cd frontend && npm run build
