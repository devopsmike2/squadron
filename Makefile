.PHONY: all ui build build-backend build-enterprise build-cli build-cli-all-platforms fleetsim run docker test clean deps docker-build docker-run docker-run-single docker-dev docker-stop docker-clean test-env-up test-env-down test-env-logs test-env-reset test-env-fleetsim webhook-echo demo-seed build-audit-verify

# Variables
BINARY_NAME=squadron
CLI_NAME=squadronctl
BUILD_DIR=bin
UI_DIR=ui
DATA_DIR=data
# VERSION is stamped into the CLI via -ldflags. Override on the
# command line for release builds: `make build-cli VERSION=v0.9.0`.
VERSION?=dev

all: ui build

# Install dependencies
deps:
	go mod download
	cd $(UI_DIR) && npm install

# Build UI
ui:
	cd $(UI_DIR) && npm install && npm run build

# Build Go binary
build: ui
	@echo "Building $(BINARY_NAME)..."
	@mkdir -p $(BUILD_DIR)
	go build -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/all-in-one

# Build Go binary without UI (for testing)
build-backend:
	@echo "Building $(BINARY_NAME) (backend only)..."
	@mkdir -p $(BUILD_DIR)
	go build -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/all-in-one

# Build the ENTERPRISE edition (backend). The enterprise edition is the
# umbrella that includes the Compliance Pack (enforced group policy,
# change windows, SIEM export, per-call access audit) and the
# commercial-tier serverless regression detectors. It is assembled by
# dropping the private squadron-enterprise repo's wire files into the
# build tree, then compiling with the edition build tags.
#
# From THIS (open-core) repo, `make build-enterprise` compiles the tags
# against the open-core STUB wire files (cmd/all-in-one/wire_compliance.go,
# wire_detectors_enterprise.go), which panic-with-guidance at startup —
# proving the seam wiring is present but the private code is not. The real
# enterprise build runs this target from a tree with the private wire files
# installed. See docs/build.md for the full edition build model.
build-enterprise:
	@echo "Building $(BINARY_NAME) (enterprise edition, backend)..."
	@mkdir -p $(BUILD_DIR)
	go build -tags "enterprise compliance" -o $(BUILD_DIR)/$(BINARY_NAME)-enterprise ./cmd/all-in-one
	@echo "NOTE: without the private squadron-enterprise wire files installed, this binary"
	@echo "      panics-with-guidance at startup. See docs/build.md."

# Demo seed. Drops a realistic engineer copilot scenario into the
# application store: demo group, baseline config, synthetic agent,
# and a +312% cost spike. Within 30 seconds the AI proposer drafts
# a rollout that appears in /rollouts as pending_approval. See
# docs/demo.md.
demo-seed:
	@echo "Seeding Squadron demo scenario..."
	@mkdir -p $(BUILD_DIR)
	go build -o $(BUILD_DIR)/squadron-demo-seed ./cmd/squadron-demo-seed
	$(BUILD_DIR)/squadron-demo-seed --db ./$(DATA_DIR)/squadron.db

# Build for Linux (for Docker)
build-linux:
	@echo "Building $(BINARY_NAME) for Linux..."
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=amd64 go build -o $(BUILD_DIR)/$(BINARY_NAME)-linux ./cmd/all-in-one

# Build fleetsim — synthetic OpAMP load generator for scale testing.
# See docs/scale-testing.md for usage.
fleetsim:
	@echo "Building fleetsim..."
	CGO_ENABLED=0 go build -o $(BUILD_DIR)/fleetsim ./cmd/fleetsim
	@echo "Run: ./$(BUILD_DIR)/fleetsim --count=100"

# Build otlpsim — synthetic OTLP/HTTP load generator for the ingest
# path (receiver → worker pool → DuckDB). Sibling of fleetsim; see
# docs/scale-testing.md for usage.
otlpsim:
	@echo "Building otlpsim..."
	CGO_ENABLED=0 go build -o $(BUILD_DIR)/otlpsim ./cmd/otlpsim
	@echo "Run: ./$(BUILD_DIR)/otlpsim --rate=200 --duration=60s"

# Build squadronctl for the host platform. No CGO needed — the CLI
# does not link SQLite/DuckDB so it cross-compiles trivially.
build-cli:
	@echo "Building $(CLI_NAME) ($(VERSION))..."
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 go build \
		-ldflags "-s -w -X github.com/devopsmike2/squadron/cmd/squadronctl/commands.Version=$(VERSION)" \
		-o $(BUILD_DIR)/$(CLI_NAME) ./cmd/squadronctl

# Build squadronctl for every platform we ship. Run from the release
# workflow on tag pushes; output binaries go in $(BUILD_DIR) and are
# attached to the GitHub release.
build-cli-all-platforms:
	@mkdir -p $(BUILD_DIR)
	@for target in \
		darwin/amd64 \
		darwin/arm64 \
		linux/amd64 \
		linux/arm64 \
		windows/amd64 \
	; do \
		GOOS=$$(echo $$target | cut -d/ -f1); \
		GOARCH=$$(echo $$target | cut -d/ -f2); \
		EXT=""; \
		if [ "$$GOOS" = "windows" ]; then EXT=".exe"; fi; \
		OUT=$(BUILD_DIR)/$(CLI_NAME)-$$GOOS-$$GOARCH$$EXT; \
		echo "→ $$OUT"; \
		CGO_ENABLED=0 GOOS=$$GOOS GOARCH=$$GOARCH go build \
			-ldflags "-s -w -X github.com/devopsmike2/squadron/cmd/squadronctl/commands.Version=$(VERSION)" \
			-o $$OUT ./cmd/squadronctl; \
	done

# Run locally
run: build
	@mkdir -p $(DATA_DIR)
	./$(BUILD_DIR)/$(BINARY_NAME)

# Run with config
run-config: build
	@mkdir -p $(DATA_DIR)
	./$(BUILD_DIR)/$(BINARY_NAME) --config squadron.yaml

# Build Docker image (legacy)
docker:
	docker build -t squadron/all-in-one:latest .

# Run Docker container (legacy - use docker-run for compose)
docker-run-single:
	docker run -p 8080:8080 -p 4317:4317 -p 4318:4318 \
		-v $(PWD)/$(DATA_DIR):/data \
		squadron/all-in-one:latest

# Run tests
test:
	go test -v ./...

# Run tests with coverage
test-coverage:
	go test -v -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html

# Format code
fmt:
	go fmt ./...

# Lint code
lint:
	golangci-lint run

# Clean build artifacts
clean:
	rm -rf $(BUILD_DIR)
	rm -rf $(UI_DIR)/dist
	rm -rf $(DATA_DIR)
	rm -f coverage.out coverage.html

# Development mode (watch and reload)
dev:
	air

# Install development tools
install-tools:
	go install github.com/air-verse/air@latest
	go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest

# =============================================================================
# Docker Commands
# =============================================================================

# Build Docker image
docker-build:
	docker build -t squadron:latest .

# Run with Docker Compose (default = published image, UI+API on :8080)
docker-run:
	docker compose up -d

# Run the hot-reload development stack (builds from source; UI on :5173)
docker-dev:
	docker compose -f docker-compose.dev.yml up

# Stop all containers
docker-stop:
	docker compose down

# Clean up Docker resources
docker-clean:
	docker compose down -v
	docker system prune -f
	docker volume prune -f

# Build and run in one command
docker-quick:
	docker compose up -d --build

# View logs
docker-logs:
	docker compose logs -f

# View logs for backend only
docker-logs-backend:
	docker compose logs -f squadron

# View logs for UI only
docker-logs-ui:
	docker compose -f docker-compose.dev.yml logs -f ui

# Shell into backend container
docker-shell:
	docker compose exec squadron sh

# Shell into UI container
docker-shell-ui:
	docker compose -f docker-compose.dev.yml exec ui sh

# ===================================================================
# v0.37 local test environment
# ===================================================================
#
# Spins up a realistic Squadron-managed fleet on your laptop:
#   * 2 OpAMP-enabled OTel collectors (one prod, one staging)
#   * 1 OTLP-only collector (no OpAMP — for v0.36.0 discovery)
#   * 1 webhook-echo server (for v0.33 + v0.35 webhook payloads)
#
# Squadron itself runs as the local binary (NOT containerized) so
# you can iterate code without rebuilding. The collectors talk to
# your host via host.docker.internal.
#
# See docs/testing.md for the full walkthrough including the
# personal GitHub test repo recipe.

# test-env-up: build the binary if needed, start Squadron in the
# background, then bring up the docker-compose fleet.
test-env-up: build-backend webhook-echo
	@mkdir -p $(DATA_DIR)
	@if [ -z "$$SQUADRON_DEPLOY_KEY" ]; then \
		export SQUADRON_DEPLOY_KEY=$$(head -c 32 /dev/urandom | base64); \
		echo "Generated SQUADRON_DEPLOY_KEY for this session."; \
		echo "To persist it: export SQUADRON_DEPLOY_KEY=$$SQUADRON_DEPLOY_KEY"; \
	fi
	@echo "Starting test fleet via docker compose..."
	@cd deploy/test && docker compose up -d --build
	@echo ""
	@echo "Test environment ready."
	@echo "  Squadron UI:    http://localhost:8090"
	@echo "  Webhook echo:   http://localhost:9001  (POST anything, see container logs)"
	@echo ""
	@echo "Next steps:"
	@echo "  make test-env-fleetsim   # add 50 synthetic OpAMP agents"
	@echo "  make test-env-logs       # tail one collector's logs"
	@echo "  make test-env-down       # stop the fleet"

# test-env-down: stop the docker-compose fleet but keep Squadron's
# data dir intact for the next start.
test-env-down:
	@cd deploy/test && docker compose down
	@echo "Test fleet stopped. Squadron's data dir is preserved."
	@echo "Run 'make test-env-reset' to wipe state for a fresh start."

# test-env-logs: tail all collector + webhook logs. Useful for
# watching Squadron config pushes land in real time.
test-env-logs:
	@cd deploy/test && docker compose logs -f

# test-env-reset: full reset — stops the fleet AND wipes Squadron's
# local data dir. The next test-env-up starts from a blank slate.
test-env-reset:
	@cd deploy/test && docker compose down -v
	@rm -rf $(DATA_DIR)
	@echo "Test fleet stopped and Squadron data wiped."

# test-env-fleetsim: add 50 synthetic OpAMP agents on top of the
# real docker collectors. Lets you stress-test scrolling /
# filtering / rollout selection on the agents page.
test-env-fleetsim: fleetsim
	@echo "Connecting 50 synthetic agents to local Squadron..."
	@./$(BUILD_DIR)/fleetsim --count=50 --target=ws://localhost:4330/v1/opamp --ramp=10s

# webhook-echo: build the tiny webhook receiver helper. Standalone
# binary so you can run it without docker if you prefer.
webhook-echo:
	@echo "Building webhook-echo..."
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 go build -o $(BUILD_DIR)/webhook-echo ./cmd/webhook-echo

# build-audit-verify: the standalone OFFLINE attestation verifier (ADR 0027).
# Takes a chain-column audit export (?include_chain=1) + an attestation JSON and
# recomputes the hash-chain offline to confirm the tip matches — zero secrets.
# CGO is left at its default (enabled) like the demo-seed target: importing the
# credstore package for the optional sealed-signature check transitively pulls
# in the CGO go-sqlite3 driver, so a CGO_ENABLED=0 build cannot link.
build-audit-verify:
	@echo "Building squadron-audit-verify..."
	@mkdir -p $(BUILD_DIR)
	go build -o $(BUILD_DIR)/squadron-audit-verify ./cmd/squadron-audit-verify
