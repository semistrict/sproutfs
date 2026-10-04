#!/usr/bin/env bash
# How long a VM takes to start on disposable GCE hosts: a cold start, a restore,
# a fork on the same host and a fork to another host, each from the request to
# the guest running and to its agent answering, split into the steps each host
# logs (docs/measurements/gce-start-latency-2026-10-04.md).
#
# The hosts are one k3s cluster, one node per host pod, each with nested
# virtualization and one local NVMe SSD under its kubelet, running deploy/ as
# scripts/lib/app-restore-manifest.py adapts it, with the alpine template. The
# driver is cmd/sproutfs-startbench, built here for linux/amd64 and run on the
# first node by scripts/lib/start-run.sh.
#
#   create   the nodes, which join one cluster as they boot
#   build    the container image and the alpine guest image on the first node,
#            copied to every other node through the bucket, and the driver
#   deploy   deploy/ at the bench's settings
#   run      the starts, on the first node; results are copied back
#   summary  the report's tables, from the results
#   delete   every node, its disks and the run's objects, verified
#   all      every step in order, and delete whatever happens
#
# SPROUTFS_GCE_BUCKET names the bucket and SPROUTFS_GCE_SERVICE_ACCOUNT the
# account the nodes reach it as. SPROUTFS_START_HOSTS (2), SPROUTFS_START_MACHINE
# (n2-highmem-4), SPROUTFS_APP_PLATFORM (Intel Ice Lake) and
# SPROUTFS_START_GUEST_BYTES (1 GiB) shape the cluster; START_BENCH_COUNT,
# START_BENCH_CASES and START_BENCH_INTERVAL of start-run.sh shape the run and
# are passed through. A worktree whose Firecracker submodule is not checked out
# names a clone of the fork at the pinned commit in SPROUTFS_FIRECRACKER_TREE
# (scripts/lib/stage-source.py).
#
#   scripts/bench-start-gce.sh all [name-prefix] [results-directory]
set -euo pipefail
hosts=${SPROUTFS_START_HOSTS:-2}
machine=${SPROUTFS_START_MACHINE:-n2-highmem-4}
platform=${SPROUTFS_APP_PLATFORM:-Intel Ice Lake}
guest_bytes=${SPROUTFS_START_GUEST_BYTES:-1073741824}
if [[ ! $hosts =~ ^[0-9]+$ ]] || ((hosts < 2)); then echo "SPROUTFS_START_HOSTS is at least 2" >&2; exit 2; fi
[[ $machine =~ ^n2-(standard|highmem)-[0-9]+$ ]] || { echo "SPROUTFS_START_MACHINE is an n2 machine type" >&2; exit 2; }
[[ $platform =~ ^Intel\ [A-Za-z\ ]+$ ]] || { echo "SPROUTFS_APP_PLATFORM is an Intel CPU platform" >&2; exit 2; }
[[ $guest_bytes =~ ^[0-9]+$ ]] || { echo "SPROUTFS_START_GUEST_BYTES is a number" >&2; exit 2; }
bucket=${SPROUTFS_GCE_BUCKET:-}
account=${SPROUTFS_GCE_SERVICE_ACCOUNT:-}
[[ $bucket =~ ^[a-z0-9._-]+$ && $account =~ ^[a-z0-9@._-]+$ ]] ||
    { echo "Set SPROUTFS_GCE_BUCKET and SPROUTFS_GCE_SERVICE_ACCOUNT." >&2; exit 2; }
[[ ${START_BENCH_COUNT:-} =~ ^[0-9]*$ ]] || { echo "START_BENCH_COUNT is a number" >&2; exit 2; }
[[ ${START_BENCH_CASES:-} =~ ^[a-z,-]*$ ]] || { echo "START_BENCH_CASES names cases, separated by commas" >&2; exit 2; }
[[ ${START_BENCH_INTERVAL:-} =~ ^([0-9]+(ms|s))?$ ]] || { echo "START_BENCH_INTERVAL is a duration" >&2; exit 2; }

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
project=${SPROUTFS_GCE_PROJECT:-$(gcloud config get-value project 2>/dev/null)}
zone=${SPROUTFS_GCE_ZONE:-us-east4-a}
action=${1:-all}
prefix=${2:-sproutfs-startbench-$(date -u +%Y%m%d-%H%M%S)}
results=${3:-$repo/docs/measurements/gce-start-latency-$(date -u +%Y-%m-%d)}
[[ -n "$project" && "$project" != '(unset)' ]] || { echo 'Set SPROUTFS_GCE_PROJECT.' >&2; exit 2; }
[[ "$prefix" =~ ^sproutfs-startbench-[a-z0-9-]+$ ]] || { echo 'Use a sproutfs-startbench- name prefix.' >&2; exit 2; }
cloud=(gcloud --quiet --project="$project")
nodes=()
for ((index = 0; index < hosts; index++)); do nodes+=("$prefix-$index"); done
server=${nodes[0]}
objects="sproutfs-bench/$prefix"
kube='export KUBECONFIG=/etc/rancher/k3s/k3s.yaml'
# Both arenas and nothing else come out of the pool: 10 GiB of 2 MiB pages.
hugepages=5120

create() {
    local token labels=purpose=start-bench,lifecycle=temporary
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
    [[ "$labels" == $'start-bench\ttemporary' ]] || {
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
            for node in "${nodes[@]}"; do
                printf '%s: %s, %s processors\n' "$node" \
                    "$(remote "$node" "grep -m1 '^model name' /proc/cpuinfo | cut -d: -f2-" | xargs)" \
                    "$(remote "$node" 'nproc')"
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
    staging=$(mktemp -d "${TMPDIR:-/tmp}/sproutfs-startbench.XXXXXX")
    python3 "$repo/scripts/lib/stage-source.py" "$repo" "$staging/repo.tar.gz"
    tar -xzOf "$staging/repo.tar.gz" repo/third_party/firecracker/Cargo.toml > /dev/null || {
        echo 'The staged source has no Firecracker: check out the submodule or set SPROUTFS_FIRECRACKER_TREE.' >&2
        rm -r -- "$staging"
        return 1
    }
    (cd "$repo" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$staging/sproutfs-startbench" \
        ./cmd/sproutfs-startbench)
    {
        echo "revision $(git -C "$repo" rev-parse HEAD)"
        git -C "$repo" status --porcelain=v1 | sed 's/^/changed /'
        echo "machine $machine ($platform), hosts $hosts, guest $guest_bytes bytes, objects gs://$bucket/$objects"
    } > "$results/source.txt"
    "${cloud[@]}" compute scp --zone="$zone" "$staging/repo.tar.gz" "$staging/sproutfs-startbench" \
        "$repo/scripts/lib/demo-image.sh" "$repo/scripts/lib/start-run.sh" "$server:"
    rm -r -- "$staging"
    # The Rust build runs for tens of minutes; the log stays on the node.
    remote "$server" "set -euo pipefail
        chmod 0755 sproutfs-startbench
        sudo install -d -m 0755 /opt/sproutfs-demo
        sudo rm -rf /opt/sproutfs-demo/repo
        sudo tar -xzf repo.tar.gz -C /opt/sproutfs-demo
        sudo SPROUTFS_DEMO_GUEST_IMAGES=alpine bash demo-image.sh /opt/sproutfs-demo sproutfs:demo \
            > /tmp/demo-image.log 2>&1 || { tail -60 /tmp/demo-image.log; exit 1; }
        tail -5 /tmp/demo-image.log
        sudo docker save sproutfs:demo -o /opt/sproutfs-demo/image.tar
        sudo gcloud storage cp /opt/sproutfs-demo/image.tar /opt/sproutfs-demo/guest/guest.ext4 \
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
            sudo gcloud storage cp gs://$bucket/$objects/artifacts/guest.ext4 /opt/sproutfs-demo/guest/guest.ext4
            sudo k3s ctr -n k8s.io images import /tmp/image.tar
            sudo rm -f /tmp/image.tar
            sha256sum /opt/sproutfs-demo/guest/guest.ext4" > "$results/import-$node.log" 2>&1 &
        waiting+=($!)
    done
    for node in "${waiting[@]}"; do wait "$node" || status=1; done
    ((status == 0)) || { echo "An import failed; see $results/import-*.log." >&2; return 1; }
    remote "$server" 'sha256sum /opt/sproutfs-demo/guest/guest.ext4' >> "$results/build.log"
}

deploy() {
    local staging token
    staging=$(mktemp -d "${TMPDIR:-/tmp}/sproutfs-startbench.XXXXXX")
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
    # The code a small deployment sets for its size (docs/hosting.md, "The code").
    local code=1+1 cidr
    ((hosts < 3)) || code=2+1
    ((hosts < 6)) || code=4+2
    cidr=$("${cloud[@]}" compute networks subnets describe "${SPROUTFS_GCE_NETWORK:-default}" \
        --region="${zone%-*}" --format='value(ipCidrRange)')
    remote "$server" "set -euo pipefail
        $kube
        kubectl apply -f deploy/00-namespace.yaml -f deploy/05-config.yaml
        kubectl create --dry-run=client -o json -f deploy/10-host.yaml -f deploy/20-orchestrator.yaml \
            -f deploy/30-networkpolicy.yaml |
            python3 deploy/app-restore-manifest.py --hosts $hosts --share 100 --guest-bytes $guest_bytes --code $code \
                --template alpine=/usr/share/sproutfs/guest/guest.ext4 --node-cidr $cidr |
            kubectl apply -f -
        rm -rf ~/deploy
        kubectl rollout status -n sproutfs deployment/sproutfs-orchestrator --timeout=300s
        kubectl rollout status -n sproutfs deployment/sproutfs-host --timeout=900s
        kubectl get -n sproutfs pods -o wide" | tee "$results/deploy.log"
}

run() {
    mkdir -p "$results"
    [[ ! -e $results/sproutfs-start ]] ||
        { echo "$results already holds a run; name another directory." >&2; return 1; }
    local settings=() setting
    for setting in START_BENCH_COUNT START_BENCH_CASES START_BENCH_INTERVAL; do
        [[ -z ${!setting:-} ]] || settings+=("$setting=${!setting}")
    done
    # The starts run for an hour or more, longer than one ssh session should be
    # trusted with, so they run on the node on their own and this polls.
    remote "$server" "rm -rf /tmp/sproutfs-start /tmp/start-run.log /tmp/start-run.done
        setsid bash -c 'env ${settings[*]:-} bash start-run.sh $bucket $objects > /tmp/start-run.log 2>&1;
            echo \$? > /tmp/start-run.done' < /dev/null > /dev/null 2>&1 &"
    local status=''
    while [[ -z $status ]]; do
        sleep 60
        status=$(remote "$server" 'cat /tmp/start-run.done 2>/dev/null' 2>/dev/null || true)
        # A node deleted under the run never answers again.
        [[ -n $status ]] || existing | grep -qx "$server" ||
            { echo "$server is gone; the run cannot end." >&2; return 1; }
        remote "$server" 'tail -2 /tmp/start-run.log; wc -l < /tmp/sproutfs-start/samples.jsonl 2>/dev/null' \
            2>/dev/null | sed 's/^/  /' || true
    done
    remote "$server" 'cat /tmp/start-run.log' > "$results/run.log" 2>&1 || true
    remote "$server" 'tar -czf /tmp/sproutfs-start.tar.gz -C /tmp sproutfs-start' || true
    "${cloud[@]}" compute scp --zone="$zone" "$server:/tmp/sproutfs-start.tar.gz" "$results/" || true
    tar -xzf "$results/sproutfs-start.tar.gz" -C "$results" && rm -f "$results/sproutfs-start.tar.gz"
    # kubectl logs prints only a container's current log file, and the polling
    # of the agents fills one every few minutes. The node keeps the files it
    # rotated, so every host's whole log is read from its node.
    local node
    for node in "${nodes[@]}"; do
        # shellcheck disable=SC2016 # expanded by the node's shell
        remote "$node" 'sudo sh -c "for f in /var/log/pods/sproutfs_sproutfs-host-*/host/0.log*; do
            case \$f in *.gz) zcat \$f ;; *) cat \$f ;; esac; done"' > "$results/sproutfs-start/$node.log"
    done
    [[ $status == 0 ]] || { echo "The run failed; see $results/run.log." >&2; return 1; }
    summary
}

summary() {
    # The nodes' files hold everything pods.log does, and more.
    local logs='' node
    for node in "${nodes[@]}"; do logs+="${logs:+,}$results/sproutfs-start/$node.log"; done
    (cd "$repo" && go run ./cmd/sproutfs-startbench summary -samples "$results/sproutfs-start/samples.jsonl" \
        -logs "$logs" -store "$results/sproutfs-start/store.json") |
        tee "$results/summary.md"
}

case "$action" in
    create) create ;;
    build) build ;;
    deploy) deploy ;;
    run) run ;;
    summary) summary ;;
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
    *) echo "Usage: $0 [all|create|ready|build|deploy|run|summary|delete] [name-prefix] [results-directory]" >&2; exit 2 ;;
esac
