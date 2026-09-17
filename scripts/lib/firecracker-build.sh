# shellcheck shell=bash
# Sourced helper: build the feature-enabled Firecracker VMM, its compiled
# aarch64 seccomp policy and the pinned CI kernel inside a Lima instance.
#
# Every consumer of a full-guest Firecracker gets exactly this build, so the
# qualification suite and the workload benchmark can never diverge on the VMM,
# the policy or the kernel they measure.

# The kernel is an official Firecracker CI artifact, pinned by URL and checksum.
SPROUTFS_KERNEL_URL=https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/20260902-a6146c8bb213-0/aarch64/vmlinux-6.18.44
SPROUTFS_KERNEL_SHA256=013ba9494dfab7d41d406734e2ddd3ad287fb59ef41e94e48048414e7c899f08

# sproutfs_firecracker_build INSTANCE REPO WORK [TOOLCHAIN]
#
# Leaves WORK/firecracker, WORK/seccomp.bpf and WORK/kernel in the instance.
# Cargo's target directory defaults to ~/.cache/sproutfs-firecracker-target in
# the instance, which outlives WORK so the build is incremental across runs;
# SPROUTFS_FIRECRACKER_TARGET_DIR overrides it. The linker's libseccomp development symlink stays
# inside WORK: no guest packages, sysctls, device permissions or Lima settings
# are modified.
sproutfs_firecracker_build() {
    local instance=$1 repo=$2 work=$3 toolchain=${4:-${SPROUTFS_RUST_TOOLCHAIN:-stable}}
    # A worktree's submodule is cloned from the main checkout's module store
    # over the file transport, which git refuses by default; the store is this
    # repository's own, so allowing it here is not trusting anything new.
    git -C "$repo" -c protocol.file.allow=always submodule update --init third_party/firecracker
    limactl shell "$instance" bash -s -- \
        "$repo" "$work" "$toolchain" "${SPROUTFS_FIRECRACKER_TARGET_DIR:-}" \
        "$SPROUTFS_KERNEL_URL" "$SPROUTFS_KERNEL_SHA256" <<'GUEST'
set -euo pipefail
repo=$1
work=$2
export RUSTUP_TOOLCHAIN=$3
export CARGO_TARGET_DIR=${4:-$HOME/.cache/sproutfs-firecracker-target}
kernel_url=$5
kernel_sha256=$6
export LIBRARY_PATH="$work/lib"
mkdir -p "$work/lib" "$CARGO_TARGET_DIR"
lib=$(ldconfig -p | awk '/libseccomp.so.2 .* => / {print $NF; exit}')
test -n "$lib"
ln -sf "$lib" "$work/lib/libseccomp.so"
cargo build --locked --manifest-path "$repo/third_party/firecracker/Cargo.toml" -p firecracker --features sproutfs-memory
cargo build --locked --manifest-path "$repo/third_party/firecracker/Cargo.toml" -p seccompiler --bin seccompiler-bin
cp "$CARGO_TARGET_DIR/debug/firecracker" "$work/firecracker"
"$CARGO_TARGET_DIR/debug/seccompiler-bin" --target-arch aarch64 \
    --input-file "$repo/third_party/firecracker/resources/seccomp/aarch64-unknown-linux-musl.json" \
    --output-file "$work/seccomp.bpf"
if [[ ! -s "$work/kernel" ]]; then
    curl --fail --location --silent --show-error "$kernel_url" --output "$work/kernel"
fi
echo "$kernel_sha256  $work/kernel" | sha256sum --check --status
GUEST
}
