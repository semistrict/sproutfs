set shell := ["bash", "-euo", "pipefail", "-c"]

default: check

generate:
    buf lint
    buf generate

fmt:
    go fmt ./...
    buf format -w

test:
    go test ./...

test-race:
    go test -race ./...

# Everything a push has to pass; .github/workflows/check.yml runs these recipes.
check: determinism check-go check-zircon-core check-guards check-proto check-shell test-shell check-rust check-spec

# test-knobs runs the campaigns with a seed's own tunables rather than the
# deployment's: part sizes, pager budgets, intervals and holds drawn per seed,
# as FoundationDB draws its knobs under buggify.
test-knobs:
    SPROUTFS_TEST_KNOBS=1 go test ./internal/simtest -count=1 -timeout=30m

test-soak-knobs:
    SPROUTFS_TEST_SOAK=1 SPROUTFS_TEST_KNOBS=1 go test ./internal/simtest -run 'Soak$' -count=1 -timeout=60m

# determinism is the no-cheating rule: nothing that decides what becomes
# durable may read the wall clock or an unseeded random source behind the
# injected platform.Clock and platform.Entropy.
determinism:
    go test ./internal/testdeterminism -count=1

# gofmt, build and vet for both operating systems, and the whole Go suite.
check-go:
    out=$(git ls-files '*.go' ':!third_party' | xargs gofmt -l); if [ -n "$out" ]; then printf 'gofmt -w these files:\n%s\n' "$out" >&2; exit 1; fi
    go build ./...
    go vet ./...
    GOOS=linux go build ./...
    GOOS=linux go vet ./...
    GOOS=darwin go build ./...
    GOOS=darwin go vet ./...
    go test ./...
    SPROUTFS_ARENA=shared go test ./vmmemory/... ./host/... ./vmmigrate/... ./internal/simtest/... ./vmmachine/...

# check-zircon-core runs the tests scripts/pager-core-zircon.json lists under
# the zircon pager core, in both arena modes, while that core is ported beside
# the current one (plans/zircon-pager-port-2026-10-05.md). A listed test that
# fails or does not exist fails it.
check-zircon-core:
    python3 scripts/test-pager-core.py

# check-guards runs every in-tree bug guard in scripts/mutation/guards.json
# against the tests it names and fails if those tests pass with it on: a guard
# its tests no longer kill protects nothing. About fifteen seconds once the test
# binaries are built. See "Negative tests in the tree" in docs/testing.md.
check-guards:
    python3 scripts/check-guards.py

check-proto:
    buf lint

# check-spec model-checks the TLA+ specs under spec/ with TLC, and checks that
# each spec still catches the defects its mutants put back. It needs Java;
# scripts/tlc.sh fetches the pinned TLA+ tools.
check-spec:
    scripts/check-spec.sh

# check-spec-deep runs the larger configurations, minutes each.
check-spec-deep:
    scripts/check-spec.sh deep

check-shell:
    shellcheck scripts/*.sh scripts/lib/*.sh scripts/test/*.sh

# The demo flows, run against a model of the deployment rather than a cluster:
# what a flow does with what the CLI tells it, which is the half of a demo run
# that needs no hardware and is where a flow's own bookkeeping goes wrong.
test-shell:
    for suite in scripts/test/*-test.sh; do echo "== $suite"; bash "$suite"; done

# The managed-memory crate; off Linux its unit tests compile and run nothing.
check-rust:
    cargo fmt --manifest-path rust/sproutfs-vm-memory/Cargo.toml --all --check
    cargo clippy --locked --manifest-path rust/sproutfs-vm-memory/Cargo.toml --all-targets -- -D warnings
    cargo test --locked --manifest-path rust/sproutfs-vm-memory/Cargo.toml --lib

# One block of the seed sweep: seeds [base, base+count). See docs/testing.md.
soak base="1" count="100":
    SPROUTFS_TEST_SOAK=1 SPROUTFS_SOAK_SEED_BASE={{ base }} SPROUTFS_SOAK_SEED_COUNT={{ count }} \
        go test -count=1 -timeout=80m -v ./internal/simtest -run 'Soak$'

# The interactive explainer's simulation (TASK-65): the real code's simulation
# test binary compiled to WebAssembly, beside the Go runtime glue that loads it.
explainer:
    GOOS=js GOARCH=wasm go test -c -o explainer/simtest.wasm ./internal/simtest
    cp "$(go env GOROOT)/lib/wasm/wasm_exec.js" explainer/wasm_exec.js

# One block under the race detector, for a seed a sweep has already flagged.
soak-race base="1" count="4":
    SPROUTFS_TEST_SOAK=1 SPROUTFS_SOAK_SEED_BASE={{ base }} SPROUTFS_SOAK_SEED_COUNT={{ count }} \
        go test -race -count=1 -timeout=180m -v ./internal/simtest -run 'Soak$'

all: generate fmt check
