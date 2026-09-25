#!/usr/bin/env bash
# Qualify the memory client against real UFFD and KVM in an existing Lima VM.
set -euo pipefail

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
instance=${SPROUTFS_LIMA_INSTANCE:-default}
repeat=${SPROUTFS_VM_MEMORY_REPEAT:-1}
if (($# != 0)); then
    echo "Usage: $0 (SPROUTFS_LIMA_INSTANCE=default, SPROUTFS_VM_MEMORY_REPEAT=1)" >&2
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
cargo fmt --manifest-path rust/sproutfs-vm-memory/Cargo.toml -- --check
limactl shell "$instance" env CARGO_TARGET_DIR="$target" \
    cargo clippy --locked --manifest-path "$repo/rust/sproutfs-vm-memory/Cargo.toml" --all-targets -- -D warnings
limactl shell "$instance" env CARGO_TARGET_DIR="$target" \
    cargo nextest run --locked --no-tests pass --manifest-path "$repo/rust/sproutfs-vm-memory/Cargo.toml"
limactl shell "$instance" env CARGO_TARGET_DIR="$target" \
    cargo build --locked --manifest-path "$repo/rust/sproutfs-vm-memory/Cargo.toml" --examples
limactl shell "$instance" "$target/debug/examples/pte_latency"

GOOS=linux GOARCH="$arch" CGO_ENABLED=0 go test -c ./internal/vmtest -o "$host_work/memory.test"
limactl shell "$instance" sudo -n env \
    SPROUTFS_VM_MEMORY_CLIENT="$target/debug/examples/client" \
    "$host_work/memory.test" -test.v -test.count="$repeat" -test.timeout=3m

GOOS=linux GOARCH="$arch" CGO_ENABLED=0 go test -c ./vmmemory -o "$host_work/managed.test"
limactl shell "$instance" sudo -n env \
    SPROUTFS_VM_MEMORY_CLIENT="$target/debug/examples/client" \
    SPROUTFS_PAGER_MEASURE="${SPROUTFS_PAGER_MEASURE:-}" \
    SPROUTFS_FRAGMENT_MIB="${SPROUTFS_FRAGMENT_MIB:-}" \
    "$host_work/managed.test" -test.v -test.count="$repeat" -test.timeout="${SPROUTFS_MEMORY_TEST_TIMEOUT:-3m}"
