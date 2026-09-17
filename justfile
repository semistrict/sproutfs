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
check: determinism check-go check-proto check-shell test-shell check-rust

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
    out=$(gofmt -l cmd internal); if [ -n "$out" ]; then printf 'gofmt -w these files:\n%s\n' "$out" >&2; exit 1; fi
    go build ./...
    go vet ./...
    GOOS=linux go build ./...
    GOOS=linux go vet ./...
    GOOS=darwin go build ./...
    GOOS=darwin go vet ./...
    go test ./...

check-proto:
    buf lint

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

# One block under the race detector, for a seed a sweep has already flagged.
soak-race base="1" count="4":
    SPROUTFS_TEST_SOAK=1 SPROUTFS_SOAK_SEED_BASE={{ base }} SPROUTFS_SOAK_SEED_COUNT={{ count }} \
        go test -race -count=1 -timeout=180m -v ./internal/simtest -run 'Soak$'

all: generate fmt check
