.PHONY: check proto build web web-check e2e bench bench-baseline

# The web app's packages; protoc-gen-es lives in them, so generating the TypeScript needs them first.
web/node_modules: web/package-lock.json
	npm --prefix web ci
	@touch web/node_modules

# Regenerate the wire contract (proto/) into internal/pb (Go) and web/src/gen (TypeScript); both are committed.
proto: web/node_modules
	go tool buf generate

# check lints the protos and fails when the committed generated code is not what the protos generate, then runs the Go
# checks and the web app's.
check: web/node_modules
	go tool buf lint
	@before=$$(find internal/pb web/src/gen -type f | sort | xargs shasum); \
	go tool buf generate; \
	after=$$(find internal/pb web/src/gen -type f | sort | xargs shasum); \
	[ "$$before" = "$$after" ] || { echo "internal/pb or web/src/gen is stale: run make proto and commit"; exit 1; }
	go vet ./...
	go test -race ./...
	CGO_ENABLED=0 go build ./...
	$(MAKE) web-check

# Build the dashboard into internal/web/ui/dist, which the binary embeds.
web: web/node_modules
	npm --prefix web run build
	touch internal/web/ui/dist/.gitkeep

# The web app's checks: types, lint, unit tests, the build and its bundle budget.
web-check: web/node_modules
	npm --prefix web run typecheck
	npm --prefix web run lint
	npm --prefix web run test
	$(MAKE) web
	npm --prefix web run bundle-budget

# End-to-end browser tests (Task 9 fills this in).
e2e:
	@echo "e2e: not yet"

build: web
	CGO_ENABLED=0 go build -o bin/athena ./cmd/athena

# Measure the performance budgets (spec 10.3) against a fake runtime, write bench/results.json and fail on a budget
# breach or a regression of more than 10% against bench/baseline.json.
bench:
	ATHENA_BENCH=1 go test -count=1 -v -run TestBudgets -bench . ./bench/...

# Record the current numbers as the baseline (commit bench/baseline.json); do it only when a slower result is intended.
bench-baseline:
	ATHENA_BENCH=1 ATHENA_BENCH_UPDATE=1 go test -count=1 -v -run TestBudgets ./bench/...

# Watch Athena's own development live: the roadmap and every plan being built task by task (Ctrl-C to quit).
plans:
	go run ./tools/planwatch
