#!/usr/bin/env bash
# Real Linux PMEM-root boot, bounded residency, capture, fork, fencing, and the
# live migration of a running guest between two pagers in one process.
set -euo pipefail
repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
instance=${SPROUTFS_LIMA_INSTANCE:-default}
toolchain=${SPROUTFS_RUST_TOOLCHAIN:-stable}
if (($# != 0)); then echo "Usage: $0 (SPROUTFS_FIRECRACKER_RUN selects tests by -test.run)" >&2; exit 2; fi
arch=$(limactl shell "$instance" uname -m)
if [[ "$arch" != aarch64 ]]; then
    echo 'This full-guest qualification pins an aarch64 kernel. Use the memory suite for x86_64.' >&2
    exit 1
fi
# shellcheck source=scripts/lib/firecracker-build.sh
source "$repo/scripts/lib/firecracker-build.sh"
limactl shell "$instance" sudo -n test -r /dev/kvm
# A 96 MiB budget of explicit 2 MiB pages forces eviction, spill and refault.
# Provision the HugeTLB pool in the instance before running this suite.
page_bytes=2097152
resident_pages=${SPROUTFS_FIRECRACKER_RESIDENT_PAGES:-$(((96 << 20) / page_bytes))}
mkdir -p "$HOME/.cache"
host_work=$(mktemp -d "$HOME/.cache/sproutfs-firecracker.XXXXXX")
guest_work=''
cleanup() {
    if [[ -n "$guest_work" ]]; then
        limactl shell "$instance" sudo -n rm -rf -- "$guest_work"
        limactl shell "$instance" test ! -e "$guest_work"
    fi
    rm -rf -- "$host_work"
}
trap cleanup EXIT
guest_work=$(limactl shell "$instance" mktemp -d /tmp/sproutfs-firecracker.XXXXXX)
sproutfs_firecracker_build "$instance" "$repo" "$guest_work" "$toolchain"
cd "$repo"
# The guest agent goes into the root image: the host reaches a guest over the
# VM's vsock rather than over its console, and a checkpoint has to be
# transparent to a command running that way. It is the deployment's own agent,
# built for the guest exactly as a deployment builds it.
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o "$host_work/sproutfs-guest-agent" ./cmd/sproutfs-guest-agent
# The guest witness goes in beside it. A guest cold started onto a larger root
# volume grows its filesystem with `witness grow`, which is an ioctl on the
# mount point rather than anything resize2fs can do on this kernel, and the only
# place that can be qualified is a real guest on a real root device.
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o "$host_work/sproutfs-guest-witness" ./cmd/sproutfs-guest-witness
limactl shell "$instance" bash -s -- "$repo" "$guest_work" \
    "$host_work/sproutfs-guest-agent" "$host_work/sproutfs-guest-witness" <<'GUEST'
set -euo pipefail
repo=$1
work=$2
agent=$3
witness=$4
mkdir -p "$work/root/dev" "$work/root/proc" "$work/root/sys" "$work/root/mnt" "$work/root/bin"
cc -static -O2 -Wall -Wextra -Werror "$repo/vmmachine/testdata/guest.c" -o "$work/root/init"
install -m 0755 "$agent" "$work/root/agent"
install -m 0755 "$witness" "$work/root/bin/sproutfs-guest-witness"
# The agent runs what it is asked to through a shell, so the image needs one.
# The instance's own static busybox is it: nothing is installed, downloaded or
# changed in the instance, and the applets the suite asks for are links to it.
busybox=$(command -v busybox || echo /usr/lib/initramfs-tools/bin/busybox)
if [[ ! -x "$busybox" ]]; then
    echo 'This suite needs a static busybox in the instance (apt install busybox-static).' >&2
    exit 1
fi
install -m 0755 "$busybox" "$work/root/bin/busybox"
for applet in sh sleep echo test touch cat df sync; do ln -sf busybox "$work/root/bin/$applet"; done
truncate -s 64M "$work/root.ext4"
mkfs.ext4 -q -F -b 4096 -d "$work/root" "$work/root.ext4"
GUEST
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go test -c ./vmmachine -o "$host_work/firecracker.test"
limactl shell "$instance" sudo -n env \
    SPROUTFS_FIRECRACKER="$guest_work/firecracker" \
    SPROUTFS_FIRECRACKER_SECCOMP="$guest_work/seccomp.bpf" \
    SPROUTFS_FIRECRACKER_KERNEL="$guest_work/kernel" \
    SPROUTFS_FIRECRACKER_ROOT="$guest_work/root.ext4" \
    SPROUTFS_FIRECRACKER_RESIDENT_PAGES="$resident_pages" \
    "$host_work/firecracker.test" -test.v -test.timeout=30m -test.run="${SPROUTFS_FIRECRACKER_RUN:-}"
