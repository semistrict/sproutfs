#!/usr/bin/env bash
# Build and measure the checked-out sources on a disposable x86-64 KVM host.
set -euo pipefail
repo=$(cd "${1:?repository required}" && pwd)
results=${2:?results directory required}
work=/var/lib/sproutfs-bench/work
mkdir -p "$work/tools" "$work/build" "$results"
test "$(uname -m)" = x86_64
test -c /dev/kvm
# The host is disposable and dedicated to qualification. Reserve the pool before
# workloads fragment physical RAM; all of it disappears with the instance.
# The workload comparison's pager holds 32 GiB resident, so its pool is 36 GiB.
hugepages=4096
if [[ ${SPROUTFS_GCE_WORKLOAD:-0} == 1 ]]; then hugepages=18432; fi
sysctl -w vm.nr_hugepages="$hugepages"
[[ $(cat /sys/kernel/mm/hugepages/hugepages-2048kB/nr_hugepages) -ge $hugepages ]]
touch /var/lib/sproutfs-bench/lease
(
    while sleep 60; do touch /var/lib/sproutfs-bench/lease; done
) &
lease_pid=$!
trap 'kill "$lease_pid" 2>/dev/null || true; wait "$lease_pid" 2>/dev/null || true' EXIT

export DEBIAN_FRONTEND=noninteractive
if [[ ! -f "$work/tools/packages-ready" ]]; then
    apt-get update -qq
    apt-get install -y -qq build-essential libseccomp-dev pkg-config musl-tools \
        busybox-static curl ca-certificates python3 e2fsprogs
    touch "$work/tools/packages-ready"
fi

go_archive="$work/tools/go.tar.gz"
if [[ ! -x "$work/tools/go/bin/go" ]]; then
    curl -fsSL https://go.dev/dl/go1.26.6.linux-amd64.tar.gz -o "$go_archive"
    echo "708effb774be8237570d0add163225abbdfaf4fca28b2611df167beba4feef89  $go_archive" | sha256sum -c -
    tar -C "$work/tools" -xzf "$go_archive"
fi
export CARGO_HOME="$work/tools/cargo" RUSTUP_HOME="$work/tools/rustup"
export RUSTUP_TOOLCHAIN=1.97.0
export PATH="$work/tools/go/bin:$CARGO_HOME/bin:$PATH"
if [[ ! -x "$CARGO_HOME/bin/cargo" ]]; then
    curl -fsSL https://sh.rustup.rs -o "$work/tools/rustup-init.sh"
    sh "$work/tools/rustup-init.sh" -y --profile minimal --default-toolchain "$RUSTUP_TOOLCHAIN" --no-modify-path
fi
export CARGO_TARGET_DIR="$work/target"
export CARGO_BUILD_JOBS=8
cd "$repo"
rustup target add x86_64-unknown-linux-musl
rustup component add rustfmt
# Ubuntu's musl headers omit Linux UAPI headers. Add only those directories,
# keeping glibc's C library headers out of the musl include search path.
mkdir -p "$work/tools/linux-headers"
ln -sfn /usr/include/linux "$work/tools/linux-headers/linux"
ln -sfn /usr/include/asm-generic "$work/tools/linux-headers/asm-generic"
ln -sfn /usr/include/x86_64-linux-gnu/asm "$work/tools/linux-headers/asm"
export CFLAGS_x86_64_unknown_linux_musl="-isystem $work/tools/linux-headers"
cargo build --locked --manifest-path third_party/firecracker/Cargo.toml \
    --target x86_64-unknown-linux-musl -p firecracker --features sproutfs-memory
cargo build --locked --manifest-path third_party/firecracker/Cargo.toml -p seccompiler --bin seccompiler-bin
cargo build --locked --manifest-path rust/sproutfs-vm-memory/Cargo.toml --example client
cp "$CARGO_TARGET_DIR/x86_64-unknown-linux-musl/debug/firecracker" "$work/build/firecracker"
"$CARGO_TARGET_DIR/debug/seccompiler-bin" --target-arch x86_64 \
    --input-file third_party/firecracker/resources/seccomp/x86_64-unknown-linux-musl.json \
    --output-file "$work/build/seccomp.bpf"
curl -fsSL https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/20260902-a6146c8bb213-0/x86_64/vmlinux-6.18.44 \
    -o "$work/build/kernel"
echo "d0fa6b694b32c9d12c5b1575180c888d9c83ff8955d21fe74debb4fe4832db22  $work/build/kernel" | sha256sum -c -

# A memory-only fixture: the same init and probe, with a static shell. It has
# the workload benchmark's 8 GiB geometry without unrelated package workloads.
root="$work/build/root"
mkdir -p "$root/bin" "$root/usr/local/bin" "$root/dev" "$root/proc" "$root/sys" "$root/mnt"
cp /usr/bin/busybox "$root/bin/busybox"
ln -sfn busybox "$root/bin/sh"
ln -sfn busybox "$root/bin/uname"
ln -sfn busybox "$root/bin/cat"
cc -static -O2 -Wall -Wextra -Werror internal/vmmachine/testdata/guest.c -o "$root/init"
cc -static -O2 -Wall -Wextra -Werror internal/vmmachine/testdata/memprobe.c -o "$root/usr/local/bin/memprobe"
truncate -s 8G "$work/build/root.ext4"
mkfs.ext4 -q -F -b 4096 -d "$root" "$work/build/root.ext4"

CGO_ENABLED=0 go test -c ./internal/vmtest -o "$work/build/vmtest.test"
CGO_ENABLED=0 go test -c ./internal/vmmemory -o "$work/build/vmmemory.test"
CGO_ENABLED=0 go test -c ./internal/vmmachine -o "$work/build/vmmachine.test"
{
    uname -a
    lscpu
    free -m
    cat /sys/kernel/mm/transparent_hugepage/enabled
    grep Huge /proc/meminfo
    cat /sys/module/kvm_intel/parameters/nested
    go version
    rustc --version
    sha256sum "$work/build/firecracker" "$work/build/kernel" "$root/init" "$root/usr/local/bin/memprobe"
} > "$results/environment.txt"

if [[ ${SPROUTFS_GCE_BUILD_ONLY:-0} == 1 ]]; then exit 0; fi

# The realistic comparison: the workload image scripts/lib/bench-image.sh builds
# — a pnpm install, a cold build of the openai/codex workspace and one crate's
# tests, that checkout as a git repository — run through
# every scenario of the guest workload benchmark, managed and on plain
# Firecracker, on this host. SPROUTFS_BENCH_SCENARIOS and SPROUTFS_BENCH_FORKS
# narrow it as they do under scripts/bench-guest-lima.sh. A guest has sixteen of
# this host's thirty-two processors, on both sides: with four, the build alone
# is a quarter of an hour a side.
if [[ ${SPROUTFS_GCE_WORKLOAD:-0} == 1 ]]; then
    key=$(cat "$repo/scripts/lib/bench-image.sh" "$repo/internal/vmmachine/testdata/guest.c" | sha256sum | cut -c1-32)
    image=$(HOME=$work bash "$repo/scripts/lib/bench-image.sh" "$repo" "$key" 2> "$results/image-build.log")
    test -s "$image"
    cp "$(dirname "$image")/manifest.json" "$results/guest-image.json"
    # A smoke pass first: everything but the build, with a command that takes
    # seconds, so that what is new in this comparison — the image, a 16 GiB
    # guest, the plain side's clones — fails in minutes rather than hours in.
    scenarios=${SPROUTFS_BENCH_SCENARIOS:-} forks=${SPROUTFS_BENCH_FORKS:-} output=workload
    if [[ ${SPROUTFS_GCE_SMOKE:-0} == 1 ]]; then
        scenarios=boot,pnpm-install,capture,restore-cold,fork-fanout,baseline forks=2 output=workload-smoke
        export SPROUTFS_BENCH_TEST='cd /opt/codex && git grep -c fn | wc -l'
    fi
    run=$(mktemp -d "$work/run-workload.XXXXXX")
    status=0
    env SPROUTFS_FIRECRACKER_BENCH=1 \
        SPROUTFS_FIRECRACKER="$work/build/firecracker" \
        SPROUTFS_FIRECRACKER_SECCOMP="$work/build/seccomp.bpf" \
        SPROUTFS_FIRECRACKER_KERNEL="$work/build/kernel" \
        SPROUTFS_BENCH_IMAGE="$image" \
        SPROUTFS_BENCH_WORK="$run/state" SPROUTFS_BENCH_OBJECT_DIR="$run/objects" \
        SPROUTFS_BENCH_OUTPUT="$results/$output.json" \
        SPROUTFS_BENCH_REVISION="$(cat "$repo/source-revision.txt")" \
        SPROUTFS_BENCH_SCENARIOS="$scenarios" \
        SPROUTFS_BENCH_FORKS="$forks" \
        SPROUTFS_BENCH_VCPUS="${SPROUTFS_BENCH_VCPUS:-16}" \
        "$work/build/vmmachine.test" -test.v -test.run '^TestGuestWorkloadBenchmark$' -test.timeout=10h \
        > "$results/$output.log" 2>&1 || status=$?
    rm -rf -- "$run"
    exit "$status"
fi

# Only the fork fan-out: forks of one published checkpoint each run one binary
# off the DAX root and write nothing, and the record lists the pages each fork
# came to own, by region. busybox is the init's shell, so the parent has run it
# before the checkpoint; memprobe is a binary no parent ever ran, so a fork is
# the first to execute its pages. It is the x86-64 half of a comparison with
# scripts/bench-guest-lima.sh on aarch64.
if [[ ${SPROUTFS_GCE_FANOUT:-0} == 1 ]]; then
    fanout() {
        local name=$1 command=$2 run
        run=$(mktemp -d "$work/run-fanout.XXXXXX")
        env SPROUTFS_FIRECRACKER_BENCH=1 \
            SPROUTFS_FIRECRACKER="$work/build/firecracker" \
            SPROUTFS_FIRECRACKER_SECCOMP="$work/build/seccomp.bpf" \
            SPROUTFS_FIRECRACKER_KERNEL="$work/build/kernel" \
            SPROUTFS_BENCH_IMAGE="$work/build/root.ext4" \
            SPROUTFS_BENCH_WORK="$run/state" SPROUTFS_BENCH_OBJECT_DIR="$run/objects" \
            SPROUTFS_BENCH_OUTPUT="$results/fanout-$name.json" \
            SPROUTFS_BENCH_REVISION="$(cat "$repo/source-revision.txt")" \
            SPROUTFS_BENCH_SCENARIOS=boot,capture,fork-fanout SPROUTFS_BENCH_FORKS=4 \
            SPROUTFS_BENCH_TEST="$command" \
            "$work/build/vmmachine.test" -test.v -test.run '^TestGuestWorkloadBenchmark$' -test.timeout=10m \
            > "$results/fanout-$name.log" 2>&1
        rm -rf -- "$run"
    }
    fanout ran-before '/bin/busybox uname -a'
    fanout first-run '/usr/local/bin/memprobe 16'
    debugfs -R 'stat /usr/local/bin/memprobe' "$work/build/root.ext4" > "$results/memprobe-extents.txt" 2>&1
    exit 0
fi

# Exercise the real syscall and mapping protocol before trusting guest timings.
export SPROUTFS_VM_MEMORY_CLIENT="$CARGO_TARGET_DIR/debug/examples/client"
"$work/build/vmtest.test" -test.v -test.timeout=3m > "$results/vmtest.log" 2>&1
"$work/build/vmmemory.test" -test.v -test.timeout=3m > "$results/vmmemory.log" 2>&1
SPROUTFS_HUGETLB_EXHAUSTION=1 "$work/build/vmmemory.test" -test.v \
    -test.run '^TestLinuxArenaPoolExhaustionReturnsError$' -test.timeout=1m > "$results/pool-exhaustion.log" 2>&1
# The correctness suite's PMEM root has a deliberately smaller fixed geometry.
mkdir -p "$work/build/qualification-root/dev" "$work/build/qualification-root/proc" \
    "$work/build/qualification-root/sys" "$work/build/qualification-root/mnt"
cp "$root/init" "$work/build/qualification-root/init"
truncate -s 64M "$work/build/qualification.ext4"
mkfs.ext4 -q -F -b 4096 -d "$work/build/qualification-root" "$work/build/qualification.ext4"
env SPROUTFS_FIRECRACKER="$work/build/firecracker" \
    SPROUTFS_FIRECRACKER_SECCOMP="$work/build/seccomp.bpf" \
    SPROUTFS_FIRECRACKER_KERNEL="$work/build/kernel" \
    SPROUTFS_FIRECRACKER_ROOT="$work/build/qualification.ext4" \
    SPROUTFS_FIRECRACKER_RESIDENT_PAGES=48 \
    "$work/build/vmmachine.test" -test.v -test.timeout=8m \
    -test.run '^TestFirecracker(DAXCaptureRestoreForkAndFence|LiveMigration)$' > "$results/firecracker.log" 2>&1

for round in 1 2 3; do "$root/usr/local/bin/memprobe" 1024; done > "$results/native.log"

for vcpus in 1 4; do
    for round in 1 2 3; do
        run=$(mktemp -d "$work/run-$vcpus-$round.XXXXXX")
        env SPROUTFS_FIRECRACKER_BENCH=1 \
            SPROUTFS_FIRECRACKER="$work/build/firecracker" \
            SPROUTFS_FIRECRACKER_SECCOMP="$work/build/seccomp.bpf" \
            SPROUTFS_FIRECRACKER_KERNEL="$work/build/kernel" \
            SPROUTFS_BENCH_IMAGE="$work/build/root.ext4" \
            SPROUTFS_BENCH_WORK="$run/state" SPROUTFS_BENCH_OBJECT_DIR="$run/objects" \
            SPROUTFS_BENCH_OUTPUT="$results/managed-$vcpus-$round.json" \
            SPROUTFS_BENCH_REVISION="$(cat "$repo/source-revision.txt")" \
            SPROUTFS_BENCH_VCPUS="$vcpus" \
            SPROUTFS_BENCH_SCENARIOS=boot,baseline SPROUTFS_BENCH_RUN='/usr/local/bin/memprobe 1024' \
            "$work/build/vmmachine.test" -test.v -test.run '^TestGuestWorkloadBenchmark$' -test.timeout=10m \
            > "$results/managed-$vcpus-$round.log" 2>&1
        rm -rf -- "$run"
    done
done
for mode in None Transparent; do
    for round in 1 2 3; do
        python3 scripts/bench-plain-memory.py --binary "$work/build/firecracker" \
            --kernel "$work/build/kernel" --root "$work/build/root.ext4" --huge-pages "$mode" \
            --command '/usr/local/bin/memprobe 1024' > "$results/plain-$mode-$round.jsonl"
    done
done
