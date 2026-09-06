# linkflow -- see PLAN.md for the phase plan these targets support.

BASE_URL ?= http://localhost:8080
SEED_COUNT ?= 10000
VUS ?= 100
DURATION ?= 60s
ZIPF_S ?= 1.2
ANALYTICS_MODE ?= batch
ANALYTICS_SINK ?= postgres
STAMPEDE_N ?= 10000
TEST_DATABASE_URL ?= postgres://linkflow:linkflow@localhost:5433/linkflow_test?sslmode=disable

.PHONY: help
help: ## Show available targets
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Compile the service binary to bin/linkflow
	go build -o bin/linkflow ./cmd/linkflow

.PHONY: run
run: ## Run the service against a local Postgres
	go run ./cmd/linkflow

.PHONY: test
test: ## Run unit tests (no external dependencies required)
	go test ./...

.PHONY: test-db
test-db: ## Create the linkflow_test database inside the compose Postgres
	docker compose exec -T postgres psql -U linkflow -d postgres \
		-c "CREATE DATABASE linkflow_test" || true

.PHONY: test-integration
test-integration: ## Run tests that need a live Postgres (set LINKFLOW_TEST_DATABASE_URL)
	LINKFLOW_TEST_DATABASE_URL=$(TEST_DATABASE_URL) go test -tags=integration -count=1 ./...

.PHONY: vet
vet: ## go vet the module
	go vet ./...

.PHONY: fmt
fmt: ## Format all Go source
	gofmt -w cmd internal bench

.PHONY: check
check: fmt vet test ## Format, vet and test

.PHONY: up
up: ## Start the stack (ANALYTICS_MODE=batch|sync, ANALYTICS_SINK=postgres|clickhouse)
	LINKFLOW_ANALYTICS_MODE=$(ANALYTICS_MODE) LINKFLOW_ANALYTICS_SINK=$(ANALYTICS_SINK) \
		docker compose up --build -d

.PHONY: down
down: ## Stop the stack, keeping the Postgres volume
	docker compose down

.PHONY: clean
clean: ## Stop the stack and delete the Postgres volume
	docker compose down -v
	rm -rf bin bench/codes.json bench/results

.PHONY: logs
logs: ## Tail service logs
	docker compose logs -f linkflow

.PHONY: seed
seed: ## Create SEED_COUNT links and write bench/codes.json
	go run ./bench/seed -base-url $(BASE_URL) -count $(SEED_COUNT) -out bench/codes.json

.PHONY: bench
bench: ## Run the k6 redirect benchmark against the seeded key set
	cd bench && k6 run -e BASE_URL=$(BASE_URL) -e VUS=$(VUS) -e DURATION=$(DURATION) -e ZIPF_S=$(ZIPF_S) redirect.js

.PHONY: stats
stats: ## Show the analytics recorder counters, including drops
	@curl -sS $(BASE_URL)/debug/analytics | python3 -m json.tool

.PHONY: cache-stats
cache-stats: ## Show per-tier cache counters from /metrics
	@curl -sS $(BASE_URL)/metrics | grep -E "^linkflow_(cache_lookups|origin_queries|singleflight|shared_cache)" | grep -v "^#"

.PHONY: stampede
stampede: ## Fire STAMPEDE_N concurrent requests at a cold key and count PG queries
	go run ./bench/stampede -base-url $(BASE_URL) -concurrent $(STAMPEDE_N)

.PHONY: grafana
grafana: ## Open the Grafana dashboard
	@echo "http://localhost:$${LINKFLOW_GRAFANA_PORT:-3000}/d/linkflow-overview"
	@open "http://localhost:$${LINKFLOW_GRAFANA_PORT:-3000}/d/linkflow-overview" 2>/dev/null || true

.PHONY: bench-ramp
bench-ramp: ## Ramp concurrency to find the knee (throughput vs p99)
	@for vus in 25 50 100 200 400 800; do \
		echo "=== $$vus VUs ==="; \
		cd bench && k6 run --quiet -e BASE_URL=$(BASE_URL) -e VUS=$$vus -e DURATION=30s -e ZIPF_S=$(ZIPF_S) redirect.js; cd ..; \
	done
