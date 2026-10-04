#!/usr/bin/env bash
# A real application restored on disposable GCE hosts, with its memory read
# from the cluster's disk cache, from the object store, or from the memory of
# the host that suspended it (docs/measurements/gce-real-app-restore-2026-10-03.md).
#
# The hosts are one k3s cluster, one node per host pod, each with nested
# virtualization and one local NVMe SSD under its kubelet, running deploy/ as
# scripts/lib/app-restore-manifest.py adapts it. The guest is the valkey
# template: Valkey loaded by cmd/sproutfs-guest-chase with data whose reads
# depend on each other, suspended, restored, and walked one request at a time
# (scripts/lib/app-restore-run.sh).
#
#   create   the nodes, which join one cluster as they boot
#   build    the container image and the valkey guest image on the first node,
#            copied to every other node through the bucket
#   deploy   deploy/ at the bench's settings
#   run      the rounds, on the first node; results are copied back
#   delete   every node, its disks and the run's objects, verified
#   all      every step in order, and delete whatever happens
#
# SPROUTFS_GCE_BUCKET names the bucket and SPROUTFS_GCE_SERVICE_ACCOUNT the
# account the nodes reach it as. SPROUTFS_APP_HOSTS (6), SPROUTFS_APP_MACHINE
# (n2-highmem-4), SPROUTFS_APP_PLATFORM (Intel Ice Lake, whose processors have
# SHA instructions; the run of 2026-10-03 was on Intel Cascade Lake, which has
# none) and SPROUTFS_APP_GUEST_BYTES (8 GiB) shape the cluster; the
# APP_RESTORE_* settings of app-restore-run.sh shape the data and the rounds
# and are passed through. A worktree whose Firecracker submodule is not checked
# out names a clone of the fork at the pinned commit in
# SPROUTFS_FIRECRACKER_TREE (scripts/lib/stage-source.py).
#
#   scripts/bench-app-restore-gce.sh all [name-prefix] [results-directory]
set -euo pipefail
hosts=${SPROUTFS_APP_HOSTS:-6}
machine=${SPROUTFS_APP_MACHINE:-n2-highmem-4}
platform=${SPROUTFS_APP_PLATFORM:-Intel Ice Lake}
guest_bytes=${SPROUTFS_APP_GUEST_BYTES:-8589934592}
if [[ ! $hosts =~ ^[0-9]+$ ]] || ((hosts < 2)); then echo "SPROUTFS_APP_HOSTS is at least 2" >&2; exit 2; fi
[[ $machine =~ ^n2-(standard|highmem)-[0-9]+$ ]] || { echo "SPROUTFS_APP_MACHINE is an n2 machine type" >&2; exit 2; }
[[ $platform =~ ^Intel\ [A-Za-z\ ]+$ ]] || { echo "SPROUTFS_APP_PLATFORM is an Intel CPU platform" >&2; exit 2; }
[[ $guest_bytes =~ ^[0-9]+$ ]] || { echo "SPROUTFS_APP_GUEST_BYTES is a number" >&2; exit 2; }
bucket=${SPROUTFS_GCE_BUCKET:-}
account=${SPROUTFS_GCE_SERVICE_ACCOUNT:-}
[[ $bucket =~ ^[a-z0-9._-]+$ && $account =~ ^[a-z0-9@._-]+$ ]] ||
    { echo "Set SPROUTFS_GCE_BUCKET and SPROUTFS_GCE_SERVICE_ACCOUNT." >&2; exit 2; }
for setting in APP_RESTORE_ROUNDS APP_RESTORE_KEYS APP_RESTORE_MEMBERS APP_RESTORE_VALUE \
    APP_RESTORE_STEPS APP_RESTORE_SCAN APP_RESTORE_SETTLE APP_RESTORE_SEED; do
    [[ ${!setting:-} =~ ^[0-9]*$ ]] || { echo "$setting is a number" >&2; exit 2; }
done
[[ ${APP_RESTORE_CASES:-} =~ ^[a-z,]*$ ]] || { echo "APP_RESTORE_CASES names cases, separated by commas" >&2; exit 2; }

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
project=${SPROUTFS_GCE_PROJECT:-$(gcloud config get-value project 2>/dev/null)}
zone=${SPROUTFS_GCE_ZONE:-us-east4-a}
action=${1:-all}
prefix=${2:-sproutfs-apprestore-$(date -u +%Y%m%d-%H%M%S)}
results=${3:-$repo/docs/measurements/gce-real-app-restore-$(date -u +%Y-%m-%d)}
[[ -n "$project" && "$project" != '(unset)' ]] || { echo 'Set SPROUTFS_GCE_PROJECT.' >&2; exit 2; }
[[ "$prefix" =~ ^sproutfs-apprestore-[a-z0-9-]+$ ]] || { echo 'Use a sproutfs-apprestore- name prefix.' >&2; exit 2; }
cloud=(gcloud --quiet --project="$project")
nodes=()
for ((index = 0; index < hosts; index++)); do nodes+=("$prefix-$index"); done
server=${nodes[0]}
objects="sproutfs-bench/$prefix"
kube='export KUBECONFIG=/etc/rancher/k3s/k3s.yaml'
# Both arenas and nothing else come out of the pool: 10 GiB of 2 MiB pages.
hugepages=5120

create() {
    local token labels=purpose=app-restore-bench,lifecycle=temporary
    token=$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')
    local -a common=(--zone="$zone" --machine-type="$machine"
        --min-cpu-platform="$platform" --enable-nested-virtualization
        --image=ubuntu-2604-resolute-amd64-v20260907 --image-project=ubuntu-os-cloud
        --boot-disk-size=60GB --boot-disk-type=pd-balanced --boot-disk-auto-delete
        --local-ssd=interface=NVME
        --network="${SPROUTFS_GCE_NETWORK:-default}"
        --service-account="$account" --scopes=storage-rw
        --metadata-from-file="startup-script=$repo/scripts/lib/gce-app-restore-startup.sh"
        --labels="$labels"
        --maintenance-policy=TERMINATE --no-restart-on-failure
        --max-run-duration=8h --instance-termination-action=DELETE)
    "${cloud[@]}" compute instances create "$server" "${common[@]}" \
        --metadata="block-project-ssh-keys=TRUE,sproutfs-k3s-role=server,sproutfs-k3s-token=$token,sproutfs-hugepages=$hugepages"
    "${cloud[@]}" compute instances create "${nodes[@]:1}" "${common[@]}" \
        --metadata="block-project-ssh-keys=TRUE,sproutfs-k3s-role=agent,sproutfs-k3s-token=$token,sproutfs-k3s-server=$server,sproutfs-hugepages=$hugepages"
}

existing() {
    "${cloud[@]}" compute instances list --filter="name ~ ^$prefix-[0-9]+\$" --format='value(name)'
}

check_owner() {
    local labels
    labels=$("${cloud[@]}" compute instances describe "$1" --zone="$zone" \
        --format='value(labels.purpose,labels.lifecycle)')
    [[ "$labels" == $'app-restore-bench\ttemporary' ]] || {
        echo "Refusing to operate on $1, which lacks the benchmark ownership labels." >&2
        return 1
    }
}

delete() {
    local name left
    local -a names=()
    while read -r name; do
        [[ -n $name ]] || continue
        check_owner "$name"
        names+=("$name")
    done < <(existing)
    if ((${#names[@]} > 0)); then
        "${cloud[@]}" compute instances delete "${names[@]}" --zone="$zone" --delete-disks=all
    fi
    left=$(existing)
    [[ -z "$left" ]] || { echo "Nodes still exist: $left" >&2; return 1; }
    left=$("${cloud[@]}" compute disks list --filter="name ~ ^$prefix-[0-9]+\$" --format='value(name)')
    [[ -z "$left" ]] || { echo "Disks still exist: $left" >&2; return 1; }
    "${cloud[@]}" storage rm --recursive "gs://$bucket/$objects/" > /dev/null 2>&1 || true
    left=$("${cloud[@]}" storage ls "gs://$bucket/$objects/" 2>/dev/null || true)
    [[ -z "$left" ]] || { echo "Objects still exist under gs://$bucket/$objects/" >&2; return 1; }
    echo "Verified deletion of every $prefix node, its disks and its objects." >&2
}

# remote runs a command on one node and touches its lease. Keepalives, because
# the build holds one session open for the length of a Rust build.
remote() {
    local node=$1
    shift
    "${cloud[@]}" compute ssh "$node" --zone="$zone" \
        --ssh-flag='-o ConnectTimeout=15' --ssh-flag='-o ServerAliveInterval=60' \
        --ssh-flag='-o ServerAliveCountMax=10' \
        --command="sudo touch /var/lib/sproutfs-bench/lease 2>/dev/null; $*" < /dev/null
}

wait_ready() {
    local node ready
    mkdir -p "$results"
    for node in "${nodes[@]}"; do
        check_owner "$node"
        ready=false
        for _ in {1..60}; do
            if remote "$node" 'test -e /var/lib/sproutfs-bench/ready' >> "$results/startup.log" 2>&1; then
                ready=true
                break
            fi
            sleep 10
        done
        "$ready" || {
            "${cloud[@]}" compute instances get-serial-port-output "$node" --zone="$zone" | tail -40 >&2 || true
            echo "$node did not finish starting." >&2
            return 1
        }
    done
    for _ in {1..60}; do
        if remote "$server" "$kube; test \$(kubectl get nodes --no-headers | grep -c ' Ready ') -eq $hosts" \
            >> "$results/startup.log" 2>&1; then
            remote "$server" "$kube; kubectl get nodes -o wide" | tee "$results/nodes.txt"
            # Each node's processor, and whether it has SHA instructions,
            # which the cost of checking a page depends on.
            for node in "${nodes[@]}"; do
                printf '%s: %s, sha_ni %s\n' "$node" \
                    "$(remote "$node" "grep -m1 '^model name' /proc/cpuinfo | cut -d: -f2-" | xargs)" \
                    "$(remote "$node" 'grep -qw sha_ni /proc/cpuinfo && echo yes || echo no')"
            done | tee "$results/cpus.txt"
            return 0
        fi
        sleep 10
    done
    echo "The cluster does not have $hosts ready nodes." >&2
    return 1
}

build() {
    local staging
    mkdir -p "$results"
    staging=$(mktemp -d "${TMPDIR:-/tmp}/sproutfs-apprestore.XXXXXX")
    python3 "$repo/scripts/lib/stage-source.py" "$repo" "$staging/repo.tar.gz"
    tar -xzOf "$staging/repo.tar.gz" repo/third_party/firecracker/Cargo.toml > /dev/null || {
        echo 'The staged source has no Firecracker: check out the submodule or set SPROUTFS_FIRECRACKER_TREE.' >&2
        rm -r -- "$staging"
        return 1
    }
    {
        echo "revision $(git -C "$repo" rev-parse HEAD)"
        git -C "$repo" status --porcelain=v1 | sed 's/^/changed /'
        echo "machine $machine ($platform), hosts $hosts, guest $guest_bytes bytes, objects gs://$bucket/$objects"
    } > "$results/source.txt"
    "${cloud[@]}" compute scp --zone="$zone" "$staging/repo.tar.gz" \
        "$repo/scripts/lib/demo-image.sh" "$server:"
    rm -r -- "$staging"
    # The Rust build runs for tens of minutes; the log stays on the node.
    remote "$server" "set -euo pipefail
        sudo install -d -m 0755 /opt/sproutfs-demo
        sudo rm -rf /opt/sproutfs-demo/repo
        sudo tar -xzf repo.tar.gz -C /opt/sproutfs-demo
        sudo SPROUTFS_DEMO_GUEST_IMAGES=valkey bash demo-image.sh /opt/sproutfs-demo sproutfs:demo \
            > /tmp/demo-image.log 2>&1 || { tail -60 /tmp/demo-image.log; exit 1; }
        tail -5 /tmp/demo-image.log
        sudo docker save sproutfs:demo -o /opt/sproutfs-demo/image.tar
        sudo gcloud storage cp /opt/sproutfs-demo/image.tar /opt/sproutfs-demo/guest/valkey.ext4 \
            gs://$bucket/$objects/artifacts/
        sudo rm -f /opt/sproutfs-demo/image.tar" | tee "$results/build.log"
    # Every other node imports the image and takes the guest image the hosts
    # are configured with, which the template is named by.
    local node status=0
    local -a waiting=()
    for node in "${nodes[@]:1}"; do
        remote "$node" "set -euo pipefail
            sudo install -d -m 0755 /opt/sproutfs-demo/guest
            sudo gcloud storage cp gs://$bucket/$objects/artifacts/image.tar /tmp/image.tar
            sudo gcloud storage cp gs://$bucket/$objects/artifacts/valkey.ext4 /opt/sproutfs-demo/guest/valkey.ext4
            sudo k3s ctr -n k8s.io images import /tmp/image.tar
            sudo rm -f /tmp/image.tar
            sha256sum /opt/sproutfs-demo/guest/valkey.ext4" > "$results/import-$node.log" 2>&1 &
        waiting+=($!)
    done
    for node in "${waiting[@]}"; do wait "$node" || status=1; done
    ((status == 0)) || { echo "An import failed; see $results/import-*.log." >&2; return 1; }
    remote "$server" 'sha256sum /opt/sproutfs-demo/guest/valkey.ext4' >> "$results/build.log"
}

deploy() {
    local staging token
    staging=$(mktemp -d "${TMPDIR:-/tmp}/sproutfs-apprestore.XXXXXX")
    # A second deploy keeps the token the pods already run with.
    token=$(remote "$server" "$kube; kubectl get secret -n sproutfs sproutfs-api-token \
        -o jsonpath='{.data.token}' 2>/dev/null | base64 -d" 2>/dev/null || true)
    [[ -n $token ]] || token=$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')
    cp "$repo"/deploy/*.yaml "$repo/scripts/lib/app-restore-manifest.py" "$staging/"
    cat > "$staging/05-config.yaml" <<CONFIG
apiVersion: v1
kind: ConfigMap
metadata:
  name: sproutfs-demo
  namespace: sproutfs
data:
  bucket: "$bucket"
  prefix: "$objects/store"
  arena: "isolated"
---
apiVersion: v1
kind: Secret
metadata:
  name: sproutfs-api-token
  namespace: sproutfs
type: Opaque
stringData:
  token: "$token"
CONFIG
    remote "$server" 'rm -rf ~/deploy'
    "${cloud[@]}" compute scp --recurse --zone="$zone" "$staging" "$server:deploy"
    rm -r -- "$staging"
    local code=4+2 cidr
    ((hosts >= 6)) || code=2+1
    cidr=$("${cloud[@]}" compute networks subnets describe "${SPROUTFS_GCE_NETWORK:-default}" \
        --region="${zone%-*}" --format='value(ipCidrRange)')
    # The image's tag does not change when build makes it again, so pods that
    # already run are restarted onto it: the hosts first, then the
    # orchestrator.
    remote "$server" "set -euo pipefail
        $kube
        existing=\$(kubectl get deployments -n sproutfs -o name 2>/dev/null || true)
        kubectl apply -f deploy/00-namespace.yaml -f deploy/05-config.yaml
        kubectl create --dry-run=client -o json -f deploy/10-host.yaml -f deploy/20-orchestrator.yaml \
            -f deploy/30-networkpolicy.yaml |
            python3 deploy/app-restore-manifest.py --hosts $hosts --share 100 --guest-bytes $guest_bytes --code $code \
                --node-cidr $cidr |
            kubectl apply -f -
        rm -rf ~/deploy
        if [[ -n \$existing ]]; then
            kubectl rollout restart -n sproutfs deployment/sproutfs-host
            kubectl rollout status -n sproutfs deployment/sproutfs-host --timeout=900s
            kubectl rollout restart -n sproutfs deployment/sproutfs-orchestrator
        fi
        kubectl rollout status -n sproutfs deployment/sproutfs-orchestrator --timeout=300s
        kubectl rollout status -n sproutfs deployment/sproutfs-host --timeout=900s
        kubectl get -n sproutfs pods -o wide" | tee "$results/deploy.log"
}

run() {
    mkdir -p "$results"
    # The rounds' files are unpacked over this directory, so the files of an
    # earlier run there, even one that failed, would be summarised as this
    # run's.
    [[ ! -e $results/sproutfs-app-restore ]] ||
        { echo "$results already holds a run's rounds; name another directory." >&2; return 1; }
    "${cloud[@]}" compute scp --zone="$zone" "$repo/scripts/lib/app-restore-run.sh" "$server:"
    # Every setting was checked above to be digits or case names and commas,
    # so each crosses the remote shell as one word.
    local settings=() setting
    for setting in APP_RESTORE_ROUNDS APP_RESTORE_KEYS APP_RESTORE_MEMBERS APP_RESTORE_VALUE \
        APP_RESTORE_STEPS APP_RESTORE_SCAN APP_RESTORE_SETTLE APP_RESTORE_SEED APP_RESTORE_CASES; do
        [[ -z ${!setting:-} ]] || settings+=("$setting=${!setting}")
    done
    # The rounds run for an hour or more, longer than one ssh session should
    # be trusted with, so they run on the node on their own and this polls.
    # No setting at all leaves the list empty, which macOS's bash 3.2 takes
    # for unbound.
    remote "$server" "rm -rf /tmp/sproutfs-app-restore /tmp/app-restore.log /tmp/app-restore.done
        setsid bash -c 'env ${settings[*]:-} bash app-restore-run.sh > /tmp/app-restore.log 2>&1;
            echo \$? > /tmp/app-restore.done' < /dev/null > /dev/null 2>&1 &"
    local status=''
    while [[ -z $status ]]; do
        sleep 60
        status=$(remote "$server" 'cat /tmp/app-restore.done 2>/dev/null' 2>/dev/null || true)
        remote "$server" 'tail -3 /tmp/app-restore.log' 2>/dev/null | sed 's/^/  /' || true
    done
    remote "$server" 'cat /tmp/app-restore.log' > "$results/run.log" 2>&1 || true
    remote "$server" 'tar -czf /tmp/app-restore.tar.gz -C /tmp sproutfs-app-restore' || true
    "${cloud[@]}" compute scp --zone="$zone" "$server:/tmp/app-restore.tar.gz" "$results/" || true
    tar -xzf "$results/app-restore.tar.gz" -C "$results" && rm -f "$results/app-restore.tar.gz"
    # Every host's log, which holds each receive's post-copy line and the fills.
    remote "$server" "$kube; for pod in \$(kubectl get pods -n sproutfs -o name); do
        echo \"=== \$pod\"; kubectl logs -n sproutfs \$pod --tail=-1; done" > "$results/pods.log" 2>&1 || true
    [[ $status == 0 ]] || { echo "The rounds failed; see $results/run.log." >&2; return 1; }
    python3 "$repo/scripts/lib/app-restore-summary.py" "$results/sproutfs-app-restore" | tee "$results/summary.md"
}

case "$action" in
    create) create ;;
    build) build ;;
    deploy) deploy ;;
    run) run ;;
    delete) delete ;;
    ready) wait_ready ;;
    all)
        [[ -z "$(existing)" ]] || { echo "Nodes named $prefix-* already exist." >&2; exit 1; }
        trap 'status=$?; trap - EXIT; if ! delete; then exit 1; fi; exit "$status"' EXIT
        create
        wait_ready
        build
        deploy
        run
        ;;
    *) echo "Usage: $0 [all|create|ready|build|deploy|run|delete] [name-prefix] [results-directory]" >&2; exit 2 ;;
esac
