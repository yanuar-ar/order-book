.PHONY: fmt lint vet test race bench build property differential fuzz throughput throughput-sync loadtest loadtest-sync loadtest-quick clean

# Override on the command line, e.g. `make loadtest TPS=200000 DURATION=1m MARKET=1`.
TPS ?= 500000
DURATION ?= 1m
USERS ?= 100
MARKET ?= 0
LEVELS ?= 15
# Engine topology for throughput/loadtest: serial (default) or parallel.
TOPOLOGY ?= serial
# Parallel market->worker map: ';' separates workers, ',' shares markets on one.
# Default: BTC isolated on worker 0, ETH+SOL sharing worker 1.
CORES ?= 0;1,2
# Group-commit batch ceiling (commands per fsync) for the durable bench targets.
FLUSHCAP ?= 8192
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

# "How fast can it go": full engine at max rate with offloaded generation.
# Both journal durably to a real WAL; `throughput` is **async** (off-thread fsync,
# the 1M durable path — the default), `throughput-sync` uses the inline journaller.
# Serial (default) or parallel; for parallel, CORES maps markets->workers.
# e.g. `make throughput FLUSHCAP=16384`, or `make throughput-sync` to compare.
throughput:
	go run ./cmd/throughput -topology $(TOPOLOGY) -cores "$(CORES)" -duration $(DURATION) -users $(USERS) -journal async -flushcap $(FLUSHCAP)

throughput-sync:
	go run ./cmd/throughput -topology $(TOPOLOGY) -cores "$(CORES)" -duration $(DURATION) -users $(USERS) -journal sync -flushcap $(FLUSHCAP)

# "How does it behave at load X": open-loop paced load with a live order-book TUI
# and two-SLO latency (internal match + durable-ack). `loadtest` is **async**
# (default), `loadtest-sync` journals inline. e.g. `make loadtest TPS=200000`.
loadtest:
	go run ./cmd/loadtest -tps $(TPS) -duration $(DURATION) -users $(USERS) -market $(MARKET) -levels $(LEVELS) -topology $(TOPOLOGY) -cores "$(CORES)" -journal async -flushcap $(FLUSHCAP)

loadtest-sync:
	go run ./cmd/loadtest -tps $(TPS) -duration $(DURATION) -users $(USERS) -market $(MARKET) -levels $(LEVELS) -topology $(TOPOLOGY) -cores "$(CORES)" -journal sync -flushcap $(FLUSHCAP)

# Short load test for a quick check (10s, async durable).
loadtest-quick:
	go run ./cmd/loadtest -tps $(TPS) -duration 10s -users $(USERS) -market $(MARKET) -levels $(LEVELS) -topology $(TOPOLOGY) -cores "$(CORES)" -journal async -flushcap $(FLUSHCAP)

clean:
	rm -rf bin
