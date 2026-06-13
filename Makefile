.PHONY: fmt lint vet test race bench build property differential fuzz loadtest loadtest-quick shardbench clean

# Override on the command line, e.g. `make loadtest TPS=200000 DURATION=1m MARKET=1`.
TPS ?= 1000000
DURATION ?= 2m
USERS ?= 100
MARKET ?= 0
LEVELS ?= 15
# Core assignment for shardbench: ';' separates cores, ',' shares markets on a core.
# Default: BTC isolated on core 0, ETH+SOL sharing core 1.
CORES ?= 0;1,2
# Native-fuzz duration for `make fuzz`. Override, e.g. `make fuzz FUZZTIME=5m`.
FUZZTIME ?= 30s

# Format check: fail if any file is not gofmt-clean.
fmt:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed on:"; echo "$$unformatted"; exit 1; \
	fi

vet:
	go vet ./...

# Pre-commit linter: formatting + vet. (golangci-lint added later if installed.)
lint: fmt vet

test:
	go test ./...

race:
	go test -race ./...

bench:
	go test -bench=. -benchmem ./...

build:
	go build -trimpath -o bin/engine ./cmd/engine

# Property suite: reference-model differential, invariants, determinism,
# recovery, adversarial corpus, and the rapid state machine.
property:
	go test ./tests/property/ ./tests/refmodel/

# Just the engine-vs-reference-model differential checks (verbose).
differential:
	go test ./tests/property/ -run Differential -v

# Coverage-guided native fuzz of the differential loop. Override duration with
# FUZZTIME, e.g. `make fuzz FUZZTIME=5m`.
fuzz:
	go test ./tests/property/ -run '^$$' -fuzz '^FuzzEngine$$' -fuzztime=$(FUZZTIME)

# Load test with live order-book TUI (defaults: 100k TPS, 2m, 100 users).
loadtest:
	go run ./cmd/loadtest -tps $(TPS) -duration $(DURATION) -users $(USERS) -market $(MARKET) -levels $(LEVELS)

# Short load test for a quick check (10s).
loadtest-quick:
	go run ./cmd/loadtest -tps $(TPS) -duration 10s -users $(USERS) -market $(MARKET) -levels $(LEVELS)

# Parallel shard-matching throughput by core assignment.
# e.g. `make shardbench CORES="0;1;2"` (each market isolated) or `CORES="0;1,2"`.
shardbench:
	go run ./cmd/shardbench -cores "$(CORES)" -duration 10s -users $(USERS)

clean:
	rm -rf bin
