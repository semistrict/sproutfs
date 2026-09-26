#!/usr/bin/env bash
# Qualify the memory client against real UFFD and KVM in an existing Lima VM.
set -euo pipefail

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
instance=${SPROUTFS_LIMA_INSTANCE:-default}
repeat=${SPROUTFS_VM_MEMORY_REPEAT:-1}
# SPROUTFS_VM_MEMORY_RUN selects the Go tests of both suites by -test.run
# pattern, and skips the crate's own checks, which the selection does not need.
# SPROUTFS_VM_MEMORY_FUZZ names a fuzz target of the vmmemory suite to run for
# SPROUTFS_VM_MEMORY_FUZZTIME instead of either suite. A failing input it finds
# is copied back into vmmemory/testdata/fuzz. SPROUTFS_VM_MEMORY_TAGS builds the
# vmmemory suite with those build tags, such as sproutfsprobe. SPROUTFS_ARENA is
# the arena mode both suites build their pagers in, shared when unset.
run=${SPROUTFS_VM_MEMORY_RUN:-}
fuzz=${SPROUTFS_VM_MEMORY_FUZZ:-}
fuzztime=${SPROUTFS_VM_MEMORY_FUZZTIME:-60s}
tags=${SPROUTFS_VM_MEMORY_TAGS:-}
if (($# != 0)); then
    echo "Usage: $0 (SPROUTFS_LIMA_INSTANCE=default, SPROUTFS_VM_MEMORY_REPEAT=1, SPROUTFS_VM_MEMORY_RUN," \
        "SPROUTFS_VM_MEMORY_FUZZ, SPROUTFS_VM_MEMORY_FUZZTIME=60s, SPROUTFS_VM_MEMORY_TAGS)" >&2
    exit 2
fi

case $(limactl shell "$instance" uname -m) in
    aarch64) arch=arm64 ;;
    x86_64) arch=amd64 ;;
    *) echo 'The memory harness supports Linux aarch64 and x86_64.' >&2; exit 1 ;;
esac

# Privileges are confined to the test process. Do not change VM-wide sysctls,
# device permissions, installed toolchains, or the running VM configuration.
limactl shell "$instance" sudo -n test -r /dev/kvm
mkdir -p "$HOME/.cache"
host_work=$(mktemp -d "$HOME/.cache/sproutfs-vm-memory.XXXXXX")
guest_work=''
cleanup() {
    if [[ -n "$guest_work" ]]; then
        limactl shell "$instance" sudo -n rm -rf -- "$guest_work"
    fi
    rm -rf -- "$host_work"
}
trap cleanup EXIT
guest_work=$(limactl shell "$instance" mktemp -d /tmp/sproutfs-vm-memory.XXXXXX)
# Cargo's target directory outlives the run, so the crate builds incrementally
# instead of from scratch every time. Override with SPROUTFS_VM_MEMORY_TARGET_DIR.
target=${SPROUTFS_VM_MEMORY_TARGET_DIR:-$(limactl shell "$instance" printenv HOME)/.cache/sproutfs-vm-memory-target}

cd "$repo"
if [[ -z "$run$fuzz" ]]; then
    cargo fmt --manifest-path rust/sproutfs-vm-memory/Cargo.toml -- --check
    limactl shell "$instance" env CARGO_TARGET_DIR="$target" \
        cargo clippy --locked --manifest-path "$repo/rust/sproutfs-vm-memory/Cargo.toml" --all-targets -- -D warnings
    limactl shell "$instance" env CARGO_TARGET_DIR="$target" \
        cargo nextest run --locked --no-tests pass --manifest-path "$repo/rust/sproutfs-vm-memory/Cargo.toml"
fi
limactl shell "$instance" env CARGO_TARGET_DIR="$target" \
    cargo build --locked --manifest-path "$repo/rust/sproutfs-vm-memory/Cargo.toml" --examples
if [[ -z "$run$fuzz" ]]; then
    limactl shell "$instance" "$target/debug/examples/pte_latency"
fi
selection=()
if [[ -n "$run" ]]; then
    selection=(-test.run="$run")
fi

# A fuzz target runs from a binary built for it, which carries the coverage
# instrumentation the fuzzer is guided by.
GOOS=linux GOARCH="$arch" CGO_ENABLED=0 go test -c -tags="$tags" ${fuzz:+-fuzz="^$fuzz\$"} ./vmmemory \
    -o "$host_work/managed.test"
if [[ -n "$fuzz" ]]; then
    # The source mount is read-only, so the fuzz target runs in a copy of the
    # package's test data, where it can write the inputs it finds.
    limactl shell "$instance" mkdir -p "$guest_work/vmmemory/testdata/fuzz" "$guest_work/fuzzcache"
    if [[ -d "vmmemory/testdata/fuzz/$fuzz" ]]; then
        limactl shell "$instance" cp -R "$repo/vmmemory/testdata/fuzz/$fuzz" "$guest_work/vmmemory/testdata/fuzz/"
    fi
    status=0
    limactl shell --workdir "$guest_work/vmmemory" "$instance" sudo -n env \
        SPROUTFS_ARENA="${SPROUTFS_ARENA:-}" \
        SPROUTFS_VM_MEMORY_CLIENT="$target/debug/examples/client" \
        "$host_work/managed.test" -test.run='^$' -test.fuzz="^$fuzz\$" -test.fuzztime="$fuzztime" \
        -test.fuzzcachedir="$guest_work/fuzzcache" -test.parallel="${SPROUTFS_VM_MEMORY_FUZZ_PARALLEL:-4}" \
        -test.fuzzminimizetime="${SPROUTFS_VM_MEMORY_FUZZMINIMIZE:-0}" -test.timeout=30m || status=$?
    # A failing fuzz run has written the input it failed on.
    if ((status != 0)) && limactl shell "$instance" test -d "$guest_work/vmmemory/testdata/fuzz/$fuzz"; then
        mkdir -p "vmmemory/testdata/fuzz/$fuzz"
        limactl shell "$instance" tar -C "$guest_work/vmmemory/testdata/fuzz/$fuzz" -c . |
            tar -x -C "vmmemory/testdata/fuzz/$fuzz"
    fi
    exit "$status"
fi

GOOS=linux GOARCH="$arch" CGO_ENABLED=0 go test -c ./internal/vmtest -o "$host_work/memory.test"
limactl shell "$instance" sudo -n env \
    SPROUTFS_ARENA="${SPROUTFS_ARENA:-}" \
    SPROUTFS_VM_MEMORY_CLIENT="$target/debug/examples/client" \
    "$host_work/memory.test" -test.v -test.count="$repeat" -test.timeout=3m ${selection[@]+"${selection[@]}"}

limactl shell "$instance" sudo -n env \
    SPROUTFS_ARENA="${SPROUTFS_ARENA:-}" \
    SPROUTFS_VM_MEMORY_CLIENT="$target/debug/examples/client" \
    SPROUTFS_PAGER_MEASURE="${SPROUTFS_PAGER_MEASURE:-}" \
    SPROUTFS_FRAGMENT_MIB="${SPROUTFS_FRAGMENT_MIB:-}" \
    "$host_work/managed.test" -test.v -test.count="$repeat" -test.timeout="${SPROUTFS_MEMORY_TEST_TIMEOUT:-3m}" \
    ${selection[@]+"${selection[@]}"}
