#!/usr/bin/env bash
# Run the fork fan-out N times in an existing Lima VM and classify every run.
#
# It exists for one open defect: a fan-out of two children at a 4 KiB RAM page
# panics a child's guest kernel on a data structure the guest itself wrote. The
# panic is rare — about one run in five, or one in two on the accelerated build
# — so deciding whether a change removes it needs many runs and an honest count
# of what each one did, which is what this writes down.
#
# Usage: scripts/fanout-reduce-lima.sh RUNS [LABEL]
#
# It builds the qualification VMM, kernel and guest image once into a directory
# that outlives the run, so an arm costs one build and N boots. The Go test
# binary is rebuilt from the working tree every time, which is what makes an arm
# an arm: edit the tree, run this, compare.
#
#   SPROUTFS_FANOUT_TAGS    build tags for the test binary, default sproutfsprobe
#                           (the accelerator; see internal/vmmemory/probe_on.go)
#   SPROUTFS_LIMA_INSTANCE  the instance, default `default`
#   SPROUTFS_FANOUT_WORK    the persistent build directory in the instance
#   SPROUTFS_FANOUT_TEST    which test one run is, default the fan-out's own;
#                           TestFirecrackerForkChildrenSurviveTheirFirstSeconds
#                           is the same defect measured by the second, and takes
#                           SPROUTFS_FORK_ARM and SPROUTFS_FORK_LIVES with it
#   SPROUTFS_RAM_PAGE_BYTES the RAM pager's page, 2097152 by default as a host
#                           runs it; 4096 runs the page a deployment may choose
#
# One line per run goes to the results file, classified as:
#   panic    the guest kernel died — the defect, whatever else the run did
#   bytes    a test assertion about what a guest read failed
#   timeout  a liveness bound expired: confounded by anything else on the host,
#            so it counts as neither a pass nor the defect
#   other    any other failure
#   pass
#
# Lima must be otherwise idle: a guest starved by another tenant hits the
# liveness bound, which looks like a slow run and is not one. The header records
# the load average so a batch taken on a busy host can be thrown away.
set -euo pipefail

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
instance=${SPROUTFS_LIMA_INSTANCE:-default}
tags=${SPROUTFS_FANOUT_TAGS-sproutfsprobe}
if (($# < 1 || $# > 2)); then
    echo "Usage: $0 RUNS [LABEL]" >&2
    exit 2
fi
runs=$1
label=${2:-arm}
if [[ "$(limactl shell "$instance" uname -m)" != aarch64 ]]; then
    echo 'This fixture pins an aarch64 kernel.' >&2
    exit 1
fi
limactl shell "$instance" sudo -n test -r /dev/kvm

home=$(limactl shell "$instance" printenv HOME)
work=${SPROUTFS_FANOUT_WORK:-$home/.cache/sproutfs-fanout}
mkdir -p "$HOME/.cache/sproutfs-reduce"
results=$HOME/.cache/sproutfs-reduce/$label.tsv
logs=$HOME/.cache/sproutfs-reduce/$label.log

# The VMM, its policy, the kernel and the guest image, built once.
if ! limactl shell "$instance" test -s "$work/root.ext4"; then
    echo "building the qualification image into $work" >&2
    # shellcheck source=scripts/lib/firecracker-build.sh
    source "$repo/scripts/lib/firecracker-build.sh"
    limactl shell "$instance" mkdir -p "$work"
    sproutfs_firecracker_build "$instance" "$repo" "$work"
    host_work=$(mktemp -d "$HOME/.cache/sproutfs-reduce/build.XXXXXX")
    trap 'rm -rf -- "$host_work"' EXIT
    GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o "$host_work/agent" "$repo/cmd/sproutfs-guest-agent"
    GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o "$host_work/witness" "$repo/cmd/sproutfs-guest-witness"
    limactl shell "$instance" bash -s -- "$repo" "$work" "$host_work/agent" "$host_work/witness" <<'GUEST'
set -euo pipefail
repo=$1; work=$2; agent=$3; witness=$4
mkdir -p "$work/root/dev" "$work/root/proc" "$work/root/sys" "$work/root/mnt" "$work/root/bin"
cc -static -O2 -Wall -Wextra -Werror "$repo/internal/vmmachine/testdata/guest.c" -o "$work/root/init"
install -m 0755 "$agent" "$work/root/agent"
install -m 0755 "$witness" "$work/root/bin/sproutfs-guest-witness"
busybox=$(command -v busybox || echo /usr/lib/initramfs-tools/bin/busybox)
test -x "$busybox"
install -m 0755 "$busybox" "$work/root/bin/busybox"
for applet in sh sleep echo test touch cat df; do ln -sf busybox "$work/root/bin/$applet"; done
truncate -s 64M "$work/root.ext4"
mkfs.ext4 -q -F -b 4096 -d "$work/root" "$work/root.ext4"
GUEST
fi

binary=$HOME/.cache/sproutfs-reduce/$label.test
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go test -c ${tags:+-tags "$tags"} \
    -o "$binary" "$repo/internal/vmmachine"

load=$(limactl shell "$instance" cat /proc/loadavg)
# An arm is a change to the tree, so the change itself is written down beside
# the result: a count of modified files says nothing a later reader can rerun.
patch=$HOME/.cache/sproutfs-reduce/$label.patch
(cd "$repo" && git diff HEAD) > "$patch"
{
    echo "# $label: $runs runs of ${SPROUTFS_FANOUT_TEST:-the fan-out}${SPROUTFS_FORK_ARM:+ arm $SPROUTFS_FORK_ARM}, tags=${tags:-none}, ram page=${SPROUTFS_RAM_PAGE_BYTES:-2097152}, load before=$load"
    echo "# $(cd "$repo" && git rev-parse --short HEAD) plus $(wc -l < "$patch" | tr -d ' ') lines of $patch"
} > "$results"
echo "results: $results" >&2

panics=0 bytes=0 timeouts=0 others=0 passes=0
for ((run = 1; run <= runs; run++)); do
    one=$(mktemp "$HOME/.cache/sproutfs-reduce/run.XXXXXX")
    started=$SECONDS
    set +e
    limactl shell "$instance" sudo -n env \
        SPROUTFS_FIRECRACKER="$work/firecracker" \
        SPROUTFS_FIRECRACKER_SECCOMP="$work/seccomp.bpf" \
        SPROUTFS_FIRECRACKER_KERNEL="$work/kernel" \
        SPROUTFS_FIRECRACKER_ROOT="$work/root.ext4" \
        SPROUTFS_FIRECRACKER_RESIDENT_PAGES="${SPROUTFS_FIRECRACKER_RESIDENT_PAGES:-48}" \
        SPROUTFS_RAM_PAGE_BYTES="${SPROUTFS_RAM_PAGE_BYTES:-}" \
        SPROUTFS_FORK_ARM="${SPROUTFS_FORK_ARM:-}" \
        SPROUTFS_FORK_LIVES="${SPROUTFS_FORK_LIVES:-}" \
        "$binary" -test.v -test.run "${SPROUTFS_FANOUT_TEST:-TestFirecrackerForkFanOutServesBothChildrenAtOnce}" \
        -test.count=1 -test.timeout=30m > "$one" 2>&1
    set -e
    elapsed=$((SECONDS - started))
    # The fork-life suite counts children rather than runs, so its tally is the
    # detail whatever the verdict is: a run of it is many trials, not one.
    tally=$(grep -m1 -o 'fork lives: arm=.* died=.*' "$one" || true)
    # A guest kernel that died is the defect whatever else the run reported, so
    # it is looked for first.
    if grep -q 'Kernel panic - not syncing' "$one"; then
        verdict=panic
        detail="${tally:-$(grep -m1 -o 'pc : [^ ]*' "$one" | head -1)}"
        panics=$((panics + 1))
    elif grep -q '^probe ' "$one" || grep -q 'panic: probe ' "$one"; then
        verdict=bytes
        detail=$(grep -m1 -o 'probe [^"]*' "$one" | head -1)
        bytes=$((bytes + 1))
    elif grep -q -- '--- PASS' "$one"; then
        verdict=pass
        detail="${tally:-$(grep -m1 -o 'fan-out read: .*' "$one" || true)}"
        passes=$((passes + 1))
    elif grep -q 'context deadline exceeded' "$one"; then
        verdict=timeout
        detail=$(grep -m1 -o 'reading [^:]*' "$one" || true)
        timeouts=$((timeouts + 1))
    else
        verdict=other
        detail=$(grep -m1 -- '--- FAIL' "$one" || echo 'no verdict line')
        others=$((others + 1))
    fi
    printf '%d\t%s\t%ds\t%s\n' "$run" "$verdict" "$elapsed" "$detail" >> "$results"
    printf '%d/%d %s %ds\n' "$run" "$runs" "$verdict" "$elapsed" >&2
    if [[ "$verdict" != pass ]]; then cat "$one" >> "$logs"; fi
    rm -f -- "$one"
done
summary="$label: $passes pass, $panics panic, $bytes bytes, $timeouts timeout, $others other, of $runs"
echo "# $summary" >> "$results"
echo "$summary" >&2
