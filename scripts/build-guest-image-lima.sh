#!/usr/bin/env bash
# Build the workload guest image for the storage benchmark inside a Lima
# instance and cache it there.
#
# The image is an 8 GiB ext4 filesystem with 4 KiB blocks holding an Alpine
# aarch64 userland, a static /init built from internal/vmmachine/testdata/guest.c, and
# three offline workloads: a pnpm project with a pre-populated store, a Rust
# workspace with vendored crates, and a git repository with full history. No
# guest workload needs a network.
#
# Everything downloaded is pinned by version and checked against a recorded
# sha256; the ripgrep checkout is pinned by commit, which is its content hash.
# The finished image is cached under ~/.cache/sproutfs-guest in the instance,
# keyed by the sha256 of this script and of the guest init it embeds, so a rerun
# that changes neither skips the build entirely. The image is never committed.
#
# Prints the guest-side path of the image on stdout; progress goes to stderr.
set -euo pipefail
repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
instance=${SPROUTFS_LIMA_INSTANCE:-default}
if (($# != 0)); then echo "Usage: $0" >&2; exit 2; fi
key=$(cat "${BASH_SOURCE[0]}" "$repo/internal/vmmachine/testdata/guest.c" | shasum -a 256 | cut -c1-32)
limactl shell "$instance" sudo -n true
limactl shell "$instance" bash -s -- "$repo" "$key" <<'GUEST'
set -euo pipefail
repo=$1
key=$2

alpine_branch=v3.24
alpine_version=3.24.1
alpine_sha256=f55a90f69052c5bd6f92cb09a8f47065970830b194c917a006fb94028e721259
alpine_url=https://dl-cdn.alpinelinux.org/alpine/$alpine_branch/releases/aarch64/alpine-minirootfs-$alpine_version-aarch64.tar.gz
# ripgrep 15.2.0. The tag's commit is the content hash of the whole checkout,
# which is what both the Rust workspace and the git workload are built from.
ripgrep_tag=15.2.0
ripgrep_commit=e89fff89ac9af12e8d4ce9d5fd07beb408ca730f
ripgrep_url=https://github.com/BurntSushi/ripgrep.git

cache=$HOME/.cache/sproutfs-guest/$key
image=$cache/root.ext4
if [[ -s $image && -s $cache/manifest.json ]]; then
    echo "reusing cached guest image $image" >&2
    echo "$image"
    exit 0
fi

work=$(mktemp -d /tmp/sproutfs-guest-build.XXXXXX)
root=$work/root
cleanup() {
    local status=$?
    local point
    for point in dev/pts dev proc sys; do
        if mountpoint -q "$root/$point"; then
            sudo -n umount "$root/$point"
        fi
    done
    sudo -n rm -rf -- "$work"
    exit $status
}
trap cleanup EXIT

echo "downloading Alpine $alpine_version aarch64 minirootfs" >&2
curl --fail --location --silent --show-error "$alpine_url" --output "$work/minirootfs.tar.gz"
echo "$alpine_sha256  $work/minirootfs.tar.gz" | sha256sum --check --status
mkdir -p "$root"
sudo -n tar -xzf "$work/minirootfs.tar.gz" -C "$root"

# apk runs natively: the instance is aarch64, so no emulation is involved. The
# chroot shares this host's network namespace, which is all the build needs.
printf 'https://dl-cdn.alpinelinux.org/alpine/%s/main\nhttps://dl-cdn.alpinelinux.org/alpine/%s/community\n' \
    "$alpine_branch" "$alpine_branch" | sudo -n tee "$root/etc/apk/repositories" >/dev/null
sudo -n cp -L /etc/resolv.conf "$root/etc/resolv.conf"
sudo -n mkdir -p "$root/dev" "$root/proc" "$root/sys" "$root/mnt" "$root/opt"
sudo -n mount --bind /dev "$root/dev"
sudo -n mount --bind /proc "$root/proc"
sudo -n mount --bind /sys "$root/sys"

echo "installing Alpine packages" >&2
sudo -n chroot "$root" /sbin/apk add --no-cache \
    bash coreutils findutils git python3 build-base nodejs npm pnpm rust cargo >&2

sudo -n tee "$root/sproutfs-workloads.sh" >/dev/null <<'INNER'
#!/bin/sh
set -eu
export HOME=/root
export CARGO_HOME=/opt/cargo
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
mkdir -p /opt /root

# (c) git workload: the pinned ripgrep checkout with its full history.
git config --global --add safe.directory '*'
git config --global user.email sproutfs@example.invalid
git config --global user.name sproutfs
git clone --quiet "$RIPGREP_URL" /opt/repo
cd /opt/repo
git checkout --quiet "$RIPGREP_COMMIT"
test "$(git rev-parse HEAD)" = "$RIPGREP_COMMIT"

# (b) Rust workload: the same tree with every crate vendored, so cargo needs no
# registry index and no network.
cp -a /opt/repo /opt/rust
rm -rf /opt/rust/.git
cd /opt/rust
mkdir -p .cargo
# ripgrep ships its own .cargo/config.toml; the source replacement is appended
# to it rather than replacing the target settings it already carries.
cargo vendor --locked --versioned-dirs vendor >> .cargo/config.toml
# Warm the dependency graph of the benchmark's build unit here, where a compile
# costs seconds, and then clean only the crate itself so the guest still has a
# real cold build to do. The guest is two orders of magnitude slower than this
# host: without the warming, the benchmark's cold build is hours of compiling
# regex and aho-corasick rather than a measurement of storage under a build, and
# its twenty concurrent forks never finish at all. What the guest builds cold is
# grep-matcher's own library and its three test targets; what the forks then run
# is those tests.
cargo build --offline --all-targets -p grep-matcher
cargo clean --offline -p grep-matcher
# The registry cache the vendoring filled is not needed once the sources are in
# the tree, and the image is measured on its bytes.
rm -rf /opt/cargo/registry /opt/cargo/git

# (a) pnpm workload: a vite + react app whose store is already populated, so
# `pnpm install --offline` links from disk.
mkdir -p /opt/app/src
cat > /opt/app/package.json <<'JSON'
{
  "name": "sproutfs-bench-app",
  "private": true,
  "type": "module",
  "scripts": { "build": "vite build" },
  "dependencies": {
    "react": "19.3.0",
    "react-dom": "19.3.0"
  },
  "devDependencies": {
    "@vitejs/plugin-react": "6.1.1",
    "vite": "8.2.2"
  }
}
JSON
# pnpm keeps its content-addressed store under $HOME, which is inside the image,
# so the guest's offline install links from disk. Two settings are what make the
# install actually offline: pnpm's minimum release age needs a publish date from
# the registry for every package, and a failed metadata request otherwise backs
# off for a minute before giving up.
cat > /opt/app/.npmrc <<'NPMRC'
update-notifier=false
strict-peer-dependencies=false
fetch-retries=0
fetch-timeout=2000
NPMRC
cat > /opt/app/pnpm-workspace.yaml <<'YAML'
minimumReleaseAge: 0
YAML
cat > /opt/app/vite.config.js <<'JS'
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
export default defineConfig({ plugins: [react()] })
JS
cat > /opt/app/index.html <<'HTML'
<!doctype html><html><body><div id="root"></div>
<script type="module" src="/src/main.jsx"></script></body></html>
HTML
cat > /opt/app/src/main.jsx <<'JS'
import { createRoot } from 'react-dom/client'
import App from './App.jsx'
createRoot(document.getElementById('root')).render(<App />)
JS
cat > /opt/app/src/App.jsx <<'JS'
export default function App() { return <h1>sproutfs</h1> }
JS
cd /opt/app
pnpm install --reporter=silent
# The guest must do the real install work, so only the store and the lockfile
# are shipped.
rm -rf /opt/app/node_modules

# Nothing here needs documentation, and the image is measured on its bytes.
rm -rf /usr/share/man /usr/share/doc /usr/share/gtk-doc /root/.cache /root/.npm
INNER
sudo -n tee "$root/sproutfs-verify.sh" >/dev/null <<'INNER'
#!/bin/sh
# Prove the workloads are offline before the image is sealed: this runs with no
# network namespace at all, so anything that reaches for a registry fails here
# rather than costing minutes of DNS backoff inside a measured guest.
set -eu
export HOME=/root
export CARGO_HOME=/opt/cargo
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
cd /opt/app && pnpm install --offline --frozen-lockfile --reporter=append-only
rm -rf /opt/app/node_modules
cd /opt/rust && cargo metadata --offline --format-version 1 > /dev/null
cd /opt/repo && git grep -c fn > /dev/null
INNER
sudo -n chmod 0755 "$root/sproutfs-verify.sh"
sudo -n chmod 0755 "$root/sproutfs-workloads.sh"
echo "vendoring offline workloads" >&2
sudo -n chroot "$root" /usr/bin/env \
    RIPGREP_URL="$ripgrep_url" RIPGREP_COMMIT="$ripgrep_commit" \
    /bin/sh /sproutfs-workloads.sh >&2
echo "verifying the workloads run with no network" >&2
sudo -n unshare --net chroot "$root" /bin/sh /sproutfs-verify.sh >&2
sudo -n rm -f "$root/sproutfs-workloads.sh" "$root/sproutfs-verify.sh" "$root/etc/resolv.conf"

echo "building the guest init" >&2
cc -static -O2 -Wall -Wextra -Werror "$repo/internal/vmmachine/testdata/guest.c" -o "$work/init"
sudo -n cp "$work/init" "$root/init"
sudo -n chmod 0755 "$root/init"

for point in dev/pts dev proc sys; do
    if mountpoint -q "$root/$point"; then
        sudo -n umount "$root/$point"
    fi
done
used=$(sudo -n du -sk "$root" | cut -f1)
echo "populated tree: $((used / 1024)) MiB" >&2

echo "creating the 8 GiB ext4 image" >&2
truncate -s 8G "$work/root.ext4"
sudo -n mkfs.ext4 -q -F -b 4096 -d "$root" "$work/root.ext4"
sudo -n chown "$(id -u):$(id -g)" "$work/root.ext4"

mkdir -p "$cache"
allocated=$(du --block-size=1 "$work/root.ext4" | cut -f1)
python3 - "$cache/manifest.json" <<PY
import json, sys
json.dump({
    "alpine_branch": "$alpine_branch",
    "alpine_version": "$alpine_version",
    "alpine_url": "$alpine_url",
    "alpine_sha256": "$alpine_sha256",
    "ripgrep_tag": "$ripgrep_tag",
    "ripgrep_commit": "$ripgrep_commit",
    "ripgrep_url": "$ripgrep_url",
    "packages": "bash coreutils findutils git python3 build-base nodejs npm pnpm rust cargo",
    "prebuilt": "cargo build --offline --all-targets -p grep-matcher, then cargo clean -p grep-matcher",
    "npm": {"react": "19.3.0", "react-dom": "19.3.0", "vite": "8.2.2", "@vitejs/plugin-react": "6.1.1"},
    "image_bytes": 8 << 30,
    "block_size": 4096,
    "populated_kib": $used,
    "allocated_bytes": $allocated,
}, open(sys.argv[1], "w"), indent=2, sort_keys=True)
PY
mv "$work/root.ext4" "$image"
echo "guest image cached at $image" >&2
echo "$image"
GUEST
