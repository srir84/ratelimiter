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
MUTATE ?= bin/mutate

# Sub-makes run through SUBMAKE, never a literal $(MAKE): make runs any recipe line
# that names $(MAKE) even under -n, so `make -n slice-check` would run bin/trace procs.
# Command-line variables (the tool stubs, S) still reach sub-makes through MAKEFLAGS.
SUBMAKE = $(MAKE)

# The Python packages that exist so far: pytest gets only these paths; the linters,
# formatter and type checker also get the root conftest.py (IMPLEMENTATION_PLAN S5 step 17).
PY_PKGS := $(wildcard refmodel report)
PY_PATHS := $(wildcard refmodel report conftest.py)

# Always rebuilt, with $(GO), whatever TRACE names. A prerequisite of check,
# slice-check and every test-* target.
.PHONY: bin/trace
bin/trace:
	$(GO) build -o bin/trace ./tools/trace

# Always rebuilt, with $(GO), whatever MUTATE names. A prerequisite of mutate and slice-check.
.PHONY: bin/mutate
bin/mutate:
	$(GO) build -o bin/mutate ./tools/mutate

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
ifneq ($(PY_PATHS),)
	$(PYTHON) -m ruff check --ignore-noqa $(PY_PATHS)
	$(PYTHON) -m ruff format --check $(PY_PATHS)
	$(PYTHON) -m mypy $(PY_PATHS)
endif
	$(TRACE) check

.PHONY: fmt
fmt:
	$(GOLANGCI) fmt
	$(STYLUA) .
ifneq ($(PY_PATHS),)
	$(PYTHON) -m ruff format $(PY_PATHS)
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

# L2 script tests (DESIGN §C3; IMPLEMENTATION_PLAN S4 step 12): the plain run, then the
# script discipline run, where scriptrun.Run calls every script instrumented
# (RL_SCRIPT_AUDIT=1, read only in internal/testutil/scriptrun). Every L2 test file carries
# //go:build scripttest, so make test-unit starts no store. Both runs append the replies they
# observe to a fresh .out/replies-l2.jsonl, so bin/trace replies reads only this target's
# (never an earlier run's, or a mutant's). Both also run cmd/rladmin's L2 test,
# TestStoreInfoAndRewrite (IMPLEMENTATION_PLAN S8 step 8).
.PHONY: test-scripts
test-scripts: bin/trace
	rm -f .out/replies-l2.jsonl
	$(TRACE) gotest -o .out/test-scripts.json -- -tags scripttest -count=1 -timeout 20m ./internal/scripts/... ./internal/testutil/storestate/... ./internal/testutil/scriptrun/... ./cmd/rladmin/...
	RL_SCRIPT_AUDIT=1 $(TRACE) gotest -o .out/test-scripts-audit.json -- -tags scripttest -count=1 -timeout 20m ./internal/scripts/... ./internal/testutil/storestate/... ./internal/testutil/scriptrun/... ./cmd/rladmin/...

# L3 library integration through the direct driver (DESIGN §C1, §C4; IMPLEMENTATION_PLAN S5
# step 13): every package under -tags rltest, with -race. It builds first, so the driver finds
# bin/test/rladmin (IMPLEMENTATION_PLAN S8 step 8); bin/trace stays the first prerequisite.
.PHONY: test-integration
test-integration: bin/trace build
	$(TRACE) gotest -o .out/test-integration.json -- -tags rltest -race -count=1 -timeout 30m ./limiter/... ./limitertest/... ./internal/core/... ./harness/fixtures/... ./harness/driver/... ./harness/spectest/...

# L4 end-to-end (DESIGN §C1, §C5; IMPLEMENTATION_PLAN H1 step 13): the HARNESS tests against the
# bin/test binaries, built first, and the harness PostgreSQL, one package at a time (-p 1: they
# share the hits table). A missing binary or PostgreSQL fails the test, never skips it. Later
# slices append their packages to E2E_PKGS: H3 the orchestrator's rltest tests (its untagged
# TestClockStub_Refuses runs in test-unit), H4 faultctl's (its untagged stub test runs in
# test-unit) and hitapi's rltest construction test, TestConstruct_FixturesWired (its untagged
# tests run in test-unit too).
E2E_PKGS = ./harness/dbclient/ ./harness/e2e/ ./harness/httpapi/ ./cmd/orchestrator/ ./cmd/faultctl/ ./cmd/hitapi/

.PHONY: test-e2e
test-e2e: bin/trace build
	$(TRACE) gotest -o .out/test-e2e.json -- -tags rltest -p 1 -count=1 -timeout 60m $(E2E_PKGS)

# The Python tests (DESIGN §C1; IMPLEMENTATION_PLAN S5 step 13), shuffled by the root
# conftest.py; run_evidence tests need a finished run and stay out (make test-report).
.PHONY: test-py
test-py: bin/trace
	$(PYTHON) -m pytest $(PY_PKGS) -m 'not run_evidence' --junitxml=.out/test-py.xml

# Mutation testing (DESIGN §C9; IMPLEMENTATION_PLAN S4 step 17): the curated planted bugs whose
# files exist, or, when SWEEP lists files, the operator sweep over them (make mutate
# SWEEP="internal/scripts/acquire.lua …"; survivors and untried mutants go to .out/mutants.txt).
.PHONY: mutate
mutate: bin/mutate
ifeq ($(strip $(SWEEP)),)
	$(MUTATE) --curated
else
	$(MUTATE) --sweep $(SWEEP)
endif

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

# make run SCENARIO=<file> [BASELINE=1]: one whole run, end to end (DESIGN §B1, §B5;
# IMPLEMENTATION_PLAN H3 step 9): the binaries built first, then the orchestrator from bin/test,
# in the repository root (it finds harness.toml, bin/, deploy/, runs/ and .data/ there).
# BASELINE=1 runs with the limiter off (HAR-17).
.PHONY: run
run: build
	@test -n "$(SCENARIO)" || { echo "run: SCENARIO is required, e.g. make run SCENARIO=harness/e2e/testdata/orch-test.toml" >&2; exit 2; }
	bin/test/orchestrator --scenario $(SCENARIO) $(if $(BASELINE),--baseline)

# make slice-check S=<id>: leftover children of an earlier run (before .out/ goes), a
# fresh .out/, check, each test target of the slices up to S followed by procs; then the
# reply-code closure, the curated mutants whose files exist (so a later slice that weakens
# an earlier test fails) and procs (IMPLEMENTATION_PLAN S4 step 17); then every ID and
# plan-named test up to S. Stops at the first failure. replies runs before --curated, so
# mutant runs never count for the reply closure or for slices. The target list goes into a
# shell variable first: bash ignores a failing $(...) in a for word list, even under -e.
.PHONY: slice-check
slice-check: bin/trace bin/mutate
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
	$(TRACE) replies --upto $(S)
	$(MUTATE) --curated
	$(TRACE) procs
	$(TRACE) slices --upto $(S)
