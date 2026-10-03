#!/usr/bin/env bash
# The cluster's disk cache read back on six disposable GCE hosts against a
# real bucket: a guest's memory published from one host, which fills the
# cluster, and read back whole on another, from the cluster, from the store,
# and from the cluster with a third host lost part way through
# (docs/measurements/gce-cluster-reads-2026-10-03.md).
#
# Every host runs `sproutfs-restorebench node`: the real checkpoint store,
# page cache, peer server and table of peers, its cache on a local NVMe SSD.
# The second host runs `sproutfs-restorebench drive`, which publishes from the
# first, then runs SPROUTFS_RESTORE_ROUNDS rounds (three by default) of every
# case, each round in its own order, dropping every host's page cache before
# each restore.
#
# SPROUTFS_GCE_BUCKET names the bucket, and SPROUTFS_GCE_SERVICE_ACCOUNT the
# account the hosts reach it as. The run's objects are removed afterwards.
# SPROUTFS_RESTORE_PAGES is the guest's memory in 2 MiB pages (4096, 8 GiB, by
# default).
#
# `all` always deletes the hosts. `create`, `run` and `delete` expose the same
# steps. Each host also deletes itself after three hours.
set -euo pipefail
[[ ${SPROUTFS_RESTORE_ROUNDS:-} =~ ^[0-9]*$ ]] || { echo "SPROUTFS_RESTORE_ROUNDS is a number" >&2; exit 2; }
[[ ${SPROUTFS_RESTORE_PAGES:-} =~ ^[0-9]*$ ]] || { echo "SPROUTFS_RESTORE_PAGES is a number" >&2; exit 2; }
rounds=${SPROUTFS_RESTORE_ROUNDS:-3}
pages=${SPROUTFS_RESTORE_PAGES:-4096}
machine=${SPROUTFS_RESTORE_MACHINE:-n2-standard-4}
[[ $machine =~ ^n2-standard-[0-9]+$ ]] || { echo "SPROUTFS_RESTORE_MACHINE is an n2-standard machine type" >&2; exit 2; }
bucket=${SPROUTFS_GCE_BUCKET:-}
account=${SPROUTFS_GCE_SERVICE_ACCOUNT:-}
[[ -n $bucket && -n $account ]] || { echo "Set SPROUTFS_GCE_BUCKET and SPROUTFS_GCE_SERVICE_ACCOUNT." >&2; exit 2; }

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
project=${SPROUTFS_GCE_PROJECT:-$(gcloud config get-value project 2>/dev/null)}
zone=${SPROUTFS_GCE_ZONE:-us-east4-a}
action=${1:-all}
prefix=${2:-sproutfs-restore-$(date -u +%Y%m%d-%H%M%S)}
results=${3:-$repo/docs/measurements/gce-cluster-reads-$(date -u +%Y-%m-%d)}
[[ -n "$project" && "$project" != '(unset)' ]] || { echo 'Set SPROUTFS_GCE_PROJECT.' >&2; exit 2; }
[[ "$prefix" =~ ^sproutfs-restore-[a-z0-9-]+$ ]] || { echo 'Use a sproutfs-restore- name prefix.' >&2; exit 2; }
cloud=(gcloud --quiet --project="$project")
hosts=()
for index in 0 1 2 3 4 5; do hosts+=("$prefix-$index"); done
# Every run keeps its objects under a prefix of its own, so a second run on
# the same hosts publishes into an empty one.
objects="sproutfs-bench/$prefix"
run_objects="$objects/$(date -u +%Y%m%dT%H%M%SZ)"

create() {
    "${cloud[@]}" compute instances create "${hosts[@]}" --zone="$zone" \
        --machine-type="$machine" --min-cpu-platform='Intel Cascade Lake' \
        --image=ubuntu-2604-resolute-amd64-v20260907 --image-project=ubuntu-os-cloud \
        --boot-disk-size=20GB --boot-disk-type=pd-balanced --boot-disk-auto-delete \
        --local-ssd=interface=NVME \
        --network="${SPROUTFS_GCE_NETWORK:-default}" \
        --service-account="$account" --scopes=storage-rw \
        --metadata=block-project-ssh-keys=TRUE \
        --metadata-from-file="startup-script=$repo/scripts/lib/gce-bench-startup.sh" \
        --labels=purpose=restore-bench,lifecycle=temporary \
        --maintenance-policy=TERMINATE --no-restart-on-failure \
        --max-run-duration=3h --instance-termination-action=DELETE
}

existing() {
    "${cloud[@]}" compute instances list --filter="name ~ ^$prefix-[0-9]+\$" --format='value(name,zone.basename())'
}

locate() {
    local name found
    while read -r name found; do
        [[ -n $found ]] && zone=$found
    done < <(existing)
}

check_owner() {
    local labels
    labels=$("${cloud[@]}" compute instances describe "$1" --zone="$zone" \
        --format='value(labels.purpose,labels.lifecycle)')
    [[ "$labels" == $'restore-bench\ttemporary' ]] || {
        echo "Refusing to operate on $1, which lacks the benchmark ownership labels." >&2
        return 1
    }
}

delete() {
    local name where left where_of=""
    local -a names=()
    while read -r name where; do
        zone=$where check_owner "$name"
        names+=("$name")
        where_of=$where
    done < <(existing)
    if ((${#names[@]} > 0)); then
        "${cloud[@]}" compute instances delete "${names[@]}" --zone="$where_of" --delete-disks=all
    fi
    left=$(existing)
    [[ -z "$left" ]] || { echo "Hosts still exist: $left" >&2; return 1; }
    left=$("${cloud[@]}" compute disks list --filter="name ~ ^$prefix-[0-9]+\$" --format='value(name)')
    [[ -z "$left" ]] || { echo "Disks still exist: $left" >&2; return 1; }
    "${cloud[@]}" storage rm --recursive "gs://$bucket/$objects/" > /dev/null 2>&1 || true
    left=$("${cloud[@]}" storage ls "gs://$bucket/$objects/" 2>/dev/null || true)
    [[ -z "$left" ]] || { echo "Objects still exist under gs://$bucket/$objects/" >&2; return 1; }
    echo "Verified deletion of every $prefix host, its disks and its objects." >&2
}

remote() {
    local host=$1
    shift
    "${cloud[@]}" compute ssh "$host" --zone="$zone" --ssh-flag='-o ConnectTimeout=10' --command="$*" < /dev/null
}

run() {
    local host ready status=0 staging index ip nodes=""
    local -a ips=()
    for host in "${hosts[@]}"; do check_owner "$host"; done
    mkdir -p "$results"
    results=$(cd "$results" && pwd)
    for host in "${hosts[@]}"; do
        ready=false
        for _ in {1..30}; do
            if remote "$host" 'test -e /var/lib/sproutfs-bench/ready' >> "$results/startup.log" 2>&1; then
                ready=true
                break
            fi
            sleep 10
        done
        "$ready" || { echo "$host did not finish starting." >&2; return 1; }
    done
    staging=$(mktemp -d "${TMPDIR:-/tmp}/sproutfs-restore.XXXXXX")
    (cd "$repo" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$staging/sproutfs-restorebench" ./cmd/sproutfs-restorebench)
    {
        echo "revision $(git -C "$repo" rev-parse HEAD)"
        git -C "$repo" status --porcelain=v1 -- cmd/sproutfs-restorebench checkpoint peer rank stripe | sed 's/^/changed /'
        (cd "$staging" && shasum -a 256 sproutfs-restorebench)
        echo "machine $machine, pages $pages, rounds $rounds, objects gs://$bucket/$run_objects"
    } > "$results/source.txt"
    for index in "${!hosts[@]}"; do
        host=${hosts[$index]}
        ip=$("${cloud[@]}" compute instances describe "$host" --zone="$zone" --format='value(networkInterfaces[0].networkIP)')
        ips+=("$ip")
        "${cloud[@]}" compute instances describe "$host" --zone="$zone" \
            --format='json(name,zone,machineType,cpuPlatform,networkInterfaces[].networkIP)' > "$results/instance-$host.json"
        {
            # A node of an earlier run on the same hosts stops first: its
            # binary is about to be replaced.
            remote "$host" "sudo systemctl stop sproutfs-node 2>/dev/null || true; \
                sudo systemctl reset-failed sproutfs-node 2>/dev/null || true"
            "${cloud[@]}" compute scp --zone="$zone" "$staging/sproutfs-restorebench" "$host:"
            # The local SSD holds the cache's file, as a host's node directory
            # does.
            remote "$host" "set -e; if mountpoint -q /mnt/ssd; then sudo umount /mnt/ssd; fi; \
                ssd=\$(ls /dev/disk/by-id/google-local-nvme-ssd-0); \
                sudo mkfs.ext4 -q -F \$ssd; sudo mkdir -p /mnt/ssd; sudo mount \$ssd /mnt/ssd; \
                sudo systemd-run --unit=sproutfs-node --property=LimitNOFILE=65536 \
                \$HOME/sproutfs-restorebench node -advertise $ip:7500 -bucket $bucket -prefix $run_objects -dir /mnt/ssd"
        } >> "$results/remote.log" 2>&1
        nodes+="${nodes:+,}$ip:7600"
    done
    rm -rf -- "$staging"
    sleep 5
    echo "Driving from ${hosts[1]}." >&2
    remote "${hosts[1]}" "./sproutfs-restorebench drive -nodes $nodes -pages $pages -rounds $rounds -out results.json" \
        > "$results/drive.log" 2>&1 || status=$?
    "${cloud[@]}" compute scp --zone="$zone" "${hosts[1]}:results.json" "$results/results.json" \
        >> "$results/remote.log" 2>&1 || status=$?
    for host in "${hosts[@]}"; do
        remote "$host" "sudo journalctl -u sproutfs-node --no-pager" > "$results/node-$host.log" 2>&1 || status=$?
    done
    return "$status"
}

case "$action" in
    create) create ;;
    run) locate; run ;;
    delete) delete ;;
    all)
        [[ -z "$(existing)" ]] || { echo "Hosts named $prefix-* already exist." >&2; exit 1; }
        trap 'status=$?; trap - EXIT; if ! delete; then exit 1; fi; exit "$status"' EXIT
        create
        run
        ;;
    *) echo "Usage: $0 [all|create|run|delete] [name-prefix] [results-directory]" >&2; exit 2 ;;
esac
