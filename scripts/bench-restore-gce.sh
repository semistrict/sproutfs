#!/usr/bin/env bash
# The cluster's disk cache read back on six disposable GCE hosts against a
# real bucket: a guest's memory published from one host, which fills the
# cluster, and read back on another, from the cluster and from the store, in
# order, at random and as a chain of dependent reads
# (docs/measurements/gce-cluster-reads-2026-10-03.md,
# docs/measurements/gce-dependent-reads-2026-10-03.md).
#
# Every host runs `sproutfs-restorebench node`: the real checkpoint store,
# page cache, peer server and table of peers, its cache on a local NVMe SSD.
# The second host runs `sproutfs-restorebench drive`, which publishes a guest
# of each page size from the first, then runs SPROUTFS_RESTORE_ROUNDS rounds
# (three by default) of every case from every source, each round in its own
# order, emptying every host's memory tiers and page cache before each read,
# then reads the SPROUTFS_RESTORE_PROFILE cases once more with the reader's
# CPU profiled. The units fault and runfirst read a page through a real
# pager over the store (cmd/sproutfs-restorebench/pager.go): fault as the
# pager reads, its page first and its run behind it, and runfirst with every
# fault reading its whole run first, as before 2026-10-04
# (docs/measurements/gce-fault-first-2026-10-04.md).
#
# SPROUTFS_GCE_BUCKET names the bucket, and SPROUTFS_GCE_SERVICE_ACCOUNT the
# account the hosts reach it as. The run's objects are removed afterwards.
# SPROUTFS_RESTORE_PAGES is the 2 MiB guest's memory in pages (4096, 8 GiB, by
# default) and SPROUTFS_RESTORE_SMALL_PAGES the 4 KiB guest's (1048576, 4 GiB).
# SPROUTFS_RESTORE_CASES and SPROUTFS_RESTORE_SOURCES choose the cases and
# sources (the drive's defaults when unset); SPROUTFS_RESTORE_PLATFORM is the
# hosts' least processor (Intel Cascade Lake by default). SPROUTFS_RESTORE_TABLES
# is when a read loads its volume's page tables: lazy (the default), as the
# first lookup of each segment needs it, or eager, all of them once the
# checkpoint is open and before the reads. SPROUTFS_RESTORE_BINARY runs a
# bench built elsewhere, for linux/amd64, instead of building this tree's:
# an earlier build, to read before and after a change on the same hosts.
# SPROUTFS_RESTORE_PUBLISHES, when set, has the drive publish the guest of each
# page size the cases name that many times, each as a VM of its own, and read
# nothing: what each publication took and what its fills dropped
# (docs/measurements/gce-fill-backpressure-2026-10-04.md).
# SPROUTFS_RESTORE_PUBLISH_PAGES names those guests instead, as counts of
# 2 MiB pages, comma-separated. SPROUTFS_RESTORE_FAULT_PAGES, when set, also
# times chains of faults on the publisher and on a holder, over a guest of that
# many 2 MiB pages a third host publishes: SPROUTFS_RESTORE_FAULT_HOPS hops
# (500 by default) with nothing publishing, and beside one more publication of
# each guest until it ends
# (docs/measurements/gce-fill-defaults-2026-10-06.md).
#
# `all` always deletes the hosts. `create`, `run` and `delete` expose the same
# steps. Each host also deletes itself after three hours.
set -euo pipefail
[[ ${SPROUTFS_RESTORE_ROUNDS:-} =~ ^[0-9]*$ ]] || { echo "SPROUTFS_RESTORE_ROUNDS is a number" >&2; exit 2; }
[[ ${SPROUTFS_RESTORE_PAGES:-} =~ ^[0-9]*$ ]] || { echo "SPROUTFS_RESTORE_PAGES is a number" >&2; exit 2; }
[[ ${SPROUTFS_RESTORE_SMALL_PAGES:-} =~ ^[0-9]*$ ]] || { echo "SPROUTFS_RESTORE_SMALL_PAGES is a number" >&2; exit 2; }
rounds=${SPROUTFS_RESTORE_ROUNDS:-3}
pages=${SPROUTFS_RESTORE_PAGES:-4096}
small_pages=${SPROUTFS_RESTORE_SMALL_PAGES:-1048576}
machine=${SPROUTFS_RESTORE_MACHINE:-n2-standard-4}
[[ $machine =~ ^n2-(standard|highmem)-[0-9]+$ ]] ||
    { echo "SPROUTFS_RESTORE_MACHINE is an n2-standard or n2-highmem machine type" >&2; exit 2; }
platform=${SPROUTFS_RESTORE_PLATFORM:-Intel Cascade Lake}
[[ $platform =~ ^Intel\ [A-Za-z\ ]+$ ]] || { echo "SPROUTFS_RESTORE_PLATFORM is an Intel CPU platform" >&2; exit 2; }
# The drive's cases and sources, as its flags take them.
drive_flags=""
for setting in cases:SPROUTFS_RESTORE_CASES sources:SPROUTFS_RESTORE_SOURCES profile:SPROUTFS_RESTORE_PROFILE \
    tables:SPROUTFS_RESTORE_TABLES publishes:SPROUTFS_RESTORE_PUBLISHES \
    publish-pages:SPROUTFS_RESTORE_PUBLISH_PAGES fault-pages:SPROUTFS_RESTORE_FAULT_PAGES \
    fault-hops:SPROUTFS_RESTORE_FAULT_HOPS; do
    name=${setting#*:}
    value=${!name:-}
    [[ $value =~ ^[A-Za-z0-9/,-]*$ ]] || { echo "$name is a comma-separated list of cases or sources" >&2; exit 2; }
    if [[ -n $value ]]; then drive_flags+=" -${setting%%:*} $value"; fi
done
# A publication's fills wait for room in the queue, so the publisher goes at
# the pace of its keeps and a larger queue only costs it memory: at 4 GiB a
# publisher of an 8 GiB guest on a 16 GB host was killed for it
# (docs/measurements/gce-fill-backpressure-2026-10-04.md).
# SPROUTFS_RESTORE_FILL_QUEUE_BYTES is the queue, 64 MiB by default, a host's
# own default.
fill_queue=${SPROUTFS_RESTORE_FILL_QUEUE_BYTES:-67108864}
[[ $fill_queue =~ ^[0-9]+$ ]] || { echo "SPROUTFS_RESTORE_FILL_QUEUE_BYTES is a number" >&2; exit 2; }
# SPROUTFS_RESTORE_FILL_BYTES_PER_SECOND is every host's rate of keeps, 4 GiB/s
# by default, which binds nothing.
fill_rate=${SPROUTFS_RESTORE_FILL_BYTES_PER_SECOND:-4294967296}
[[ $fill_rate =~ ^[0-9]+$ ]] || { echo "SPROUTFS_RESTORE_FILL_BYTES_PER_SECOND is a number" >&2; exit 2; }
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
        --machine-type="$machine" --min-cpu-platform="$platform" \
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
    if [[ -n ${SPROUTFS_RESTORE_BINARY:-} ]]; then
        cp -- "$SPROUTFS_RESTORE_BINARY" "$staging/sproutfs-restorebench"
    else
        (cd "$repo" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$staging/sproutfs-restorebench" ./cmd/sproutfs-restorebench)
    fi
    {
        if [[ -n ${SPROUTFS_RESTORE_BINARY:-} ]]; then echo "binary $SPROUTFS_RESTORE_BINARY, built elsewhere"; fi
        echo "revision $(git -C "$repo" rev-parse HEAD)"
        git -C "$repo" status --porcelain=v1 -- cmd/sproutfs-restorebench checkpoint peer rank stripe vmmemory | sed 's/^/changed /'
        (cd "$staging" && shasum -a 256 sproutfs-restorebench)
        echo "machine $machine ($platform), pages $pages and $small_pages, rounds $rounds,$drive_flags," \
            "fill queue $fill_queue, fill rate $fill_rate," \
            "objects gs://$bucket/$run_objects"
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
                \$HOME/sproutfs-restorebench node -advertise $ip:7500 -bucket $bucket -prefix $run_objects -dir /mnt/ssd \
                -fill-queue-bytes $fill_queue -fill-bytes-per-second $fill_rate"
        } >> "$results/remote.log" 2>&1
        nodes+="${nodes:+,}$ip:7600"
    done
    rm -rf -- "$staging"
    sleep 5
    echo "Driving from ${hosts[1]}." >&2
    # The drive runs as a unit of its own, so a dropped SSH connection does not
    # end an hour's run, and is polled until it ends.
    drive_dir=/var/tmp/sproutfs-drive-${run_objects##*/}
    remote "${hosts[1]}" "sudo systemctl reset-failed sproutfs-drive 2>/dev/null || true; \
        sudo mkdir -p $drive_dir; sudo systemd-run --unit=sproutfs-drive --working-directory=$drive_dir \
        \$HOME/sproutfs-restorebench drive -nodes $nodes -pages $pages -small-pages $small_pages \
        -rounds $rounds$drive_flags -out results.json -profiles profiles" >> "$results/remote.log" 2>&1 || status=$?
    local state=active unreachable=0
    while [[ $state == active || $state == activating || $state == unknown ]] && ((status == 0)); do
        sleep 30
        if state=$(remote "${hosts[1]}" "systemctl is-active sproutfs-drive || true" 2>> "$results/remote.log"); then
            unreachable=0
        else
            state=unknown
            ((++unreachable < 10)) || { echo "${hosts[1]} did not answer ten times." >&2; status=1; }
        fi
    done
    [[ $state == inactive ]] || { echo "The drive ended $state." >&2; status=1; }
    remote "${hosts[1]}" "sudo journalctl -u sproutfs-drive --no-pager" > "$results/drive.log" 2>&1 || status=$?
    "${cloud[@]}" compute scp --zone="$zone" "${hosts[1]}:$drive_dir/results.json" "$results/results.json" \
        >> "$results/remote.log" 2>&1 || status=$?
    "${cloud[@]}" compute scp --zone="$zone" --recurse "${hosts[1]}:$drive_dir/profiles" "$results/" \
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
