#!/usr/bin/env bash
# Build the demo's two images on the demo node.
#
# scripts/demo-gce.sh copies this one file to the node and runs it there as
# root, once the node is provisioned:
#
#     sudo bash demo-image.sh /opt/sproutfs-demo sproutfs:demo
#
#   $1  the demo directory on the node. The repository copy lives under it:
#       either the directory itself or one level down, recognised by holding
#       deploy/Dockerfile and scripts/build-guest-image.sh. SPROUTFS_DEMO_REPO
#       names it explicitly instead.
#   $2  the container image tag, sproutfs:demo by default.
#
# When it succeeds the node has:
#   - the tagged image in k3s's containerd under the k8s.io namespace, which is
#     the one kubelet reads, so the pods run it with imagePullPolicy: Never;
#   - the x86_64 guest images at <demo directory>/guest/guest.ext4 and
#     <demo directory>/guest/workload.ext4, the second of which carries git,
#     ripgrep, Node, pnpm and three repositories with their dependencies.
# Any failing step fails the script: there is no partial success to act on.
#
# The node needs a network for the base images, the crates and the kernel. A
# container engine is installed if neither docker nor buildah is present. The
# build is not incremental: each run rebuilds both images from the copy it
# finds.
set -euo pipefail

(($# >= 1 && $# <= 2)) || { echo "Usage: $0 demo-directory [image-tag]" >&2; exit 2; }
demo=$1
tag=${2:-sproutfs:demo}
[[ $(id -u) == 0 ]] || { echo "$0 must run as root on the demo node." >&2; exit 2; }
[[ -d $demo ]] || { echo "No such demo directory: $demo" >&2; exit 2; }
demo=$(cd "$demo" && pwd)

repository() {
    local candidate
    for candidate in "${SPROUTFS_DEMO_REPO:-}" "$demo" "$demo"/*; do
        [[ -n $candidate ]] || continue
        if [[ -f $candidate/deploy/Dockerfile && -f $candidate/scripts/build-guest-image.sh ]]; then
            (cd "$candidate" && pwd)
            return 0
        fi
    done
    return 1
}
repo=$(repository) || {
    echo "No repository copy under $demo: expected one holding deploy/Dockerfile" >&2
    echo "and scripts/build-guest-image.sh, or SPROUTFS_DEMO_REPO naming it." >&2
    exit 1
}
command -v k3s >/dev/null || { echo "k3s is not installed on this node." >&2; exit 1; }

if command -v docker >/dev/null; then
    engine=docker
elif command -v buildah >/dev/null; then
    engine=buildah
else
    echo "installing docker" >&2
    apt-get update -qq
    # NEEDRESTART_SUSPEND keeps Ubuntu's post-install hook from stopping to ask
    # about restarting services, which on this node includes the ssh session
    # this script runs under.
    DEBIAN_FRONTEND=noninteractive NEEDRESTART_SUSPEND=1 apt-get install -y -qq docker.io
    engine=docker
fi
if [[ $engine == docker ]] && command -v systemctl >/dev/null; then
    systemctl is-active --quiet docker || systemctl start docker
fi

archive=$demo/image.tar
cleanup() {
    local status=$?
    rm -f -- "$archive"
    exit $status
}
trap cleanup EXIT

# What the binaries report at /version. A node copy of the repository has its
# Git directory, so this is the build's own description of itself; a copy
# without one is "unknown", which is still an answer.
version=$(git -C "$repo" describe --always --dirty --tags 2>/dev/null || echo unknown)
echo "building $tag from $repo/deploy/Dockerfile with $engine, version $version" >&2
case $engine in
docker)
    docker build --file "$repo/deploy/Dockerfile" --tag "$tag" \
        --build-arg "SPROUTFS_VERSION=$version" "$repo"
    docker save "$tag" --output "$archive"
    ;;
buildah)
    buildah bud --file "$repo/deploy/Dockerfile" --tag "$tag" \
        --build-arg "SPROUTFS_VERSION=$version" "$repo"
    buildah push "$tag" "docker-archive:$archive:$tag"
    ;;
esac

# k3s runs its own containerd; k8s.io is the namespace kubelet resolves images
# in, so an import into any other namespace leaves the pods pulling.
echo "importing $tag into k3s's containerd" >&2
k3s ctr -n k8s.io images import "$archive"
if ! k3s ctr -n k8s.io images list --quiet | grep -q -- "$tag"; then
    echo "Import did not leave $tag in the k8s.io namespace." >&2
    exit 1
fi

# The guest images carry the agent the host reaches its guests through and the
# witness the soak asks a guest with. The container image already holds a static
# build of each and this node has no Go toolchain, so they are taken out of the
# image rather than built again.
agent=$demo/sproutfs-guest-agent
witness=$demo/sproutfs-guest-witness
echo "taking the guest binaries out of $tag" >&2
case $engine in
docker)
    container=$(docker create "$tag")
    docker cp "$container:/usr/local/bin/sproutfs-guest-agent" "$agent"
    docker cp "$container:/usr/local/bin/sproutfs-guest-witness" "$witness"
    docker rm --force "$container" > /dev/null
    ;;
buildah)
    working=$(buildah from "$tag")
    mount=$(buildah mount "$working")
    cp "$mount/usr/local/bin/sproutfs-guest-agent" "$agent"
    cp "$mount/usr/local/bin/sproutfs-guest-witness" "$witness"
    buildah umount "$working" > /dev/null
    buildah rm "$working" > /dev/null
    ;;
esac
[[ -s $agent ]] || { echo "The image holds no guest agent at /usr/local/bin." >&2; exit 1; }
[[ -s $witness ]] || { echo "The image holds no guest witness at /usr/local/bin." >&2; exit 1; }

install -d -m 0755 "$demo/guest"
bash "$repo/scripts/build-guest-image.sh" "$demo/guest/guest.ext4" "$agent" "$witness"

# The workload image carries Node, pnpm and three repositories with their
# dependencies, so building it takes minutes and downloads a few hundred
# megabytes. It does not change when the Go code does, so a redeploy keeps the
# one already on the node; SPROUTFS_DEMO_REBUILD_WORKLOAD=1 builds it again.
workload=$demo/guest/workload.ext4
if [[ -s $workload && ${SPROUTFS_DEMO_REBUILD_WORKLOAD:-0} != 1 ]]; then
    echo "keeping the workload guest image already at $workload" >&2
else
    bash "$repo/scripts/build-guest-image.sh" --template workload "$workload" "$agent" "$witness"
fi
