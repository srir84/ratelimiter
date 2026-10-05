# Rate limiter and harness commands (DESIGN §C8; IMPLEMENTATION_PLAN S0 step 7).

# Every recipe line runs in bash with -eu -o pipefail, so a failure anywhere in a
# line (a pipeline, a ; list, a loop) fails the target. macOS's /usr/bin/make is
# GNU Make 3.81, which ignores .SHELLFLAGS, so SHELL carries the same flags.
SHELL := /bin/bash -eu -o pipefail
.SHELLFLAGS := -eu -o pipefail -c

# Recipes call tools only through these variables, so a test can swap in stubs.
GO ?= go
GOLANGCI ?= bin/tools/golangci-lint
STYLUA ?= stylua
PYTHON ?= python3
TRACE ?= bin/trace

# Sub-makes run through SUBMAKE, never a literal $(MAKE): make runs any recipe line
# that names $(MAKE) even under -n, so `make -n slice-check` would run bin/trace procs.
# Command-line variables (the tool stubs, S) still reach sub-makes through MAKEFLAGS.
SUBMAKE = $(MAKE)

# The Python packages that exist so far; the Python tools get only these paths.
PY_PKGS := $(wildcard refmodel report)

# Always rebuilt, with $(GO), whatever TRACE names. A prerequisite of check,
# slice-check and every test-* target.
.PHONY: bin/trace
bin/trace:
	$(GO) build -o bin/trace ./tools/trace

.PHONY: setup
setup:
	bash tools/setup.sh

# golangci-lint runs once per build (untagged, then each tag), since each build sees
# other files; the runs cover go vet's analyzers, gofmt and goimports.
.PHONY: check
check: bin/trace
	$(GOLANGCI) config verify
	$(GOLANGCI) run ./...
	$(GOLANGCI) run --build-tags rltest ./...
	$(GOLANGCI) run --build-tags conformance ./...
	$(GOLANGCI) run --build-tags scripttest ./...
	$(STYLUA) --check .
ifneq ($(PY_PKGS),)
	$(PYTHON) -m ruff check --ignore-noqa $(PY_PKGS)
	$(PYTHON) -m ruff format --check $(PY_PKGS)
	$(PYTHON) -m mypy $(PY_PKGS)
endif
	$(TRACE) check

.PHONY: fmt
fmt:
	$(GOLANGCI) fmt
	$(STYLUA) .
ifneq ($(PY_PKGS),)
	$(PYTHON) -m ruff format $(PY_PKGS)
endif

# A second pass at GOMAXPROCS 1 and 4 over the packages without timing windows (DESIGN
# §C1; IMPLEMENTATION_PLAN S2 step 11), in two runs because -run applies to every package
# of one go test. internal/store stays out: its tests have timing windows.
.PHONY: test-unit
test-unit: bin/trace
	$(TRACE) gotest -o .out/test-unit.json -- -race -count=1 ./...
	$(TRACE) gotest -o .out/test-unit-cpu.json -- -race -count=1 -cpu 1,4 ./internal/settings/... ./internal/keys/... ./internal/fingerprint/... ./internal/receipt/... ./internal/metrics/...
	$(TRACE) gotest -o .out/test-unit-cpu-core.json -- -race -count=1 -cpu 1,4 -run 'TestPermitTransitionPairsRace$$' ./internal/core/

# L0 store conformance (DESIGN §C2; IMPLEMENTATION_PLAN S1 step 8): the whole suite, one
# package at a time, without -race, which distorts the timings. Only this unfiltered run
# rewrites .data/conformance.json.
.PHONY: test-store
test-store: bin/trace
	$(TRACE) gotest -o .out/test-store.json -- -tags conformance -count=1 -p 1 -timeout 45m ./internal/testutil/...

# The functional assertions only, without the timing tests; slice-check runs it from S4 on.
.PHONY: test-store-quick
test-store-quick: bin/trace
	$(TRACE) gotest -o .out/test-store-quick.json -- -tags conformance -count=1 -p 1 -timeout 10m \
		-skip 'TestC2_Row09_Throughput|TestC2_Row13_Curve|TestC2_Row14_NoWrites|TestC2_FsyncFloor|TestC2_ScanUnlink200k' \
		./internal/testutil/...

# The pinned store's durability source lines (DESIGN §A3.0, §C8). Needs the network, so
# it is not part of slice-check; run it when the pinned version changes.
.PHONY: verify-store-source
verify-store-source:
	bash tools/verify-store-source.sh

# bin/bench: hitapi and rladmin built without tags, the system under test and the
# product tool; bin/test: every binary built with -tags rltest, where the harness
# tools always run from (DESIGN §B1). Only the packages that exist so far, judged by
# their Go files: a cmd/<name>/ with no .go file is not a package yet.
CMDS := $(wildcard cmd/*/*.go)
BENCH_CMDS := $(strip $(foreach c,hitapi rladmin,$(if $(wildcard cmd/$(c)/*.go),./cmd/$(c))))

.PHONY: build
build:
ifeq ($(CMDS),)
	@echo "no binaries yet"
else
ifneq ($(BENCH_CMDS),)
	$(GO) build -o bin/bench/ $(BENCH_CMDS)
endif
	$(GO) build -tags rltest -o bin/test/ ./cmd/...
endif

# make slice-check S=<id>: leftover children of an earlier run (before .out/ goes), a
# fresh .out/, check, each test target of the slices up to S followed by procs, then
# every ID and plan-named test up to S. Stops at the first failure. The target list
# goes into a shell variable first: bash ignores a failing $(...) in a for word list,
# even under -e.
.PHONY: slice-check
slice-check: bin/trace
	@test -n "$(S)" || { echo "slice-check: S is required, e.g. make slice-check S=S0" >&2; exit 2; }
	$(TRACE) procs
	rm -rf .out && mkdir .out
	$(SUBMAKE) check
	targets="$$($(TRACE) targets --upto $(S))"; \
	for t in $$targets; do \
		$(SUBMAKE) -n "$$t" >/dev/null; \
		$(SUBMAKE) "$$t"; \
		$(TRACE) procs; \
	done
	$(TRACE) slices --upto $(S)
