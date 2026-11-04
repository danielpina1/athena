.PHONY: check proto build bench bench-baseline

# Regenerate the Go code of the wire contract (proto/) into internal/pb; the generated code is committed.
proto:
	go tool buf generate

# check lints the protos and fails when the committed generated code is not what the protos generate.
check:
	go tool buf lint
	@before=$$(find internal/pb -type f | sort | xargs shasum); \
	go tool buf generate; \
	after=$$(find internal/pb -type f | sort | xargs shasum); \
	[ "$$before" = "$$after" ] || { echo "internal/pb is stale: run make proto and commit"; exit 1; }
	go vet ./...
	go test -race ./...
	CGO_ENABLED=0 go build ./...

build:
	CGO_ENABLED=0 go build -o bin/athena ./cmd/athena

# Measure the performance budgets (spec 10.3) against a fake runtime, write bench/results.json and fail on a budget
# breach or a regression of more than 10% against bench/baseline.json.
bench:
	ATHENA_BENCH=1 go test -count=1 -v -run TestBudgets -bench . ./bench/...

# Record the current numbers as the baseline (commit bench/baseline.json); do it only when a slower result is intended.
bench-baseline:
	ATHENA_BENCH=1 ATHENA_BENCH_UPDATE=1 go test -count=1 -v -run TestBudgets ./bench/...
