#!/usr/bin/env bash
# The cluster's disk cache with its disk on a network disk, as a shard keeps
# it, against the same cache on local NVMe, on six disposable GCE hosts
# (docs/measurements/gce-shards-2026-10-04.md). It extends
# scripts/bench-restore-gce.sh.
#
# Every host runs `sproutfs-restorebench node -device <disk>`: the cache keeps
# its disk on the raw block device of one disk attached to the host, with
# buffered I/O, as a shard does. SPROUTFS_SHARD_DISKS names the disks, in the
# order the run takes them: local-nvme (the host's local SSD), and the network
# disk types hyperdisk-balanced, pd-balanced and pd-ssd. For each disk type in
# turn the run makes one disk of it for every host and attaches it, restarts
# every node on it with a new prefix in the bucket, publishes a guest of each
# page size from the first host and reads it back on the second as
# SPROUTFS_RESTORE_CASES says (the chains by default), SPROUTFS_RESTORE_ROUNDS
# rounds (three by default). SPROUTFS_SHARD_BASELINE names the disk type whose
# pass also reads from the store, with no cache, as the baseline: the first by
# default, or none. Then every node stops and fio measures the
# disk raw on every host (scripts/lib/shard-fio.sh), and the network disks are
# deleted before the next type's are made, since the project's quota of
# SSD-backed disks is small.
#
# SPROUTFS_SHARD_DISK_GB is each network disk's size (60 by default), and
# SPROUTFS_SHARD_HDB_IOPS and SPROUTFS_SHARD_HDB_THROUGHPUT (MiB/s) what a
# Hyperdisk Balanced disk is provisioned with (25000 and 500 by default).
# SPROUTFS_SHARD_MACHINE is the machine type (n2-highmem-4 by default; a c3
# type ending in -lssd has its local SSD built in), and
# SPROUTFS_RESTORE_PLATFORM an n2 host's least processor (Intel Ice Lake by
# default). Hyperdisk Balanced does not attach to an n2 host of 4 vCPUs; C3
# takes every type. SPROUTFS_RESTORE_PAGES and SPROUTFS_RESTORE_SMALL_PAGES are
# the guests' pages (4096 of 2 MiB and 1048576 of 4 KiB by default),
# SPROUTFS_RESTORE_FILL_QUEUE_BYTES the nodes' fill queue (12 GiB) and
# SPROUTFS_RESTORE_MEMORY_LIMIT their GOMEMLIMIT (none); a host of 16 GB needs
# smaller guests, a smaller queue and a limit. SPROUTFS_RESTORE_BINARY is a
# built linux/amd64
# sproutfs-restorebench to run, and SPROUTFS_RESTORE_REVISION the commit it was
# built from; without it the script builds one from the tree.
#
# SPROUTFS_GCE_BUCKET names the bucket, and SPROUTFS_GCE_SERVICE_ACCOUNT the
# account the hosts reach it as. The run's objects are removed afterwards.
#
# `all` always deletes the hosts, their disks and the run's objects. `create`,
# `run` and `delete` expose the same steps. Each host also deletes itself, and
# the disks attached to it, after three hours.
set -euo pipefail
for setting in SPROUTFS_RESTORE_ROUNDS SPROUTFS_RESTORE_PAGES SPROUTFS_RESTORE_SMALL_PAGES \
    SPROUTFS_RESTORE_FILL_QUEUE_BYTES SPROUTFS_SHARD_DISK_GB SPROUTFS_SHARD_HDB_IOPS SPROUTFS_SHARD_HDB_THROUGHPUT; do
    [[ ${!setting:-} =~ ^[0-9]*$ ]] || { echo "$setting is a number" >&2; exit 2; }
done
rounds=${SPROUTFS_RESTORE_ROUNDS:-3}
pages=${SPROUTFS_RESTORE_PAGES:-4096}
small_pages=${SPROUTFS_RESTORE_SMALL_PAGES:-1048576}
# The publisher holds the fills of the whole guest it publishes until they
# are sent; 12 GiB holds every fill of the 8 GiB guest.
fill_queue=${SPROUTFS_RESTORE_FILL_QUEUE_BYTES:-12884901888}
disk_gb=${SPROUTFS_SHARD_DISK_GB:-60}
hdb_iops=${SPROUTFS_SHARD_HDB_IOPS:-25000}
hdb_throughput=${SPROUTFS_SHARD_HDB_THROUGHPUT:-500}
disks=${SPROUTFS_SHARD_DISKS:-local-nvme,hyperdisk-balanced,pd-balanced,pd-ssd}
[[ $disks =~ ^((local-nvme|hyperdisk-balanced|pd-balanced|pd-ssd),?)+$ ]] ||
    { echo "SPROUTFS_SHARD_DISKS is a comma-separated list of local-nvme, hyperdisk-balanced, pd-balanced and pd-ssd" >&2; exit 2; }
IFS=, read -r -a disk_types <<< "$disks"
baseline=${SPROUTFS_SHARD_BASELINE:-${disk_types[0]}}
[[ $baseline =~ ^(none|local-nvme|hyperdisk-balanced|pd-balanced|pd-ssd)$ ]] ||
    { echo "SPROUTFS_SHARD_BASELINE is a disk type or none" >&2; exit 2; }
# SPROUTFS_RESTORE_MEMORY_LIMIT is the nodes' GOMEMLIMIT, none by default: a
# host of 16 GB that holds a large fill queue needs one.
memory_limit=${SPROUTFS_RESTORE_MEMORY_LIMIT:-}
[[ $memory_limit =~ ^([0-9]+(MiB|GiB))?$ ]] || { echo "SPROUTFS_RESTORE_MEMORY_LIMIT is a size such as 12GiB" >&2; exit 2; }
node_env=""
if [[ -n $memory_limit ]]; then node_env="--setenv=GOMEMLIMIT=$memory_limit"; fi
machine=${SPROUTFS_SHARD_MACHINE:-n2-highmem-4}
platform=${SPROUTFS_RESTORE_PLATFORM:-Intel Ice Lake}
[[ $platform =~ ^Intel\ [A-Za-z\ ]+$ ]] || { echo "SPROUTFS_RESTORE_PLATFORM is an Intel CPU platform" >&2; exit 2; }
# An n2 host takes its local SSD as a flag and boots from a standard disk,
# which leaves the quota of SSD-backed persistent disks to the shards. A c3
# host has its local SSD built into an -lssd type, cannot boot from a standard
# disk, and boots from Hyperdisk Balanced. The project's region holds 500 GB
# of each kind, so six shards of 60 GB fit beside six boot disks of 20 GB.
machine_flags=()
if [[ $machine =~ ^n2-(standard|highmem)-[0-9]+$ ]]; then
    machine_flags=(--min-cpu-platform="$platform" --local-ssd=interface=NVME --boot-disk-type=pd-standard)
elif [[ $machine =~ ^c3-(standard|highmem)-[0-9]+-lssd$ ]]; then
    platform="Intel Sapphire Rapids"
    machine_flags=(--boot-disk-type=hyperdisk-balanced)
else
    echo "SPROUTFS_SHARD_MACHINE is an n2-standard, n2-highmem or c3 -lssd machine type" >&2
    exit 2
fi
cases=${SPROUTFS_RESTORE_CASES:-2MiB/chain/page/1,2MiB/chain/run/1,4KiB/chain/page/1,4KiB/chain/run/1}
profile=${SPROUTFS_RESTORE_PROFILE:-2MiB/chain/page/1,4KiB/chain/page/1}
for list in "$cases" "$profile"; do
    [[ $list =~ ^[A-Za-z0-9/,-]+$ ]] || { echo "SPROUTFS_RESTORE_CASES and SPROUTFS_RESTORE_PROFILE are lists of cases" >&2; exit 2; }
done
bucket=${SPROUTFS_GCE_BUCKET:-}
account=${SPROUTFS_GCE_SERVICE_ACCOUNT:-}
[[ -n $bucket && -n $account ]] || { echo "Set SPROUTFS_GCE_BUCKET and SPROUTFS_GCE_SERVICE_ACCOUNT." >&2; exit 2; }

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
project=${SPROUTFS_GCE_PROJECT:-$(gcloud config get-value project 2>/dev/null)}
zone=${SPROUTFS_GCE_ZONE:-us-east4-a}
action=${1:-all}
prefix=${2:-sproutfs-shards-$(date -u +%Y%m%d-%H%M%S)}
results=${3:-$repo/docs/measurements/gce-shards-$(date -u +%Y-%m-%d)}
[[ -n "$project" && "$project" != '(unset)' ]] || { echo 'Set SPROUTFS_GCE_PROJECT.' >&2; exit 2; }
[[ "$prefix" =~ ^sproutfs-shards-[a-z0-9-]+$ && ${#prefix} -le 50 ]] ||
    { echo 'Use a sproutfs-shards- name prefix of at most 50 characters.' >&2; exit 2; }
cloud=(gcloud --quiet --project="$project")
labels=purpose=shard-bench,lifecycle=temporary
hosts=()
for index in 0 1 2 3 4 5; do hosts+=("$prefix-$index"); done
# Every pass keeps its objects under a prefix of its own, so each disk type's
# publication goes into an empty one.
objects="sproutfs-bench/$prefix"

# short is a network disk type's part of its disks' names.
short() {
    case "$1" in
        hyperdisk-balanced) echo hdb ;;
        pd-balanced) echo pdb ;;
        pd-ssd) echo pds ;;
        *) return 1 ;;
    esac
}

# device is the name the node opens the disk of a type on host index by: the
# local SSD's own, or the network disk's, which is attached under its name.
device() {
    if [[ $1 == local-nvme ]]; then echo local-nvme-ssd-0; else echo "$prefix-$2-$(short "$1")"; fi
}

create() {
    "${cloud[@]}" compute instances create "${hosts[@]}" --zone="$zone" \
        --machine-type="$machine" "${machine_flags[@]}" \
        --image=ubuntu-2604-resolute-amd64-v20260907 --image-project=ubuntu-os-cloud \
        --boot-disk-size=20GB --boot-disk-auto-delete \
        --network="${SPROUTFS_GCE_NETWORK:-default}" \
        --service-account="$account" --scopes=storage-rw \
        --metadata=block-project-ssh-keys=TRUE \
        --metadata-from-file="startup-script=$repo/scripts/lib/gce-bench-startup.sh" \
        --labels="$labels" \
        --maintenance-policy=TERMINATE --no-restart-on-failure \
        --max-run-duration=3h --instance-termination-action=DELETE
}

existing() {
    "${cloud[@]}" compute instances list --filter="name ~ ^$prefix-[0-9]+\$" --format='value(name,zone.basename())'
}

existing_disks() {
    "${cloud[@]}" compute disks list --filter="name ~ ^$prefix-[0-9]+(-[a-z]+)?\$" --format='value(name,zone.basename())'
}

locate() {
    local name found
    while read -r name found; do
        [[ -n $found ]] && zone=$found
    done < <(existing)
}

check_owner() {
    local kind=$1 name=$2 found
    found=$("${cloud[@]}" compute "$kind" describe "$name" --zone="$zone" \
        --format='value(labels.purpose,labels.lifecycle)')
    [[ "$found" == $'shard-bench\ttemporary' ]] || {
        echo "Refusing to operate on $name, which lacks the benchmark ownership labels." >&2
        return 1
    }
}

# make_disks makes one disk of a network type for every host and attaches it
# under its own name, to be deleted with the host should the host delete
# itself. It is called where errexit does not hold, so it checks each step.
# gcloud cannot find a disk by --disk on a host with a local SSD, so the
# auto-delete is set by device name.
make_disks() {
    local type=$1 index name
    local -a names=() flags=()
    for index in "${!hosts[@]}"; do names+=("$(device "$type" "$index")"); done
    if [[ $type == hyperdisk-balanced ]]; then
        flags=(--provisioned-iops="$hdb_iops" --provisioned-throughput="$hdb_throughput")
    fi
    "${cloud[@]}" compute disks create "${names[@]}" --zone="$zone" --type="$type" --size="${disk_gb}GB" \
        ${flags[@]+"${flags[@]}"} --labels="$labels" || return 1
    for index in "${!hosts[@]}"; do
        name=${names[$index]}
        "${cloud[@]}" compute instances attach-disk "${hosts[$index]}" --zone="$zone" --disk="$name" \
            --device-name="$name" || return 1
        "${cloud[@]}" compute instances set-disk-auto-delete "${hosts[$index]}" --zone="$zone" \
            --device-name="$name" --auto-delete || return 1
    done
}

# drop_disks detaches and deletes the disks of a network type.
drop_disks() {
    local type=$1 index name
    local -a names=()
    for index in "${!hosts[@]}"; do
        name=$(device "$type" "$index")
        if "${cloud[@]}" compute disks describe "$name" --zone="$zone" --format='value(name)' > /dev/null 2>&1; then
            check_owner disks "$name"
            "${cloud[@]}" compute instances detach-disk "${hosts[$index]}" --zone="$zone" --disk="$name" || true
            names+=("$name")
        fi
    done
    if ((${#names[@]} > 0)); then
        "${cloud[@]}" compute disks delete "${names[@]}" --zone="$zone"
    fi
}

delete() {
    local name where left where_of=""
    local -a names=() disk_names=()
    while read -r name where; do
        zone=$where check_owner instances "$name"
        names+=("$name")
        where_of=$where
    done < <(existing)
    if ((${#names[@]} > 0)); then
        "${cloud[@]}" compute instances delete "${names[@]}" --zone="$where_of" --delete-disks=all
    fi
    left=$(existing)
    [[ -z "$left" ]] || { echo "Hosts still exist: $left" >&2; return 1; }
    while read -r name where; do
        [[ -n $name ]] || continue
        zone=$where check_owner disks "$name"
        disk_names+=("$name")
        where_of=$where
    done < <(existing_disks)
    if ((${#disk_names[@]} > 0)); then
        "${cloud[@]}" compute disks delete "${disk_names[@]}" --zone="$where_of"
    fi
    left=$(existing_disks)
    [[ -z "$left" ]] || { echo "Disks still exist: $left" >&2; return 1; }
    "${cloud[@]}" storage rm --recursive "gs://$bucket/$objects/" > /dev/null 2>&1 || true
    left=$("${cloud[@]}" storage ls "gs://$bucket/$objects/" 2>/dev/null || true)
    [[ -z "$left" ]] || { echo "Objects still exist under gs://$bucket/$objects/" >&2; return 1; }
    echo "Verified deletion of every $prefix host, its disks and its objects." >&2
}

remote() {
    local host=$1
    shift
    "${cloud[@]}" compute ssh "$host" --zone="$zone" \
        --ssh-flag='-o ConnectTimeout=10' --ssh-flag='-o ServerAliveInterval=15' --command="$*" < /dev/null
}

# stage copies the bench and the fio script to every host once they have
# started, and records what ran.
stage() {
    local host ready staging binary revision
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
    staging=$(mktemp -d "${TMPDIR:-/tmp}/sproutfs-shards.XXXXXX")
    if [[ -n ${SPROUTFS_RESTORE_BINARY:-} ]]; then
        binary=$SPROUTFS_RESTORE_BINARY
        revision="${SPROUTFS_RESTORE_REVISION:-unknown} (given with the binary)"
    else
        binary=$staging/sproutfs-restorebench
        (cd "$repo" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$binary" ./cmd/sproutfs-restorebench)
        revision="$(git -C "$repo" rev-parse HEAD) (built here)"
    fi
    {
        echo "revision $revision"
        if [[ -z ${SPROUTFS_RESTORE_BINARY:-} ]]; then
            git -C "$repo" status --porcelain=v1 -- cmd/sproutfs-restorebench checkpoint peer platform rank stripe |
                sed 's/^/changed /'
        fi
        echo "sha256 $(shasum -a 256 < "$binary" | cut -d' ' -f1)  sproutfs-restorebench"
        echo "machine $machine ($platform), zone $zone, disks $disks, network disks of ${disk_gb} GB," \
            "Hyperdisk Balanced at $hdb_iops IOPS and $hdb_throughput MiB/s"
        echo "pages $pages and $small_pages, rounds $rounds, cases $cases, profile $profile," \
            "fill queue $fill_queue, memory limit ${memory_limit:-none}, baseline $baseline," \
            "objects gs://$bucket/$objects"
    } > "$results/source.txt"
    for host in "${hosts[@]}"; do
        "${cloud[@]}" compute instances describe "$host" --zone="$zone" \
            --format='json(name,zone,machineType,cpuPlatform,networkInterfaces[].networkIP)' > "$results/instance-$host.json"
        {
            remote "$host" "sudo systemctl stop sproutfs-node 2>/dev/null || true; \
                sudo systemctl reset-failed sproutfs-node 2>/dev/null || true"
            "${cloud[@]}" compute scp --zone="$zone" "$binary" "$repo/scripts/lib/shard-fio.sh" "$host:"
            remote "$host" "chmod +x sproutfs-restorebench shard-fio.sh"
        } >> "$results/remote.log" 2>&1
    done
    rm -rf -- "$staging"
}

# start restarts every node with its cache on the disk of a type, under a new
# prefix of objects, and waits until each answers.
start() {
    local type=$1 pass_objects=$2 out=$3 index host ip name answered
    for index in "${!hosts[@]}"; do
        host=${hosts[$index]}
        name=$(device "$type" "$index")
        ip=$("${cloud[@]}" compute instances describe "$host" --zone="$zone" --format='value(networkInterfaces[0].networkIP)')
        {
            remote "$host" "set -e; sudo systemctl stop sproutfs-node 2>/dev/null || true; \
                sudo systemctl reset-failed sproutfs-node 2>/dev/null || true; \
                if mountpoint -q /mnt/ssd; then sudo umount /mnt/ssd; fi; \
                sudo systemd-run --unit=sproutfs-node --property=LimitNOFILE=65536 $node_env \
                \$HOME/sproutfs-restorebench node -advertise $ip:7500 -bucket $bucket -prefix $pass_objects \
                -device $name -fill-queue-bytes $fill_queue"
        } >> "$results/remote.log" 2>&1
        # What the kernel does with the block device's buffered reads.
        remote "$host" "dev=\$(readlink -f /dev/disk/by-id/google-$name); b=\$(basename \$dev); \
            echo device \$dev; for f in read_ahead_kb max_sectors_kb scheduler nr_requests rotational; do \
            echo \$f \$(cat /sys/block/\$b/queue/\$f); done; lsblk -o NAME,SIZE,MODEL" > "$out/device-$host.txt" 2>&1
    done
    for host in "${hosts[@]}"; do
        answered=false
        for _ in {1..30}; do
            if remote "$host" "systemctl is-active --quiet sproutfs-node && \
                curl -sf -X POST -d '{}' localhost:7600/identity > /dev/null" >> "$results/remote.log" 2>&1; then
                answered=true
                break
            fi
            sleep 2
        done
        "$answered" || { echo "The node on $host did not answer." >&2; return 1; }
    done
}

# drive reads the cases on the disk of a type from the second host, as a unit
# of its own, so a dropped SSH connection does not end it, and polls it until
# it ends.
drive() {
    local sources=$1 out=$2 stamp=$3 nodes="" host ip status=0 state=active unreachable=0 drive_dir
    for host in "${hosts[@]}"; do
        ip=$("${cloud[@]}" compute instances describe "$host" --zone="$zone" --format='value(networkInterfaces[0].networkIP)')
        nodes+="${nodes:+,}$ip:7600"
    done
    drive_dir=/var/tmp/sproutfs-drive-$stamp
    remote "${hosts[1]}" "sudo systemctl reset-failed sproutfs-drive 2>/dev/null || true; \
        sudo mkdir -p $drive_dir; sudo systemd-run --unit=sproutfs-drive --working-directory=$drive_dir \
        \$HOME/sproutfs-restorebench drive -nodes $nodes -pages $pages -small-pages $small_pages \
        -rounds $rounds -cases $cases -sources $sources -profile $profile -out results.json -profiles profiles" \
        >> "$results/remote.log" 2>&1 || status=$?
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
    remote "${hosts[1]}" "sudo journalctl -u sproutfs-drive --no-pager" > "$out/drive.log" 2>&1 || status=$?
    "${cloud[@]}" compute scp --zone="$zone" "${hosts[1]}:$drive_dir/results.json" "$out/results.json" \
        >> "$results/remote.log" 2>&1 || status=$?
    "${cloud[@]}" compute scp --zone="$zone" --recurse "${hosts[1]}:$drive_dir/profiles" "$out/" \
        >> "$results/remote.log" 2>&1 || status=$?
    for host in "${hosts[@]}"; do
        remote "$host" "sudo journalctl -u sproutfs-node --no-pager" > "$out/node-$host.log" 2>&1 || status=$?
    done
    return "$status"
}

# measure stops every node and runs fio on the disk of a type on every host
# at once.
measure() {
    local type=$1 out=$2 index host status=0
    local -a waits=()
    for index in "${!hosts[@]}"; do
        host=${hosts[$index]}
        {
            remote "$host" "sudo systemctl stop sproutfs-node 2>/dev/null || true; \
                sudo systemctl reset-failed sproutfs-node 2>/dev/null || true; \
                rm -rf /var/tmp/shard-fio-$type; ./shard-fio.sh $(device "$type" "$index") /var/tmp/shard-fio-$type" \
                > "$out/fio-$host.log" 2>&1 &&
                "${cloud[@]}" compute scp --zone="$zone" --recurse "$host:/var/tmp/shard-fio-$type" "$out/fio-$host" \
                    >> "$out/fio-$host.log" 2>&1
        } &
        waits+=($!)
    done
    for index in "${!waits[@]}"; do
        wait "${waits[$index]}" || { echo "fio failed on ${hosts[$index]}." >&2; status=1; }
    done
    return "$status"
}

# pass measures the cache on the disks of one type: made and attached, read
# through, measured raw, and deleted.
pass() {
    local type=$1 sources=$2 out stamp status=0
    out=$results/$type
    mkdir -p "$out"
    stamp=$(date -u +%Y%m%dT%H%M%SZ)
    echo "Passing over $type." >&2
    if [[ $type != local-nvme ]]; then
        make_disks "$type" >> "$results/remote.log" 2>&1 || { drop_disks "$type" >> "$results/remote.log" 2>&1; return 1; }
        "${cloud[@]}" compute disks describe "$(device "$type" 0)" --zone="$zone" \
            --format='json(name,type,sizeGb,provisionedIops,provisionedThroughput)' > "$out/disk.json"
    fi
    echo "objects gs://$bucket/$objects/$stamp" > "$out/source.txt"
    start "$type" "$objects/$stamp" "$out" || status=$?
    if ((status == 0)); then drive "$sources" "$out" "$stamp" || status=$?; fi
    if ((status == 0)); then measure "$type" "$out" || status=$?; fi
    if [[ $type != local-nvme ]]; then
        drop_disks "$type" >> "$results/remote.log" 2>&1 || status=1
    fi
    return "$status"
}

run() {
    local host index sources status=0
    for host in "${hosts[@]}"; do check_owner instances "$host"; done
    mkdir -p "$results"
    results=$(cd "$results" && pwd)
    stage
    for index in "${!disk_types[@]}"; do
        sources=cluster
        if [[ ${disk_types[$index]} == "$baseline" ]]; then sources=cluster,store; fi
        pass "${disk_types[$index]}" "$sources" || { echo "The pass over ${disk_types[$index]} failed." >&2; status=1; }
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
