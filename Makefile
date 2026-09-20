SHELL := /bin/bash
COMPOSE := docker compose -f deploy/docker-compose.yml
API := http://localhost:8082

.PHONY: help
help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Compile every binary
	go build ./...

.PHONY: test
test: ## Run unit tests
	go test ./... -count=1

.PHONY: lint
lint: ## gofmt + go vet
	@test -z "$$(gofmt -l . | tee /dev/stderr)" || (echo "run gofmt -w ." && exit 1)
	go vet ./...

.PHONY: up
up: ## Start the stack and seed the benchmark database
	$(COMPOSE) up -d --build
	@echo "api on $(API) — first start seeds 1.5M orders, give it a minute"

.PHONY: down
down: ## Stop the stack and delete its volumes
	$(COMPOSE) down -v

.PHONY: infra
infra: ## Start only postgres, redis and kafka
	$(COMPOSE) up -d postgres redis kafka

.PHONY: logs
logs: ## Tail the worker
	$(COMPOSE) logs -f worker

.PHONY: optimize
optimize: ## Submit a query: make optimize SQL="SELECT ..."
	@test -n "$(SQL)" || (echo 'usage: make optimize SQL="SELECT ..."' && exit 1)
	@curl -sS -X POST $(API)/v1/optimizations -H 'content-type: application/json' \
		-d "$$(python3 -c 'import json,sys; print(json.dumps({"sql": sys.argv[1]}))' "$(SQL)")" \
		| python3 -m json.tool

.PHONY: report
report: ## Fetch a finished report: make report JOB=<uuid>
	@test -n "$(JOB)" || (echo 'usage: make report JOB=<uuid>' && exit 1)
	@curl -sS $(API)/v1/optimizations/$(JOB) | python3 -m json.tool

.PHONY: analyze
analyze: ## Parse a query without touching the database: make analyze SQL="SELECT ..."
	@test -n "$(SQL)" || (echo 'usage: make analyze SQL="SELECT ..."' && exit 1)
	@curl -sS -X POST $(API)/v1/analyze -H 'content-type: application/json' \
		-d "$$(python3 -c 'import json,sys; print(json.dumps({"sql": sys.argv[1]}))' "$(SQL)")" \
		| python3 -m json.tool

.PHONY: bench
bench: ## Score the optimizer against the benchmark queries
	go run ./cmd/bench -out testdata/bench-report.json

.PHONY: bench-rules
bench-rules: ## Benchmark with rule-derived candidates only (no LLM, no API key needed)
	go run ./cmd/bench -skip-llm -out testdata/bench-report-rules.json

.PHONY: mysql-up
mysql-up: ## Start the MySQL benchmark database (opt-in profile)
	$(COMPOSE) --profile mysql up -d mysql
	@echo "seeding 3.8M rows — watch with: docker logs -f queryforge-mysql-1"

.PHONY: mysql-down
mysql-down: ## Stop MySQL and delete its volume
	$(COMPOSE) --profile mysql down -v

.PHONY: test-mysql
test-mysql: ## Run the MySQL integration tests against a running mysql-up
	MYSQL_TEST_DSN='queryforge:queryforge@tcp(127.0.0.1:3307)/shop' go test ./... -count=1
