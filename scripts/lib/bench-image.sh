#!/usr/bin/env bash
# Build the workload guest image for the storage benchmark on this Linux host,
# natively, and cache it: scripts/build-guest-image-lima.sh runs this inside a
# Lima instance and scripts/lib/bench-memory-linux.sh on a disposable GCE host,
# so the two measure the same image on aarch64 and on x86-64.
#
# The image is a 32 GiB ext4 filesystem with 4 KiB blocks holding an Alpine
# userland of this host's architecture, a static /init built from
# vmmachine/testdata/guest.c, and four offline workloads: a pnpm
# project with a pre-populated store, a Rust workspace with vendored crates, a
# git repository with full history, and a Valkey database the guest seeds
# itself and then updates at random. No guest workload needs a network.
#
# Everything downloaded is pinned by version and checked against a recorded
# sha256; the codex checkout is pinned by commit, which is its content hash.
# The finished image is cached under ~/.cache/sproutfs-guest, keyed by the
# caller's hash of this script and of the guest init it embeds, so a rerun that
# changes neither skips the build entirely. The image is never committed.
#
# Prints the path of the image on stdout; progress goes to stderr.
set -euo pipefail
repo=${1:?repository required}
key=${2:?cache key required}

alpine_branch=v3.24
alpine_version=3.24.1
# The image is built natively, so its architecture is this host's, and each has
# its own pinned minirootfs.
alpine_arch=$(uname -m)
case $alpine_arch in
    aarch64) alpine_sha256=f55a90f69052c5bd6f92cb09a8f47065970830b194c917a006fb94028e721259 ;;
    x86_64) alpine_sha256=41f73e3cf5fa919b8aa5ca6b30dc48f0da2720776d7423e2a7748211456fe081 ;;
    *) echo "No pinned Alpine minirootfs for $alpine_arch." >&2; exit 2 ;;
esac
alpine_url=https://dl-cdn.alpinelinux.org/alpine/$alpine_branch/releases/$alpine_arch/alpine-minirootfs-$alpine_version-$alpine_arch.tar.gz
# openai/codex (Apache-2.0) at its rust-v0.155.1 release. The tag's commit is
# the content hash of the whole checkout, which is what both the Rust workload
# and the git workload are built from. Its workspace pins its own toolchain.
codex_tag=rust-v0.155.1
codex_commit=be2951ea34f0d295ed0becf97079f92fa5f6950e
codex_url=https://github.com/openai/codex.git
rust_toolchain=1.95.0

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

echo "downloading Alpine $alpine_version $alpine_arch minirootfs" >&2
curl --fail --location --silent --show-error "$alpine_url" --output "$work/minirootfs.tar.gz"
echo "$alpine_sha256  $work/minirootfs.tar.gz" | sha256sum --check --status
mkdir -p "$root"
sudo -n tar -xzf "$work/minirootfs.tar.gz" -C "$root"

# apk runs natively: the image is this host's architecture, so no emulation is
# involved. The
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
    bash coreutils findutils git python3 build-base cmake perl linux-headers \
    pkgconf openssl-dev openssl-libs-static nodejs npm pnpm rustup \
    valkey valkey-cli valkey-benchmark >&2

sudo -n tee "$root/sproutfs-workloads.sh" >/dev/null <<'INNER'
#!/bin/sh
set -eu
export HOME=/root
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
mkdir -p /opt /root

# The toolchain the workspace pins, with the components its rust-toolchain.toml
# names: a guest has no network, and rustup would otherwise reach for whatever
# is missing the first time cargo runs there. It lives under $HOME, which is
# where a guest's rustup looks, and its proxies are put on the guest's PATH.
rustup-init -y --no-modify-path --profile minimal --default-toolchain "$RUST_TOOLCHAIN" \
    --component clippy --component rustfmt --component rust-src
for tool in cargo rustc rustdoc rustup; do
    ln -sf "/root/.cargo/bin/$tool" "/usr/local/bin/$tool"
done

# (b) and (c) the Rust workload and the git workload: the pinned codex checkout.
# The clone is shallow; what the git workload reads is the tree, and the build
# needs no history.
git config --global --add safe.directory '*'
git config --global user.email sproutfs@example.invalid
git config --global user.name sproutfs
git clone --quiet --depth 1 --branch "$CODEX_TAG" "$CODEX_URL" /opt/codex
cd /opt/codex
test "$(git rev-parse HEAD)" = "$CODEX_COMMIT"

# Every crate is vendored, the git dependencies included, so cargo needs no
# registry index and no network. The release tag's Cargo.lock does not satisfy
# --locked against its own manifests, so the lock the vendoring resolves is the
# one the guest builds with, and it is committed so the tree a guest sees is
# clean.
cd /opt/codex/codex-rs
mkdir -p .cargo
cargo vendor --versioned-dirs vendor >> .cargo/config.toml
# The registry cache the vendoring filled is not needed once the sources are in
# the tree, and the image is measured on its bytes.
rm -rf /root/.cargo/registry /root/.cargo/git

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

# (d) the database workload: Valkey (BSD-3-Clause), an in-memory key-value
# store, which the guest seeds with keys of one size and then updates at random
# in place. Nothing of the data is in the image: the guest builds it in its own
# RAM, so a fork of the seeded guest inherits every page of it and an update
# stores into one of them at random. It listens on a unix socket, because a
# guest brings up no network interface at all.
mkdir -p /opt/db
cat > /opt/db/start.sh <<'SH'
#!/bin/sh
# Start the server with nothing persisted, and return once it answers.
set -eu
valkey-server --daemonize yes --save '' --appendonly no --port 0 \
    --unixsocket /tmp/valkey.sock --unixsocketperm 700 --dir /tmp
until valkey-cli -s /tmp/valkey.sock ping > /dev/null 2>&1; do sleep 0.1; done
SH
cat > /opt/db/seed.py <<'PY'
"""Write SET commands for keys key:000000000000 up to the count given, each
holding a value of the size given, in the protocol valkey-cli --pipe reads.
The key names are the ones valkey-benchmark's __rand_int__ expands to, so its
random updates land on keys that are already there."""
import sys
count, size = int(sys.argv[1]), int(sys.argv[2])
out = sys.stdout.buffer
batch = []
for i in range(count):
    key = b"key:%012d" % i
    value = (b"%012d" % i) * (size // 12) + b"v" * (size % 12)
    batch.append(b"*3\r\n$3\r\nSET\r\n$%d\r\n%s\r\n$%d\r\n%s\r\n" % (len(key), key, len(value), value))
    if len(batch) == 10000:
        out.write(b"".join(batch))
        batch.clear()
out.write(b"".join(batch))
PY
cat > /opt/db/seed.sh <<'SH'
#!/bin/sh
# Seed the running server with $1 keys of $2 bytes each.
set -eu
python3 /opt/db/seed.py "$1" "$2" | valkey-cli -s /tmp/valkey.sock --pipe
valkey-cli -s /tmp/valkey.sock info memory | grep -E '^used_memory:'
SH
cat > /opt/db/update.sh <<'SH'
#!/bin/sh
# $1 updates by one client, each overwriting the first bytes of a key chosen at
# random among the $2 seeded, in place: one small store into the page that
# value is on. The CSV line is the latency distribution of those updates.
set -eu
valkey-benchmark -s /tmp/valkey.sock -c 1 -n "$1" -r "$2" --csv \
    SETRANGE key:__rand_int__ 0 sproutfs
SH
chmod 0755 /opt/db/start.sh /opt/db/seed.sh /opt/db/update.sh

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
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
cd /opt/app
pnpm install --offline --frozen-lockfile --reporter=append-only
rm -rf /opt/app/node_modules
# The whole build the guest will be measured on, once, natively and with no
# network: a toolchain or a crate that is not in the image fails here rather
# than an hour into a measured run. What it wrote is removed again, so the
# guest's build is cold.
# Each step is a command of its own: under set -e a failure inside an && list
# does not stop the script, and an image whose build failed must not be sealed.
cd /opt/codex/codex-rs
cargo build --offline -p codex-cli --bin codex
# What the fan-out's forks run, which builds test targets the binary does not:
# their dev-dependencies reach for OpenSSL through pkg-config. The two tests
# left out expect a write to be refused, and a guest runs as root, which is
# refused nothing.
cargo test --offline -p codex-apply-patch -- \
    --skip test_apply_patch_fails_on_write_error \
    --skip test_failed_move_returns_committed_destination_delta
cargo clean --offline
cd /opt/codex
git grep -c fn > /dev/null
# The database workload end to end, small: a server, a seed, updates whose CSV
# names its percentiles, and the server gone again so the image holds no data.
/opt/db/start.sh
/opt/db/seed.sh 1000 1024
/opt/db/update.sh 100 1000 | grep -q p99_latency_ms
valkey-cli -s /tmp/valkey.sock shutdown nosave || true
rm -f /tmp/valkey.sock
INNER
sudo -n chmod 0755 "$root/sproutfs-verify.sh"
sudo -n chmod 0755 "$root/sproutfs-workloads.sh"
echo "vendoring offline workloads" >&2
sudo -n chroot "$root" /usr/bin/env \
    CODEX_URL="$codex_url" CODEX_TAG="$codex_tag" CODEX_COMMIT="$codex_commit" \
    RUST_TOOLCHAIN="$rust_toolchain" \
    /bin/sh /sproutfs-workloads.sh >&2
echo "verifying the workloads run with no network" >&2
sudo -n unshare --net chroot "$root" /bin/sh /sproutfs-verify.sh >&2
sudo -n rm -f "$root/sproutfs-workloads.sh" "$root/sproutfs-verify.sh" "$root/etc/resolv.conf"

echo "building the guest init" >&2
cc -static -O2 -Wall -Wextra -Werror "$repo/vmmachine/testdata/guest.c" -o "$work/init"
sudo -n cp "$work/init" "$root/init"
sudo -n chmod 0755 "$root/init"

for point in dev/pts dev proc sys; do
    if mountpoint -q "$root/$point"; then
        sudo -n umount "$root/$point"
    fi
done
used=$(sudo -n du -sk "$root" | cut -f1)
echo "populated tree: $((used / 1024)) MiB" >&2

# The build alone writes 12 GiB under target/, so the filesystem is sized for
# it; the file is sparse and the populated tree is what it costs on disk.
echo "creating the 32 GiB ext4 image" >&2
truncate -s 32G "$work/root.ext4"
sudo -n mkfs.ext4 -q -F -b 4096 -d "$root" "$work/root.ext4"
sudo -n chown "$(id -u):$(id -g)" "$work/root.ext4"

mkdir -p "$cache"
allocated=$(du --block-size=1 "$work/root.ext4" | cut -f1)
python3 - "$cache/manifest.json" <<PY
import json, sys
json.dump({
    "alpine_branch": "$alpine_branch",
    "alpine_version": "$alpine_version",
    "alpine_arch": "$alpine_arch",
    "alpine_url": "$alpine_url",
    "alpine_sha256": "$alpine_sha256",
    "codex_tag": "$codex_tag",
    "codex_commit": "$codex_commit",
    "codex_url": "$codex_url",
    "rust_toolchain": "$rust_toolchain",
    "packages": "bash coreutils findutils git python3 build-base cmake perl linux-headers pkgconf openssl-dev openssl-libs-static nodejs npm pnpm rustup valkey valkey-cli valkey-benchmark",
    "prebuilt": "nothing: cargo build --offline -p codex-cli --bin codex is proved with no network and then cleaned",
    "npm": {"react": "19.3.0", "react-dom": "19.3.0", "vite": "8.2.2", "@vitejs/plugin-react": "6.1.1"},
    "image_bytes": 32 << 30,
    "block_size": 4096,
    "populated_kib": $used,
    "allocated_bytes": $allocated,
}, open(sys.argv[1], "w"), indent=2, sort_keys=True)
PY
mv "$work/root.ext4" "$image"
echo "guest image cached at $image" >&2
echo "$image"
