# The demo's single container image: the sproutfs host and orchestrator, the
# feature-enabled Firecracker VMM, its compiled seccomp policy and the pinned
# Firecracker CI kernel. Build it from the repository root, because the VMM
# stage needs both the Firecracker submodule and the pager crate it depends on:
#
#     git submodule update --init third_party/firecracker
#     docker build -f deploy/Dockerfile -t sproutfs:demo .
#
# x86_64 only. GCE nested virtualization is x86, so the policy compiled here is
# the x86_64 one and the kernel is the x86_64 CI artifact.

# Stage 1 mirrors scripts/lib/firecracker-build.sh on x86_64: the same pinned
# fork, --features sproutfs-memory, and the policy compiled by the fork's own
# seccompiler-bin. RUSTUP_TOOLCHAIN=stable overrides the fork's checked-in
# toolchain pin exactly as that recipe's default does. The profile is release,
# which is what a deployment wants and what every measurement in docs/ was taken
# with: the recipe's own default, dev, is an unoptimised build carrying debug
# assertions, and the numbers it produces are not this system's.
# --build-arg FIRECRACKER_PROFILE=dev builds that instead, which is faster to
# build and slower to run.
#
# The VMM is built for the musl target because the policy is: the checked-in
# x86_64 policy allows epoll_pwait and not epoll_wait, which is musl's spelling
# of the same wait. A glibc build of the VMM dies on its first poll with
# "intercepting a bad syscall (232)". seccompiler-bin, which only ever runs
# here, stays a host build: it links the distribution's libseccomp.
#
# The packages are what a Lima guest already has and this base image does not:
# musl-tools is the musl C toolchain the cross build links with, libclang-dev
# is what userfaultfd-sys's bindgen loads, cmake is what aws-lc-rs builds with,
# and libseccomp-dev is the libseccomp.so the recipe symlinks by hand.
FROM rust:bookworm AS vmm
ARG FIRECRACKER_PROFILE=release
ENV RUSTUP_TOOLCHAIN=stable CARGO_TARGET_DIR=/target CARGO_TERM_COLOR=never
# musl-gcc compiles with only musl's own headers, and userfaultfd-sys includes
# linux/userfaultfd.h. The kernel's uapi headers belong to no libc, so they are
# added after musl's, where they cannot shadow them.
ENV CFLAGS_x86_64_unknown_linux_musl="-idirafter /usr/include/x86_64-linux-gnu -idirafter /usr/include"
RUN apt-get update \
 && apt-get install --yes --no-install-recommends cmake libclang-dev libseccomp-dev musl-tools \
 && rm -rf /var/lib/apt/lists/* \
 && rustup target add x86_64-unknown-linux-musl
WORKDIR /src
COPY rust rust
COPY third_party/firecracker third_party/firecracker
RUN cargo build --locked --manifest-path third_party/firecracker/Cargo.toml \
        --target x86_64-unknown-linux-musl \
        --profile "$FIRECRACKER_PROFILE" -p firecracker --features sproutfs-memory \
 && cargo build --locked --manifest-path third_party/firecracker/Cargo.toml \
        --profile "$FIRECRACKER_PROFILE" -p seccompiler --bin seccompiler-bin
RUN if [ "$FIRECRACKER_PROFILE" = dev ]; then built=debug; else built=$FIRECRACKER_PROFILE; fi \
 && cp "/target/x86_64-unknown-linux-musl/$built/firecracker" /firecracker \
 && "/target/$built/seccompiler-bin" --target-arch x86_64 \
        --input-file third_party/firecracker/resources/seccomp/x86_64-unknown-linux-musl.json \
        --output-file /seccomp.bpf

# Stage 2: the guest kernel, an official Firecracker CI artifact pinned by URL
# and checksum. It is the x86_64 build of the same CI run and the same kernel
# version that scripts/lib/firecracker-build.sh pins for aarch64.
FROM debian:bookworm-slim AS kernel
ARG KERNEL_URL=https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/20260902-a6146c8bb213-0/x86_64/vmlinux-6.18.44
ARG KERNEL_SHA256=d0fa6b694b32c9d12c5b1575180c888d9c83ff8955d21fe74debb4fe4832db22
RUN apt-get update \
 && apt-get install --yes --no-install-recommends ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*
RUN curl --fail --location --silent --show-error "$KERNEL_URL" --output /vmlinux \
 && echo "$KERNEL_SHA256  /vmlinux" | sha256sum --check --status

# Stage 3: the Go binaries, static so the runtime stage needs no Go runtime and
# no cgo resolver: the host, the orchestrator and the sproutfsctl CLI, which is
# in the image so that `kubectl exec` can drive the demo from inside the
# cluster as well as through a port-forward. The guest agent and the guest
# witness are built here too and carried in this image, not to be run from it:
# scripts/lib/demo-image.sh takes them out and installs them into the guest
# images, which spares the demo node a Go toolchain of its own.
FROM golang:1.26-bookworm AS commands
# SPROUTFS_VERSION is what every binary here reports at /version and logs at
# startup: the build's `git describe`. It is an argument rather than a `git`
# call in this stage because the build context carries no repository.
ARG SPROUTFS_VERSION=dev
ENV CGO_ENABLED=0
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -trimpath -ldflags "-X main.version=$SPROUTFS_VERSION" -o /commands/ ./cmd/...

FROM debian:bookworm-slim
# The VMM is static and the Go binaries are built without cgo, so the only
# thing this stage adds is the root store the object store needs for GCS.
RUN apt-get update \
 && apt-get install --yes --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/*
COPY --from=vmm /firecracker /usr/local/bin/firecracker
COPY --from=vmm /seccomp.bpf /usr/share/sproutfs/seccomp.bpf
COPY --from=kernel /vmlinux /usr/share/sproutfs/vmlinux
COPY --from=commands /commands/sproutfs-host /commands/sproutfs-orchestrator \
     /commands/sproutfsctl /commands/sproutfs-guest-agent \
     /commands/sproutfs-guest-witness /usr/local/bin/
CMD ["/usr/local/bin/sproutfs-host"]
