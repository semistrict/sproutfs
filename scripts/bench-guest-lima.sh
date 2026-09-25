#!/usr/bin/env bash
# Real guest workloads on managed storage, against a plain-Firecracker baseline.
#
# Builds the feature-enabled VMM exactly as the qualification suite does, builds
# (or reuses) the Alpine workload image, runs the benchmark inside the Lima
# instance and copies the JSON records the documentation cites into
# docs/measurements/guest-workload-<date>.json, or SPROUTFS_BENCH_RECORDS.
# SPROUTFS_BENCH_SCENARIOS selects scenarios, and SPROUTFS_BENCH_RAM_BYTES,
# SPROUTFS_BENCH_ROOT_BYTES, SPROUTFS_BENCH_RAM_RESIDENT_BYTES and
# SPROUTFS_BENCH_PMEM_RESIDENT_BYTES the guest's shape and each pager's arena;
# see TestGuestWorkloadBenchmark. The run uses a host's two pagers, so the
# instance needs a HugeTLB pool holding the PMEM resident budget — the RAM
# pager's 4 KiB pages are ordinary memory.
set -euo pipefail
repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
instance=${SPROUTFS_LIMA_INSTANCE:-default}
toolchain=${SPROUTFS_RUST_TOOLCHAIN:-stable}
timeout=${SPROUTFS_BENCH_TIMEOUT:-4h}
if (($# != 0)); then echo "Usage: $0" >&2; exit 2; fi
arch=$(limactl shell "$instance" uname -m)
if [[ "$arch" != aarch64 ]]; then
    echo 'The workload benchmark pins an aarch64 kernel and guest image.' >&2
    exit 1
fi
# shellcheck source=scripts/lib/firecracker-build.sh
source "$repo/scripts/lib/firecracker-build.sh"
limactl shell "$instance" sudo -n test -r /dev/kvm

image=$(bash "$repo/scripts/build-guest-image-lima.sh")
test -n "$image"

mkdir -p "$HOME/.cache"
host_work=$(mktemp -d "$HOME/.cache/sproutfs-bench.XXXXXX")
guest_work=''
# shellcheck disable=SC2317,SC2329  # invoked by the EXIT trap below; older shellcheck reports it as SC2317.
cleanup() {
    if [[ -n "$guest_work" ]]; then
        limactl shell "$instance" sudo -n rm -rf -- "$guest_work"
        limactl shell "$instance" test ! -e "$guest_work"
    fi
    rm -rf -- "$host_work"
}
trap cleanup EXIT
guest_work=$(limactl shell "$instance" mktemp -d /tmp/sproutfs-bench.XXXXXX)
sproutfs_firecracker_build "$instance" "$repo" "$guest_work" "$toolchain"
limactl shell "$instance" mkdir -p "$guest_work/run" "$guest_work/objects"

cd "$repo"
revision=$(git rev-parse HEAD)
if ! git diff --quiet || ! git diff --cached --quiet; then revision="$revision-dirty"; fi
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go test -c ./vmmachine -o "$host_work/bench.test"
# The whole run stays under the work directory: replica journals, the pager's
# scratch spill, every VM's private sockets, and the checkpoint objects. The
# object store keeps the same latency model on disk because the instance cannot
# hold a multi-gigabyte workload's checkpoints in the test process's heap.
limactl shell "$instance" sudo -n env \
    SPROUTFS_FIRECRACKER_BENCH=1 \
    SPROUTFS_BENCH_REVISION="$revision" \
    SPROUTFS_FIRECRACKER="$guest_work/firecracker" \
    SPROUTFS_FIRECRACKER_SECCOMP="$guest_work/seccomp.bpf" \
    SPROUTFS_FIRECRACKER_KERNEL="$guest_work/kernel" \
    SPROUTFS_BENCH_IMAGE="$image" \
    SPROUTFS_BENCH_WORK="$guest_work/run" \
    SPROUTFS_BENCH_OBJECT_DIR="${SPROUTFS_BENCH_OBJECT_DIR-$guest_work/objects}" \
    SPROUTFS_BENCH_OUTPUT="$guest_work/records.json" \
    SPROUTFS_BENCH_PROFILE="${SPROUTFS_BENCH_PROFILE_SCENARIO:+$guest_work/cpu.pprof}" \
    SPROUTFS_BENCH_PROFILE_SCENARIO="${SPROUTFS_BENCH_PROFILE_SCENARIO:-}" \
    SPROUTFS_BENCH_SCENARIOS="${SPROUTFS_BENCH_SCENARIOS:-}" \
    SPROUTFS_BENCH_FORKS="${SPROUTFS_BENCH_FORKS:-}" \
    SPROUTFS_BENCH_RAM_BYTES="${SPROUTFS_BENCH_RAM_BYTES:-}" \
    SPROUTFS_BENCH_ROOT_BYTES="${SPROUTFS_BENCH_ROOT_BYTES:-}" \
    SPROUTFS_BENCH_RAM_RESIDENT_BYTES="${SPROUTFS_BENCH_RAM_RESIDENT_BYTES:-}" \
    SPROUTFS_BENCH_PMEM_RESIDENT_BYTES="${SPROUTFS_BENCH_PMEM_RESIDENT_BYTES:-}" \
    SPROUTFS_BENCH_BUILD="${SPROUTFS_BENCH_BUILD:-}" \
    SPROUTFS_BENCH_TEST="${SPROUTFS_BENCH_TEST:-}" \
    SPROUTFS_BENCH_STEADY="${SPROUTFS_BENCH_STEADY:-}" \
    SPROUTFS_GCS_BUCKET="${SPROUTFS_GCS_BUCKET:-}" \
    SPROUTFS_GCS_ENDPOINT="${SPROUTFS_GCS_ENDPOINT:-}" \
    SPROUTFS_GCS_PREFIX="${SPROUTFS_GCS_PREFIX:-}" \
    GOOGLE_APPLICATION_CREDENTIALS="${GOOGLE_APPLICATION_CREDENTIALS:-}" \
    "$host_work/bench.test" -test.v -test.run TestGuestWorkloadBenchmark -test.timeout="$timeout" \
    || status=$?
mkdir -p "$repo/docs/measurements"
records=${SPROUTFS_BENCH_RECORDS:-$repo/docs/measurements/guest-workload-$(date -u +%Y-%m-%d).json}
limactl shell "$instance" sudo -n cat "$guest_work/records.json" > "$records"
# The CPU profile is diagnostic rather than evidence, so it goes to the caller's
# cache rather than into the repository. It exists only when
# SPROUTFS_BENCH_PROFILE_SCENARIO asked for one, which a recorded run does not:
# profiling interrupts every thread and inflates a fault-bound scenario several
# times over.
profile=${SPROUTFS_BENCH_PROFILE_OUT:-$HOME/.cache/sproutfs-bench-$(date -u +%Y-%m-%d).pprof}
if [[ -n "${SPROUTFS_BENCH_PROFILE_SCENARIO:-}" ]] && limactl shell "$instance" sudo -n test -s "$guest_work/cpu.pprof"; then
    limactl shell "$instance" sudo -n cat "$guest_work/cpu.pprof" > "$profile"
    echo "CPU profile written to $profile" >&2
fi
# The image manifest records the pinned inputs every number was measured over.
limactl shell "$instance" cat "$(dirname "$image")/manifest.json" \
    > "$repo/docs/measurements/guest-image-$(date -u +%Y-%m-%d).json"
echo "records written to $records" >&2
exit "${status:-0}"
