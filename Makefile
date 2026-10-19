# Shortcuts. Everything here is plain `docker compose` or `go`; on Windows without make, copy the commands.
SHELL := /bin/bash
.PHONY: help up down logs migrate migrate-status seed-dev test test-local lint fmt build sqlc

help:
	@grep -E '^[a-z-]+:.*##' $(MAKEFILE_LIST) | sed 's/:.*##/ -/'

up: ## start PostgreSQL and the API in Docker
	docker compose up --build -d

down: ## stop everything (data is kept in the postgres_data volume)
	docker compose down

logs: ## follow API logs
	docker compose logs -f backend

migrate: ## apply pending migrations (explicit, never automatic)
	docker compose run --rm --entrypoint /app/migrate backend up

migrate-status: ## list migrations and whether they are applied
	docker compose run --rm --entrypoint /app/migrate backend status

seed-dev: ## load FAKE development users (refuses when APP_ENV=production)
	docker compose run --rm --entrypoint /app/seed-dev backend

test: ## all Go tests including database tests, in Docker
	docker compose run --rm test

test-local: ## Go tests on this machine; database tests skip unless TEST_DATABASE_URL is set
	go test -count=1 ./...
	@test -n "$$TEST_DATABASE_URL" || echo "NOTE: TEST_DATABASE_URL not set, database tests were SKIPPED (nothing verified for them)"

lint: ## gofmt check and go vet
	@test -z "$$(gofmt -l .)" || (gofmt -l . && echo "gofmt needed" && exit 1)
	go vet ./...

fmt:
	gofmt -w .

build: ## build the binaries into ./bin
	go build -o bin/ ./cmd/...

sqlc: ## regenerate the repository layer after changing database/queries or table migrations
	sqlc generate
