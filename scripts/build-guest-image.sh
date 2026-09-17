#!/usr/bin/env bash
# Build an x86_64 demo guest image on this Linux host.
#
# Every image is an ext4 filesystem with 4 KiB blocks holding an Alpine x86_64
# minirootfs and a static /init built from deploy/guest/init.c, which runs an
# interactive shell on the serial console. There are two templates:
#
#   alpine    2 GiB, the minirootfs and nothing else. No package is installed
#             into it, and the only download the build makes is the minirootfs
#             itself, pinned by version and checked against a recorded sha256.
#             A guest cold started onto a larger root volume grows its
#             filesystem with the guest witness's own `grow`, which is an ioctl
#             on the mount point and needs no tool this image would have to
#             carry.
#   workload  5 GiB, the same plus git, ripgrep, Node and pnpm, and three
#             cloned MIT-licensed TypeScript repositories whose dependencies are
#             in a pnpm store inside the image. Their node_modules are removed
#             after that store is populated, so a guest with no network can run
#             `pnpm install --offline --frozen-lockfile` in each of them and
#             have it do real work. docs/measurements-2026-09-14-workload.md
#             records which repositories and their licences.
#
# Neither guest ever needs a network; only this build does, and the workload
# template needs it for Alpine's package index, npm and the clones.
#
# This runs on the demo VM, not on a developer's machine: the build is native
# x86_64 with no emulation, and it needs root through sudo to own the extracted
# tree and to write the filesystem. The workload template also chroots into that
# tree, which is why it is native and not cross-built.
#
# The image also carries two static Go binaries of ours. The guest agent,
# cmd/sproutfs-guest-agent, is what the host runs commands in the guest through.
# The guest witness, cmd/sproutfs-guest-witness, is what a guest answers with
# when it is asked whether its memory and its disk are what it wrote, which is
# what the soak checks after every fork, migration, stop, start and host loss.
# Each is either one already built, named as an argument, or one this script
# builds with the Go toolchain on this host. The demo node has no Go, so
# scripts/lib/demo-image.sh takes both out of the container image it has just
# built and passes them here.
#
# Usage: scripts/build-guest-image.sh [--template NAME]
#            [image-path [agent-binary [witness-binary]]]
# The image path defaults to /var/lib/sproutfs/guest.ext4 and is printed on
# stdout when the build succeeds; progress goes to stderr.
set -euo pipefail
repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
usage() { echo "Usage: $0 [--template alpine|workload] [image-path [agent-binary [witness-binary]]]" >&2; exit 2; }
template=alpine
while [[ ${1:-} == --* ]]; do
    case $1 in
        --template) template=${2:-}; shift 2 || usage ;;
        --template=*) template=${1#--template=}; shift ;;
        *) usage ;;
    esac
done
(($# <= 3)) || usage
case $template in
    alpine | workload) ;;
    *) echo "No template named $template." >&2; usage ;;
esac
image=${1:-/var/lib/sproutfs/guest.ext4}
agent=${2:-${SPROUTFS_GUEST_AGENT:-}}
witness=${3:-${SPROUTFS_GUEST_WITNESS:-}}

alpine_branch=v3.24
alpine_version=3.24.1
alpine_arch=x86_64
alpine_sha256=41f73e3cf5fa919b8aa5ca6b30dc48f0da2720776d7423e2a7748211456fe081
alpine_url=https://dl-cdn.alpinelinux.org/alpine/$alpine_branch/releases/$alpine_arch/alpine-minirootfs-$alpine_version-$alpine_arch.tar.gz
alpine_mirror=https://dl-cdn.alpinelinux.org/alpine/$alpine_branch
# The pnpm the workload image carries. It is pinned both for reproducibility and
# for the lockfiles: every repository below ships a lockfileVersion 9.0 file,
# which pnpm 11 reads without rewriting, so `--frozen-lockfile` holds.
pnpm_version=11.26.0
# The repositories the workload guest holds, as "directory url". All three are
# MIT-licensed TypeScript projects that build with pnpm and whose test suites
# need no network.
workload_repos=(
    "h3 https://github.com/h3js/h3"
    "unstorage https://github.com/unjs/unstorage"
    "ofetch https://github.com/unjs/ofetch"
)
case $template in
    alpine) image_size=2G ;;
    workload) image_size=5G ;;
esac

[[ $(uname -m) == x86_64 ]] || { echo "This image is x86_64; this host is $(uname -m)." >&2; exit 2; }
sudo -n true

# The tools the build needs are the C compiler with its static libc, the ext4
# tools and curl. On the demo VM they arrive from the distribution's packages.
missing=()
command -v cc >/dev/null || missing+=(gcc libc6-dev)
command -v mkfs.ext4 >/dev/null || missing+=(e2fsprogs)
command -v curl >/dev/null || missing+=(curl)
if ((${#missing[@]})); then
    command -v apt-get >/dev/null || { echo "Install these first: ${missing[*]}" >&2; exit 2; }
    echo "installing build tools: ${missing[*]}" >&2
    sudo -n apt-get update -qq >&2
    # NEEDRESTART_SUSPEND keeps Ubuntu's post-install hook from stopping to ask
    # about restarting services: this build has no business restarting the
    # demo's containerd, or the ssh session it is running under.
    sudo -n env DEBIAN_FRONTEND=noninteractive NEEDRESTART_SUSPEND=1 \
        apt-get install -y -qq "${missing[@]}" >&2
fi

work=$(mktemp -d /tmp/sproutfs-demo-guest.XXXXXX)
root=$work/root
# The workload template chroots into the tree, which needs the kernel's own
# filesystems inside it. They are unmounted before anything is removed: an rm
# over a live bind mount of /dev would delete the host's device nodes.
unmount_tree() {
    local point
    for point in dev/pts dev proc sys; do
        sudo -n umount --lazy "$root/$point" 2> /dev/null || true
    done
}
cleanup() {
    local status=$?
    unmount_tree
    sudo -n rm -rf -- "$work"
    exit $status
}
trap cleanup EXIT

echo "downloading Alpine $alpine_version $alpine_arch minirootfs" >&2
curl --fail --location --silent --show-error "$alpine_url" --output "$work/minirootfs.tar.gz"
echo "$alpine_sha256  $work/minirootfs.tar.gz" | sha256sum --check --status
mkdir -p "$root"
sudo -n tar -xzf "$work/minirootfs.tar.gz" -C "$root"

echo "building the guest init" >&2
cc -static -O2 -Wall -Wextra -Werror "$repo/deploy/guest/init.c" -o "$work/init"
sudo -n install -m 0755 "$work/init" "$root/init"

# The agent is what the host reaches the guest over its vsock with, and the
# witness is what a guest answers with when it is asked whether its memory and
# its disk are what it wrote. A prebuilt binary is used as it is; otherwise it
# is built here, statically, so the image needs no Go runtime and no shared
# library of ours.
#
# install_guest_binary NAME PATH-OR-EMPTY installs one of them at
# /usr/local/bin/NAME in the image being built, building it first when nothing
# was passed.
install_guest_binary() {
    local name=$1 supplied=$2
    if [[ -z $supplied ]]; then
        command -v go >/dev/null || {
            echo "No Go toolchain here and no prebuilt $name given." >&2
            echo "Pass one as an argument or in SPROUTFS_GUEST_AGENT/SPROUTFS_GUEST_WITNESS." >&2
            exit 2
        }
        echo "building $name" >&2
        (cd "$repo" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
            go build -trimpath -o "$work/$name" "./cmd/$name")
        supplied=$work/$name
    fi
    [[ -f $supplied ]] || { echo "No $name at $supplied" >&2; exit 2; }
    sudo -n install -D -m 0755 "$supplied" "$root/usr/local/bin/$name"
    echo "$name: $(sudo -n du -h "$root/usr/local/bin/$name" | cut -f1)" >&2
}
install_guest_binary sproutfs-guest-agent "$agent"
install_guest_binary sproutfs-guest-witness "$witness"

# The init mounts these itself; the directories have to exist in the image for
# it to have anything to mount onto.
sudo -n mkdir -p "$root/dev" "$root/proc" "$root/sys" "$root/run" "$root/root"
printf 'sproutfs-demo\n' | sudo -n tee "$root/etc/hostname" >/dev/null

# The workload template is the only part of this build that runs guest code: a
# chroot into the tree, which is native x86_64 here and needs a network for
# Alpine's index, npm and the clones. What it leaves behind needs none.
if [[ $template == workload ]]; then
    echo "building the workload tree: packages, pnpm and the repositories" >&2
    printf '%s/main\n%s/community\n' "$alpine_mirror" "$alpine_mirror" |
        sudo -n tee "$root/etc/apk/repositories" > /dev/null
    # A chroot shares this host's network stack, so the host's own resolver is
    # the one that answers inside it, stub listener and all.
    sudo -n cp -L /etc/resolv.conf "$root/etc/resolv.conf"
    sudo -n mount --bind /proc "$root/proc"
    sudo -n mount --bind /sys "$root/sys"
    sudo -n mount --bind /dev "$root/dev"
    sudo -n mkdir -p "$root/dev/pts"
    sudo -n mount --bind /dev/pts "$root/dev/pts"
    printf '%s\n' "${workload_repos[@]}" | sudo -n tee "$root/tmp/repos" > /dev/null
    printf 'pnpm_version=%s\n' "$pnpm_version" | sudo -n tee "$root/tmp/workload.env" > /dev/null
    sudo -n tee "$root/tmp/workload.sh" > /dev/null <<'WORKLOAD'
#!/bin/sh
# Populate the workload guest, from inside its own root. Written by
# scripts/build-guest-image.sh; nothing in the finished image runs it.
set -eu
. /tmp/workload.env
# The guest's init gives every command HOME=/root, so this build has to write
# the same home the guest will read.
export HOME=/root

apk add --no-cache git ripgrep nodejs npm
npm install --global --no-fund --no-audit "pnpm@$pnpm_version"

# pnpm's global configuration, written where pnpm looks for it rather than
# through `pnpm config set --global`, which refuses to run until pnpm's own
# global bin directory is on PATH — a shell setup this guest has no use for.
#
# store-dir is the one that matters: it is the store an offline install in the
# guest installs from, and it has to be inside the image. Managed package
# manager versions are off because a repository that declares a packageManager
# would otherwise have pnpm fetch that exact version, which a guest with no
# network cannot do; the pinned pnpm above reads every lockfile here.
mkdir -p "$HOME/.config/pnpm"
cat > "$HOME/.config/pnpm/rc" <<'RC'
store-dir=/var/cache/pnpm
fund=false
update-notifier=false
manage-package-manager-versions=false
RC
# git in the guest reads history rather than writing it, but a repository with
# no identity configured makes even a commit-less command complain.
git config --global user.name 'sproutfs demo'
git config --global user.email 'demo@sproutfs.invalid'
git config --global advice.detachedHead false

mkdir -p /root/repos
: > /root/repos/MANIFEST
while read -r name url; do
    [ -n "$name" ] || continue
    echo "cloning $name from $url" >&2
    # Enough history for `git log` to have something to print, and not the
    # whole of it: the clone is a workload, not an archive.
    git clone --quiet --depth 50 "$url" "/root/repos/$name"
    cd "/root/repos/$name"
    echo "installing the dependencies of $name" >&2
    pnpm install --frozen-lockfile --reporter=silent
    printf '%s\t%s\t%s\t%s\n' "$name" "$url" "$(git rev-parse --short HEAD)" \
        "$(ls LICENSE LICENSE.md LICENCE COPYING 2> /dev/null | head -1)" \
        >> /root/repos/MANIFEST
    # The store stays and the installed tree goes, so that the guest's own
    # offline install has the work to do rather than finding it already done.
    find . -maxdepth 3 -type d -name node_modules -prune -exec rm -rf {} +
done < /tmp/repos

# Everything that was only needed to fetch the above.
rm -rf /root/.npm /var/cache/apk/* /tmp/repos /tmp/workload.env
echo "repositories in the image:" >&2
cat /root/repos/MANIFEST >&2
du -sh /var/cache/pnpm /root/repos >&2
WORKLOAD
    sudo -n chroot "$root" /bin/sh /tmp/workload.sh
    sudo -n rm -f "$root/tmp/workload.sh"
    unmount_tree
fi

# A resolver that points nowhere is honest about a guest with no network, and
# stops a name lookup in the demo's shell from hanging on an inherited file.
printf '# the demo guest has no network\n' | sudo -n tee "$root/etc/resolv.conf" >/dev/null

used=$(sudo -n du -sk "$root" | cut -f1)
echo "populated tree: $((used / 1024)) MiB" >&2

echo "creating the $image_size ext4 image" >&2
truncate -s "$image_size" "$work/guest.ext4"
sudo -n mkfs.ext4 -q -F -b 4096 -L sproutfs-demo -d "$root" "$work/guest.ext4"
sudo -n install -D -m 0644 "$work/guest.ext4" "$image"
echo "guest image written to $image ($(sudo -n du -h --apparent-size "$image" | cut -f1) apparent)" >&2
echo "$image"
