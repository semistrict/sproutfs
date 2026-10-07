#!/usr/bin/env bash
# Durable flush on disposable GCE hosts: what a guest's flush costs when it is
# answered from a journal on a network disk, what recovery and scaling cost
# around the journal disks, and what the disks themselves take to write and
# sync (plans/fsync-journal-2026-10-06.md, step 9;
# docs/measurements/gce-fsync-journal-2026-10-07.md).
#
#   raw      one c3 machine with a journal-sized disk of each kind attached:
#            fio's writes of 4 to 64 KiB with a sync, one in flight
#            (scripts/lib/fsync-fio.sh), SHA-256's speed, and the pager's
#            capture against real userfaultfd
#            (TestManagedPagerCaptureProtectsAndTrapsOnUFFD); then deleted
#   create   two c3 nodes with nested virtualization, which join one k3s
#            cluster as they boot (scripts/lib/gce-app-restore-startup.sh)
#   build    the container image and the alpine guest image on the first node,
#            copied to the second through the bucket
#   deploy   deploy/ at the bench's settings (scripts/lib/app-restore-manifest.py),
#            with durable flush off
#   measure  three stages, each also an action of its own
#            (scripts/lib/fsync-journal-run.sh runs each step on the first node):
#     measure-flushes  in a guest, flushes and writes with durable flush off,
#                      then on over Hyperdisk Balanced journal disks the
#                      orchestrator creates; the journal's live bytes over two
#                      checkpoint intervals; the second node powered off after
#                      a flush and its VM recovered on the first
#     measure-scale    a node added, a VM flushing on it, and the node drained
#     measure-pds      the flushes again over a pd-ssd journal disk
#   fetch    what the node-side steps wrote, into the results
#   summary  the report's tables (scripts/lib/fsync-summary.py)
#   delete   every machine, disk, journal disk and object, verified
#   all      raw, then every other step in order, and delete whatever happens
#
# SPROUTFS_GCE_BUCKET names the bucket. The nodes run as
# SPROUTFS_GCE_SERVICE_ACCOUNT, the project's default compute account unless it
# is set, with the cloud-platform scope: the orchestrator creates, attaches,
# detaches and deletes the journal disks with the node's own credentials.
# SPROUTFS_FSYNC_MACHINE is the machine type, c3-standard-4 by default. A
# worktree whose Firecracker submodule is not checked out names a clone of the
# fork at the pinned commit in SPROUTFS_FIRECRACKER_TREE
# (scripts/lib/stage-source.py).
#
# The orchestrator labels every journal disk sproutfs-journal=sproutfs, the
# namespace. `all` refuses to start while any disk in the zone carries that
# label, and `delete` deletes every disk that does, so no other deployment may
# run in the zone at the same time.
#
#   scripts/bench-fsync-journal-gce.sh all [name-prefix] [results-directory]
set -euo pipefail
machine=${SPROUTFS_FSYNC_MACHINE:-c3-standard-4}
[[ $machine =~ ^c3-(standard|highmem)-[0-9]+$ ]] || { echo "SPROUTFS_FSYNC_MACHINE is a c3 machine type" >&2; exit 2; }
bucket=${SPROUTFS_GCE_BUCKET:-}
[[ $bucket =~ ^[a-z0-9._-]+$ ]] || { echo "Set SPROUTFS_GCE_BUCKET." >&2; exit 2; }

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
project=${SPROUTFS_GCE_PROJECT:-$(gcloud config get-value project 2>/dev/null)}
zone=${SPROUTFS_GCE_ZONE:-us-east4-a}
action=${1:-all}
prefix=${2:-sproutfs-fsync-$(date -u +%Y%m%d-%H%M%S)}
results=${3:-$repo/docs/measurements/gce-fsync-journal-$(date -u +%Y-%m-%d)}
[[ -n "$project" && "$project" != '(unset)' ]] || { echo 'Set SPROUTFS_GCE_PROJECT.' >&2; exit 2; }
[[ "$prefix" =~ ^sproutfs-fsync-[a-z0-9-]+$ && ${#prefix} -le 45 ]] ||
    { echo 'Use a sproutfs-fsync- name prefix of at most 45 characters.' >&2; exit 2; }
cloud=(gcloud --quiet --project="$project")
account=${SPROUTFS_GCE_SERVICE_ACCOUNT:-$("${cloud[@]}" projects describe "$project" \
    --format='value(projectNumber)')-compute@developer.gserviceaccount.com}
[[ $account =~ ^[a-z0-9@._-]+$ ]] || { echo "SPROUTFS_GCE_SERVICE_ACCOUNT is a service account email" >&2; exit 2; }
labels=purpose=fsync-journal-bench,lifecycle=temporary
nodes=("$prefix-0" "$prefix-1")
joiner=$prefix-2
server=${nodes[0]}
raw=$prefix-raw
objects="sproutfs-bench/$prefix"
journal_label=sproutfs-journal=sproutfs
kube='export KUBECONFIG=/etc/rancher/k3s/k3s.yaml'
# Both arenas, 3 GiB, come out of the pool: 1536 pages of 2 MiB.
hugepages=1536

# --- the cloud's side ------------------------------------------------------

check_owner() {
    local kind=$1 name=$2 found
    found=$("${cloud[@]}" compute "$kind" describe "$name" --zone="$zone" \
        --format='value(labels.purpose,labels.lifecycle)')
    [[ "$found" == $'fsync-journal-bench\ttemporary' ]] || {
        echo "Refusing to operate on $name, which lacks the benchmark ownership labels." >&2
        return 1
    }
}

existing() {
    "${cloud[@]}" compute instances list --filter="name ~ ^$prefix-" --format='value(name)'
}

existing_disks() {
    "${cloud[@]}" compute disks list --filter="name ~ ^$prefix-" --format='value(name)'
}

# journal_disks is every disk in the zone the orchestrator labels as a journal
# disk of this namespace.
journal_disks() {
    "${cloud[@]}" compute disks list --filter="labels.sproutfs-journal=sproutfs AND zone:$zone" \
        --format='value(name)'
}

delete() {
    local name left status=0
    local -a names=() disks=()
    while read -r name; do
        [[ -n $name ]] || continue
        check_owner instances "$name" || { status=1; continue; }
        names+=("$name")
    done < <(existing)
    if ((${#names[@]} > 0)); then
        "${cloud[@]}" compute instances delete "${names[@]}" --zone="$zone" --delete-disks=all || status=1
    fi
    # A journal disk is never deleted with its machine: the orchestrator
    # attaches it with auto-delete off. Once the machines are gone, nothing
    # holds it.
    while read -r name; do
        [[ -n $name ]] && disks+=("$name")
    done < <(existing_disks; journal_disks)
    if ((${#disks[@]} > 0)); then
        "${cloud[@]}" compute disks delete "${disks[@]}" --zone="$zone" || status=1
    fi
    "${cloud[@]}" storage rm --recursive "gs://$bucket/$objects/" > /dev/null 2>&1 || true
    left=$(existing)
    [[ -z "$left" ]] || { echo "Machines still exist: $left" >&2; status=1; }
    left=$(existing_disks; journal_disks)
    [[ -z "$left" ]] || { echo "Disks still exist: $left" >&2; status=1; }
    left=$("${cloud[@]}" storage ls "gs://$bucket/$objects/" 2>/dev/null || true)
    [[ -z "$left" ]] || { echo "Objects still exist under gs://$bucket/$objects/" >&2; status=1; }
    ((status == 0)) && echo "Verified deletion of every $prefix machine, its disks, the journal disks and its objects." >&2
    return "$status"
}

# remote runs a command on one machine and touches its lease. Keepalives,
# because the build holds one session open for the length of a Rust build.
remote() {
    local node=$1
    shift
    "${cloud[@]}" compute ssh "$node" --zone="$zone" \
        --ssh-flag='-o ConnectTimeout=15' --ssh-flag='-o ServerAliveInterval=60' \
        --ssh-flag='-o ServerAliveCountMax=10' \
        --command="sudo touch /var/lib/sproutfs-bench/lease 2>/dev/null; $*" < /dev/null
}

# operations is every Compute Engine operation on the journal disks and on
# this run's machines since $1, as JSON: what each call took from its insert
# to its end.
operations() {
    "${cloud[@]}" compute operations list --zones="$zone" \
        --filter="insertTime >= \"$1\" AND (targetLink ~ /instances/$prefix- OR targetLink ~ /disks/sproutfs-journal-)" \
        --format='json(operationType,targetLink.basename(),status,insertTime,startTime,endTime,error)'
}

wait_started() {
    local node=$1 ready=false
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
}

# --- the raw disks ---------------------------------------------------------

raw() {
    local out=$results/raw staging status=0 type short
    mkdir -p "$out"
    "${cloud[@]}" compute instances create "$raw" --zone="$zone" --machine-type="$machine" \
        --enable-nested-virtualization \
        --image=ubuntu-2604-resolute-amd64-v20260907 --image-project=ubuntu-os-cloud \
        --boot-disk-size=30GB --boot-disk-type=pd-balanced --boot-disk-auto-delete \
        --network="${SPROUTFS_GCE_NETWORK:-default}" --no-service-account --no-scopes \
        --metadata=block-project-ssh-keys=TRUE \
        --metadata-from-file="startup-script=$repo/scripts/lib/gce-bench-startup.sh" \
        --labels="$labels" \
        --maintenance-policy=TERMINATE --no-restart-on-failure \
        --max-run-duration=3h --instance-termination-action=DELETE
    # A journal disk as the orchestrator makes one: 32 GiB, and Hyperdisk
    # Balanced at 6000 IOPS and 400 MB/s.
    "${cloud[@]}" compute disks create "$raw-hdb" --zone="$zone" --type=hyperdisk-balanced --size=32GB \
        --provisioned-iops=6000 --provisioned-throughput=400 --labels="$labels"
    "${cloud[@]}" compute disks create "$raw-pds" --zone="$zone" --type=pd-ssd --size=32GB --labels="$labels"
    for short in hdb pds; do
        "${cloud[@]}" compute instances attach-disk "$raw" --zone="$zone" --disk="$raw-$short" \
            --device-name="$raw-$short"
        "${cloud[@]}" compute disks describe "$raw-$short" --zone="$zone" \
            --format='json(name,type.basename(),sizeGb,provisionedIops,provisionedThroughput)' > "$out/disk-$short.json"
    done
    wait_started "$raw"
    "${cloud[@]}" compute instances describe "$raw" --zone="$zone" \
        --format='json(name,zone,machineType,cpuPlatform)' > "$out/instance.json"
    staging=$(mktemp -d "${TMPDIR:-/tmp}/sproutfs-fsync.XXXXXX")
    (cd "$repo" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c ./vmmemory -o "$staging/vmmemory.test")
    git -C "$repo" archive --format=tar.gz -o "$staging/memory-crate.tar.gz" HEAD rust/sproutfs-vm-memory
    "${cloud[@]}" compute scp --zone="$zone" "$repo/scripts/lib/fsync-fio.sh" "$staging/vmmemory.test" \
        "$staging/memory-crate.tar.gz" "$raw:"
    rm -r -- "$staging"
    for type in hdb pds; do
        remote "$raw" "bash fsync-fio.sh $raw-$type fio-$type" > "$out/fio-$type.log" 2>&1 || status=1
    done
    remote "$raw" 'tar -czf fio.tar.gz fio-hdb fio-pds' >> "$out/remote.log" 2>&1 || status=1
    "${cloud[@]}" compute scp --zone="$zone" "$raw:fio.tar.gz" "$out/" >> "$out/remote.log" 2>&1 &&
        tar -xzf "$out/fio.tar.gz" -C "$out" && rm -f "$out/fio.tar.gz" || status=1
    # SHA-256 of a block and of a 2 MiB page on this processor, as Go's own
    # hashing uses the same instructions OpenSSL does.
    remote "$raw" 'grep -m1 -o -w sha_ni /proc/cpuinfo || echo no sha_ni; \
        openssl speed -seconds 3 -bytes 4096 -evp sha256 2>/dev/null | tail -2; \
        openssl speed -seconds 3 -bytes 2097152 -evp sha256 2>/dev/null | tail -2' > "$out/sha256.txt" 2>&1 || status=1
    # The pager's capture under real userfaultfd, on 4 KiB pages and on 2 MiB
    # HugeTLB pages, with the memory crate's client as the VMM.
    remote "$raw" "set -euo pipefail
        sudo sysctl -q vm.nr_hugepages=512
        sudo DEBIAN_FRONTEND=noninteractive apt-get -qq update
        sudo DEBIAN_FRONTEND=noninteractive apt-get -qq install -y cargo > /dev/null
        cargo --version
        rm -rf crate && mkdir crate && tar -xzf memory-crate.tar.gz -C crate
        cargo build -q --locked --manifest-path crate/rust/sproutfs-vm-memory/Cargo.toml --examples
        chmod +x vmmemory.test
        sudo env SPROUTFS_VM_MEMORY_CLIENT=\$HOME/crate/rust/sproutfs-vm-memory/target/debug/examples/client \
            ./vmmemory.test -test.run '^TestManagedPagerCaptureProtectsAndTrapsOnUFFD\$' -test.count=1 -test.v \
            -test.timeout=5m" > "$out/uffd-capture.log" 2>&1 || status=1
    "${cloud[@]}" compute instances delete "$raw" --zone="$zone" --delete-disks=all
    "${cloud[@]}" compute disks delete "$raw-hdb" "$raw-pds" --zone="$zone" 2>/dev/null || true
    return "$status"
}

# --- the cluster -----------------------------------------------------------

# make_node creates one node of the cluster: the server, with a new token, or
# an agent that joins it with the token the server holds in its metadata.
make_node() {
    local node=$1 role=$2 token
    if [[ $role == server ]]; then
        token=$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')
    else
        token=$("${cloud[@]}" compute instances describe "$server" --zone="$zone" \
            --format='value(metadata.items.sproutfs-k3s-token)')
        [[ -n $token ]] || { echo "$server holds no cluster token." >&2; return 1; }
    fi
    local metadata="block-project-ssh-keys=TRUE,sproutfs-k3s-role=$role,sproutfs-k3s-token=$token"
    metadata+=",sproutfs-hugepages=$hugepages,sproutfs-kubelet-disk=boot"
    [[ $role == server ]] || metadata+=",sproutfs-k3s-server=$server"
    "${cloud[@]}" compute instances create "$node" --zone="$zone" --machine-type="$machine" \
        --enable-nested-virtualization \
        --image=ubuntu-2604-resolute-amd64-v20260907 --image-project=ubuntu-os-cloud \
        --boot-disk-size=100GB --boot-disk-type=pd-balanced --boot-disk-auto-delete \
        --network="${SPROUTFS_GCE_NETWORK:-default}" \
        --service-account="$account" --scopes=cloud-platform \
        --metadata="$metadata" \
        --metadata-from-file="startup-script=$repo/scripts/lib/gce-app-restore-startup.sh" \
        --labels="$labels" \
        --maintenance-policy=TERMINATE --no-restart-on-failure \
        --max-run-duration=6h --instance-termination-action=DELETE
}

create() {
    mkdir -p "$results"
    make_node "$server" server
    make_node "${nodes[1]}" agent
}

# wait_cluster waits until each node named has started and the cluster lists
# every one of them ready.
wait_cluster() {
    local node
    for node in "$@"; do
        check_owner instances "$node"
        wait_started "$node"
    done
    for _ in {1..60}; do
        if remote "$server" "$kube; for n in $*; do kubectl get node \$n --no-headers | grep -q ' Ready '; done" \
            >> "$results/startup.log" 2>&1; then
            remote "$server" "$kube; kubectl get nodes -o wide" > "$results/nodes.txt"
            return 0
        fi
        sleep 10
    done
    echo "The cluster does not list $* ready." >&2
    return 1
}

build() {
    local staging
    mkdir -p "$results"
    staging=$(mktemp -d "${TMPDIR:-/tmp}/sproutfs-fsync.XXXXXX")
    python3 "$repo/scripts/lib/stage-source.py" "$repo" "$staging/repo.tar.gz"
    tar -xzOf "$staging/repo.tar.gz" repo/third_party/firecracker/Cargo.toml > /dev/null || {
        echo 'The staged source has no Firecracker: check out the submodule or set SPROUTFS_FIRECRACKER_TREE.' >&2
        rm -r -- "$staging"
        return 1
    }
    {
        echo "revision $(git -C "$repo" rev-parse HEAD)"
        git -C "$repo" status --porcelain=v1 | sed 's/^/changed /'
        echo "machine $machine, zone $zone, objects gs://$bucket/$objects"
    } > "$results/source.txt"
    "${cloud[@]}" compute scp --zone="$zone" "$staging/repo.tar.gz" "$repo/scripts/lib/demo-image.sh" \
        "$repo/scripts/lib/fsync-journal-run.sh" "$server:"
    rm -r -- "$staging"
    # The Rust build runs for tens of minutes; the log stays on the node.
    remote "$server" "set -euo pipefail
        sudo install -d -m 0755 /opt/sproutfs-demo
        sudo rm -rf /opt/sproutfs-demo/repo
        sudo tar -xzf repo.tar.gz -C /opt/sproutfs-demo
        sudo SPROUTFS_DEMO_GUEST_IMAGES=alpine bash demo-image.sh /opt/sproutfs-demo sproutfs:demo \
            > /tmp/demo-image.log 2>&1 || { tail -60 /tmp/demo-image.log; exit 1; }
        tail -5 /tmp/demo-image.log
        sudo docker save sproutfs:demo -o /opt/sproutfs-demo/image.tar
        sudo gcloud storage cp /opt/sproutfs-demo/image.tar /opt/sproutfs-demo/guest/guest.ext4 \
            gs://$bucket/$objects/artifacts/
        sudo rm -f /opt/sproutfs-demo/image.tar" > "$results/build.log" 2>&1
    import "${nodes[1]}"
}

# import gives a node the container image and the guest image the first node
# built.
import() {
    remote "$1" "set -euo pipefail
        sudo install -d -m 0755 /opt/sproutfs-demo/guest
        sudo gcloud storage cp gs://$bucket/$objects/artifacts/image.tar /tmp/image.tar
        sudo gcloud storage cp gs://$bucket/$objects/artifacts/guest.ext4 /opt/sproutfs-demo/guest/guest.ext4
        sudo k3s ctr -n k8s.io images import /tmp/image.tar
        sudo rm -f /tmp/image.tar
        sha256sum /opt/sproutfs-demo/guest/guest.ext4" > "$results/import-$1.log" 2>&1
}

# deploy applies deploy/ with the store under $1 and durable flush $2, on or
# off, for $3 host pods, and waits for them.
deploy() {
    local store=$1 mode=$2 replicas=$3 staging token flush=''
    [[ $mode == on ]] && flush='durable_flush: "gce"'
    staging=$(mktemp -d "${TMPDIR:-/tmp}/sproutfs-fsync.XXXXXX")
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
  prefix: "$objects/$store"
  arena: "isolated"
  $flush
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
    "${cloud[@]}" compute scp --recurse --zone="$zone" "$staging" "$server:deploy" > /dev/null
    rm -r -- "$staging"
    local cidr
    cidr=$("${cloud[@]}" compute networks subnets describe "${SPROUTFS_GCE_NETWORK:-default}" \
        --region="${zone%-*}" --format='value(ipCidrRange)')
    # The ConfigMap is replaced whole, so a durable_flush key a run before
    # set goes with it; the pods read it as they start.
    remote "$server" "set -euo pipefail
        $kube
        kubectl apply -f deploy/00-namespace.yaml
        kubectl delete configmap -n sproutfs sproutfs-demo --ignore-not-found
        kubectl apply -f deploy/05-config.yaml
        kubectl create --dry-run=client -o json -f deploy/10-host.yaml -f deploy/20-orchestrator.yaml \
            -f deploy/30-networkpolicy.yaml |
            python3 deploy/app-restore-manifest.py --hosts $replicas --share 0 --guest-bytes 1073741824 --code 1+1 \
                --template alpine=/usr/share/sproutfs/guest/guest.ext4 --node-cidr $cidr \
                --arena-gib 3 --ram-share 50 --memory-gib 8 --disk-used-gib 40 --orchestrator-node $server |
            kubectl apply -f -
        rm -rf ~/deploy
        kubectl rollout restart -n sproutfs deployment/sproutfs-orchestrator deployment/sproutfs-host
        kubectl rollout status -n sproutfs deployment/sproutfs-orchestrator --timeout=300s
        kubectl rollout status -n sproutfs deployment/sproutfs-host --timeout=900s
        kubectl get -n sproutfs pods -o wide" >> "$results/deploy.log" 2>&1
}

# on runs one step of scripts/lib/fsync-journal-run.sh on the first node.
on() {
    remote "$server" "FSYNC_BENCH_SECONDS=${FSYNC_BENCH_SECONDS:-10s} bash fsync-journal-run.sh $*"
}

# host_on is the host pod on node $1.
host_on() {
    remote "$server" "$kube; kubectl get pods -n sproutfs -l app.kubernetes.io/name=sproutfs-host \
        --field-selector=spec.nodeName=$1,status.phase=Running -o jsonpath='{.items[0].metadata.name}'"
}

# fetch copies what the node-side steps wrote back into the results.
fetch() {
    remote "$server" 'tar -czf /tmp/sproutfs-fsync.tar.gz -C /tmp --exclude=.header sproutfs-fsync' > /dev/null
    "${cloud[@]}" compute scp --zone="$zone" "$server:/tmp/sproutfs-fsync.tar.gz" "$results/" > /dev/null
    tar -xzf "$results/sproutfs-fsync.tar.gz" -C "$results" && rm -f "$results/sproutfs-fsync.tar.gz"
}

# mark appends one moment of the run to the timeline, and says it on stderr:
# a step's stdout is what it returns.
mark() {
    local now
    now=$(python3 -c 'import datetime; print(datetime.datetime.now(datetime.UTC).isoformat(timespec="milliseconds"))')
    printf '%s\t%s\n' "$now" "$*" | tee -a "$results/timeline.tsv" >&2
}

# flushes_with measures a guest's flushes and writes under the deployment as
# it is, on host pod $2, under the label $1.
flushes_with() {
    local label=$1 host=$2 vm
    vm=$(on create)
    on place "$vm" "$host" > /dev/null
    mark "$label: flushes of $vm on $host"
    on flushes "$vm" "$label" > "$results/flushes-$label.txt"
    echo "$vm"
}

# measure_flushes measures a guest's flushes with durable flush off, then on
# over Hyperdisk Balanced, the journal's live bytes over two intervals, and a
# node powered off after a flush.
measure_flushes() {
    local vm host1 sha off_vm
    # The steps append on the node, so this first stage starts from nothing.
    remote "$server" 'rm -rf /tmp/sproutfs-fsync' > /dev/null
    # Durable flush off: the flush bound answers every flush.
    mark "deploying with durable flush off"
    deploy store-off off 2
    on hosts 2 > "$results/hosts-off.txt"
    host1=$(host_on "${nodes[1]}")
    off_vm=$(flushes_with off "$host1")
    on delete "$off_vm" > /dev/null

    # Durable flush on: the orchestrator creates a Hyperdisk Balanced journal
    # disk for each node and attaches it.
    mark "deploying with durable flush on, Hyperdisk Balanced"
    deploy store-hdb on 2
    on hosts 2 > "$results/hosts-hdb.txt"
    mark "both hosts ready with durable flush on"
    host1=$(host_on "${nodes[1]}")
    vm=$(flushes_with hdb "$host1")
    # Live bytes over two checkpoint intervals.
    mark "hdb: one thread flushing for 150 s"
    on interval "$vm" hdb 150
    mark "hdb: interval done"

    # A node powered off after a flush: its journal disk moves to the
    # survivor, and the VM is recovered there with the flush replayed.
    sha=$(on marker "$vm")
    mark "kill: marker $sha flushed on $vm"
    remote "${nodes[1]}" 'sudo sh -c "echo 1 > /proc/sys/kernel/sysrq; echo o > /proc/sysrq-trigger"' \
        > /dev/null 2>&1 || true
    mark "kill: ${nodes[1]} powered off"
    # The operator's evidence that the host is gone: its node and its pod
    # deleted. The pod's replacement has nowhere to run, and the Deployment
    # is scaled to the one node left, which removes that replacement first.
    remote "$server" "$kube; kubectl delete node ${nodes[1]} --wait=false
        kubectl delete pod -n sproutfs $host1 --force --grace-period=0
        kubectl scale -n sproutfs deployment/sproutfs-host --replicas=1" >> "$results/kill.log" 2>&1
    mark "kill: node and pod deleted"
    local recovered=false
    for _ in {1..30}; do
        if remote "$server" "$kube; kubectl exec -n sproutfs -i deploy/sproutfs-orchestrator -- \
            sproutfsctl recover $vm" >> "$results/kill.log" 2>&1; then
            recovered=true
            break
        fi
        sleep 2
    done
    "$recovered" || { echo "$vm was not recovered; see $results/kill.log." >&2; return 1; }
    mark "kill: recovered $vm"
    on check "$vm" "$sha" | tee -a "$results/kill.log"
    mark "kill: marker checked"
    on logs kill > /dev/null
    on delete "$vm" > /dev/null
}

# measure_scale adds a node and drains it, with a VM flushing on it between.
measure_scale() {
    local vm joined
    # A node added: the journal disk the lost node left, read and emptied,
    # is reserved for it, or one is created, and attached while it boots.
    mark "scale-up: creating $joiner"
    # A zone out of c3 machines has some again within minutes, and the node
    # has to be in the journal disks' zone.
    for _ in {1..20}; do
        make_node "$joiner" agent >> "$results/scale-up.log" 2>&1 && break
        grep -q ZONE_RESOURCE_POOL_EXHAUSTED "$results/scale-up.log" || return 1
        mark "scale-up: $zone has no $machine; trying again"
        sleep 30
    done
    existing | grep -qx "$joiner" || { echo "$joiner was never created." >&2; return 1; }
    mark "scale-up: $joiner created"
    wait_cluster "$joiner"
    mark "scale-up: $joiner ready in the cluster"
    import "$joiner"
    mark "scale-up: images imported on $joiner"
    remote "$server" "$kube; kubectl scale -n sproutfs deployment/sproutfs-host --replicas=2" >> "$results/scale-up.log" 2>&1
    on hosts 2 >> "$results/scale-up.log"
    mark "scale-up: two hosts ready"
    joined=$(host_on "$joiner")
    vm=$(on create)
    on place "$vm" "$joined" >> "$results/scale-up.log"
    mark "scale-up: $vm moved to $joined"
    on flushes "$vm" scale-up > /dev/null
    mark "scale-up: $vm flushed on $joined"

    # The node drained: its host migrates the VM away and exits, and its
    # journal disk is detached.
    mark "scale-down: draining $joiner"
    # The pod is deleted by name, so the Deployment's scale-down, which takes
    # the replacement that cannot be scheduled, never chooses the other host.
    remote "$server" "$kube; kubectl cordon $joiner
        kubectl delete pod -n sproutfs $joined --wait=false
        kubectl scale -n sproutfs deployment/sproutfs-host --replicas=1
        kubectl wait -n sproutfs --for=delete pod/$joined --timeout=1900s" >> "$results/scale-down.log" 2>&1
    mark "scale-down: the host pod on $joiner is gone"
    for _ in {1..60}; do
        [[ -z $("${cloud[@]}" compute disks list --filter="labels.sproutfs-journal=sproutfs AND users ~ /$joiner\$" \
            --format='value(name)') ]] && break
        sleep 2
    done
    mark "scale-down: no journal disk attached to $joiner"
    on logs scale > /dev/null
    on delete "$vm" > /dev/null
}

# measure_pds measures a guest's flushes again over a pd-ssd journal disk.
measure_pds() {
    local vm host0
    # pd-ssd: the hosts start again over a new store, with journal disks the
    # bench made of pd-ssd and labelled as the orchestrator labels them, so
    # it reserves those rather than creating any.
    mark "pd-ssd: removing the Hyperdisk Balanced journal disks"
    remote "$server" "$kube; kubectl delete deployment -n sproutfs sproutfs-host sproutfs-orchestrator --wait=true" \
        >> "$results/pd-ssd.log" 2>&1
    local name user
    while read -r name user; do
        [[ -n $name ]] || continue
        [[ -z $user ]] || "${cloud[@]}" compute instances detach-disk "$(basename "$user")" --zone="$zone" \
            --disk="$name" >> "$results/pd-ssd.log" 2>&1
        "${cloud[@]}" compute disks delete "$name" --zone="$zone" >> "$results/pd-ssd.log" 2>&1
    done < <("${cloud[@]}" compute disks list --filter="labels.sproutfs-journal=sproutfs AND zone:$zone" \
        --format='value(name,users[0])')
    "${cloud[@]}" compute disks create "$prefix-jpds-0" --zone="$zone" --type=pd-ssd --size=32GB \
        --labels="$labels,$journal_label" >> "$results/pd-ssd.log" 2>&1
    mark "deploying with durable flush on, pd-ssd"
    deploy store-pds on 1
    on hosts 1 > "$results/hosts-pds.txt"
    host0=$(host_on "$server")
    vm=$(flushes_with pds "$host0")
    on interval "$vm" pds 75
    on delete "$vm" > /dev/null
    on logs end > /dev/null
    mark "done"
}

# measure runs the stages it names, flushes, scale and pds, every one by
# default, and keeps the Compute Engine calls they made and what the node
# wrote.
measure() {
    local began stage
    mkdir -p "$results"
    began=$(date -u +%FT%TZ)
    "${cloud[@]}" compute scp --zone="$zone" "$repo/scripts/lib/fsync-journal-run.sh" "$server:" > /dev/null
    (($# > 0)) || set -- flushes scale pds
    for stage in "$@"; do
        "measure_$stage"
    done
    operations "$began" > "$results/operations-$began.json"
    fetch
}

case "$action" in
    raw) raw ;;
    create) create ;;
    ready) wait_cluster "${nodes[@]}" ;;
    build) build ;;
    deploy) deploy store-off off 2 ;;
    measure) measure ;;
    measure-flushes) measure flushes ;;
    measure-scale) measure scale ;;
    measure-pds) measure pds ;;
    fetch) fetch ;;
    summary) python3 "$repo/scripts/lib/fsync-summary.py" "$results" ;;
    delete) delete ;;
    all)
        [[ -z "$(existing)" ]] || { echo "Machines named $prefix-* already exist." >&2; exit 1; }
        [[ -z "$(journal_disks)" ]] || { echo "Journal disks labelled $journal_label already exist in $zone." >&2; exit 1; }
        trap 'status=$?; trap - EXIT; if ! delete; then exit 1; fi; exit "$status"' EXIT
        mkdir -p "$results"
        # A failed fio job or capture test is in its log; the cluster's
        # measurements do not depend on it.
        raw || echo "The raw step failed; see $results/raw." >&2
        create
        wait_cluster "${nodes[@]}"
        build
        measure
        ;;
    *) echo "Usage: $0 [all|raw|create|ready|build|deploy|measure|measure-flushes|measure-scale|measure-pds|fetch|summary|delete] [name-prefix] [results-directory]" >&2; exit 2 ;;
esac
