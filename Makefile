SHELL := /usr/bin/env bash
CLUSTER := golang-sole
NS := sole
GO_PKGS := ./cmd/... ./internal/...

.PHONY: help deps deps-down run test test-unit test-integration lint vuln images cluster-up cluster-down cluster-status smoke e2e logs

help: ## what each target does
	@grep -E '^[a-z-]+:.*##' $(MAKEFILE_LIST) | sed 's/:.*##/ -/'

deps: ## Postgres, Kafka and RabbitMQ in Docker for `go run` and `go test`
	docker compose -f deploy/compose.deps.yaml up -d --wait

deps-down: ## stop them and drop their data
	docker compose -f deploy/compose.deps.yaml down -v

run: ## the API on :5228 against `make deps`
	JWT_KEY=dev-signing-key-0123456789abcdef-0123 ADMIN_EMAIL=admin@sole.local ADMIN_PASSWORD=SoleAdmin2026 \
	AUTH_REQUIRE_CONFIRMED_EMAIL=false MAIL_DEV_MAILBOX=true ORDERS_FINAL_MARK_DELAY_MS=1500 RAFFLES_DRAW_INTERVAL_MS=500 \
	RATE_LIMIT_AUTH_PER_MINUTE=120 JWT_REVOCATION_CACHE_SECONDS=2 LISTEN_ADDR=:5228 go run ./cmd/api

test: test-unit test-integration ## everything

test-unit: ## no Docker needed
	go test $(shell go list $(GO_PKGS) | grep -v /integration)

test-integration: ## needs `make deps` (a real Postgres); uses the database sole_test
	go test -count=1 ./internal/integration/...

lint: ## gofmt, go vet
	@test -z "$$(gofmt -l cmd internal)" || (gofmt -l cmd internal; echo "run gofmt -w"; exit 1)
	go vet $(GO_PKGS)

vuln: ## known-vulnerable dependencies
	go run golang.org/x/vuln/cmd/govulncheck@latest $(GO_PKGS)

images: ## build the two images
	docker build --target api -t golang-sole-api:dev .
	docker build --target web -t golang-sole-web:dev .

cluster-up: images ## kind cluster + images + manifests; waits until everything is Ready
	./scripts/cluster-up.sh

cluster-down: ## delete the kind cluster
	kind delete cluster --name $(CLUSTER)

cluster-status: ## what is running
	kubectl --context kind-$(CLUSTER) -n $(NS) get pods,svc,pdb

logs: ## follow the API logs of every replica
	kubectl --context kind-$(CLUSTER) -n $(NS) logs -f -l app.kubernetes.io/name=api --max-log-requests=5 --prefix

smoke: ## the HTTP script against the cluster (http://127.0.0.1:5228)
	./scripts/smoke.sh

e2e: ## the Playwright specs against the cluster (http://127.0.0.1:8108)
	npx playwright test
