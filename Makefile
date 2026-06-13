.PHONY: fmt lint vet test race bench build loadtest loadtest-quick clean

# Override on the command line, e.g. `make loadtest TPS=200000 DURATION=1m MARKET=1`.
TPS ?= 100000
DURATION ?= 2m
USERS ?= 100
MARKET ?= 0
LEVELS ?= 15

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

# Load test with live order-book TUI (defaults: 100k TPS, 2m, 100 users).
loadtest:
	go run ./cmd/loadtest -tps $(TPS) -duration $(DURATION) -users $(USERS) -market $(MARKET) -levels $(LEVELS)

# Short load test for a quick check (10s).
loadtest-quick:
	go run ./cmd/loadtest -tps $(TPS) -duration 10s -users $(USERS) -market $(MARKET) -levels $(LEVELS)

clean:
	rm -rf bin
