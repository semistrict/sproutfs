#!/usr/bin/env bash
# A shard of the cluster's cache moving between two disposable GCE hosts when
# the one serving it is removed (docs/measurements/gce-shards-2026-10-04.md,
# "Moving a shard").
#
# Each host runs `sproutfs-shardbench member`: the real host, with no VMM,
# serving the shards the membership assigns it, which it opens at
# /dev/disk/by-id/google-<name>. It runs as a systemd unit that starts again
# when it ends, as a kubelet starts a pod again, so a member that dies comes
# back as a new member. The first host also runs `sproutfs-shardbench control`,
# the controller: a pass of the membership and of Compute Engine's attach API
# every SPROUTFS_SHARD_MOVE_INTERVAL (one second by default; the orchestrator's
# is five), with the hosts' credentials. It fills the shard by publishing a VM
# of SPROUTFS_SHARD_MOVE_FILL_GB through the member that serves it, then
# SPROUTFS_SHARD_MOVE_ROUNDS times (three by default) takes that member away by
# draining it, and as many times by ending its process, and times each step
# until the shard serves on the other member.
#
# SPROUTFS_SHARD_DISK_TYPE is the shard's disk type (hyperdisk-balanced by
# default, or pd-balanced or pd-ssd), SPROUTFS_SHARD_DISK_GB its size (256 by
# default), and SPROUTFS_SHARD_HDB_IOPS and SPROUTFS_SHARD_HDB_THROUGHPUT what
# a Hyperdisk Balanced disk is provisioned with (6000 and 500 MiB/s by
# default). SPROUTFS_SHARD_MACHINE is the hosts' machine type: c3-standard-4
# by default for Hyperdisk Balanced, which no N2 machine attaches, and
# n2-highmem-4, Intel Ice Lake, for the others. SPROUTFS_SHARD_BINARY is a built linux/amd64
# sproutfs-shardbench to run; without it the script builds one from the tree.
# SPROUTFS_GCE_BUCKET names the bucket. The hosts run as the project's
# default compute account, which may attach and detach disks.
#
# `all` always deletes the hosts, the disk and the run's objects, and checks
# that none remain. `create`, `run` and `delete` expose the same steps. Each
# host also deletes itself after three hours.
set -euo pipefail
for setting in SPROUTFS_SHARD_MOVE_ROUNDS SPROUTFS_SHARD_MOVE_FILL_GB SPROUTFS_SHARD_DISK_GB \
    SPROUTFS_SHARD_HDB_IOPS SPROUTFS_SHARD_HDB_THROUGHPUT; do
    [[ ${!setting:-} =~ ^[0-9]*$ ]] || { echo "$setting is a number" >&2; exit 2; }
done
rounds=${SPROUTFS_SHARD_MOVE_ROUNDS:-3}
fill_gb=${SPROUTFS_SHARD_MOVE_FILL_GB:-32}
interval=${SPROUTFS_SHARD_MOVE_INTERVAL:-1s}
[[ $interval =~ ^[0-9]+(ms|s)$ ]] || { echo "SPROUTFS_SHARD_MOVE_INTERVAL is a duration such as 1s" >&2; exit 2; }
disk_type=${SPROUTFS_SHARD_DISK_TYPE:-hyperdisk-balanced}
[[ $disk_type =~ ^(hyperdisk-balanced|pd-balanced|pd-ssd)$ ]] ||
    { echo "SPROUTFS_SHARD_DISK_TYPE is hyperdisk-balanced, pd-balanced or pd-ssd" >&2; exit 2; }
disk_gb=${SPROUTFS_SHARD_DISK_GB:-256}
hdb_iops=${SPROUTFS_SHARD_HDB_IOPS:-6000}
hdb_throughput=${SPROUTFS_SHARD_HDB_THROUGHPUT:-500}
if [[ $disk_type == hyperdisk-balanced ]]; then
    machine=${SPROUTFS_SHARD_MACHINE:-c3-standard-4}
    [[ $machine != n2-* ]] || { echo "No N2 machine attaches a Hyperdisk Balanced disk." >&2; exit 2; }
else
    machine=${SPROUTFS_SHARD_MACHINE:-n2-highmem-4}
fi
[[ $machine =~ ^(n2|c3)-(standard|highmem)-[0-9]+$ ]] ||
    { echo "SPROUTFS_SHARD_MACHINE is an n2 or c3 standard or highmem machine type" >&2; exit 2; }
machine_flags=(--boot-disk-type=pd-balanced)
if [[ $machine == n2-* ]]; then
    machine_flags+=(--min-cpu-platform="Intel Ice Lake")
fi
bucket=${SPROUTFS_GCE_BUCKET:-}
[[ -n $bucket ]] || { echo "Set SPROUTFS_GCE_BUCKET." >&2; exit 2; }

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
project=${SPROUTFS_GCE_PROJECT:-$(gcloud config get-value project 2>/dev/null)}
zone=${SPROUTFS_GCE_ZONE:-us-east4-a}
action=${1:-all}
prefix=${2:-sproutfs-move-$(date -u +%Y%m%d-%H%M%S)}
results=${3:-$repo/docs/measurements/gce-shards-$(date -u +%Y-%m-%d)/move-$disk_type}
[[ -n "$project" && "$project" != '(unset)' ]] || { echo 'Set SPROUTFS_GCE_PROJECT.' >&2; exit 2; }
[[ "$prefix" =~ ^sproutfs-move-[a-z0-9-]+$ && ${#prefix} -le 50 ]] ||
    { echo 'Use a sproutfs-move- name prefix of at most 50 characters.' >&2; exit 2; }
cloud=(gcloud --quiet --project="$project")
labels=purpose=shard-move-bench,lifecycle=temporary
hosts=("$prefix-0" "$prefix-1")
shard="$prefix-shard"
objects="sproutfs-bench/$prefix"

create() {
    local -a flags=()
    "${cloud[@]}" compute instances create "${hosts[@]}" --zone="$zone" \
        --machine-type="$machine" "${machine_flags[@]}" \
        --image=ubuntu-2604-resolute-amd64-v20260907 --image-project=ubuntu-os-cloud \
        --boot-disk-size=20GB --boot-disk-auto-delete \
        --network="${SPROUTFS_GCE_NETWORK:-default}" --scopes=cloud-platform \
        --metadata=block-project-ssh-keys=TRUE \
        --metadata-from-file="startup-script=$repo/scripts/lib/gce-bench-startup.sh" \
        --labels="$labels" \
        --maintenance-policy=TERMINATE --no-restart-on-failure \
        --max-run-duration=3h --instance-termination-action=DELETE
    if [[ $disk_type == hyperdisk-balanced ]]; then
        flags=(--provisioned-iops="$hdb_iops" --provisioned-throughput="$hdb_throughput")
    fi
    "${cloud[@]}" compute disks create "$shard" --zone="$zone" --type="$disk_type" --size="${disk_gb}GB" \
        ${flags[@]+"${flags[@]}"} --labels="$labels"
}

existing() {
    "${cloud[@]}" compute instances list --filter="name ~ ^$prefix-[0-9]+\$" --format='value(name,zone.basename())'
}

existing_disks() {
    "${cloud[@]}" compute disks list --filter="name ~ ^$prefix-" --format='value(name,zone.basename())'
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
    [[ "$found" == $'shard-move-bench\ttemporary' ]] || {
        echo "Refusing to operate on $name, which lacks the benchmark ownership labels." >&2
        return 1
    }
}

delete() {
    local name where left status=0
    local -a names=()
    while read -r name where; do
        zone=$where check_owner instances "$name"
        names+=("$name")
    done < <(existing)
    if ((${#names[@]} > 0)); then
        "${cloud[@]}" compute instances delete "${names[@]}" --zone="$zone" --delete-disks=all || status=1
    fi
    while read -r name where; do
        zone=$where check_owner disks "$name"
        "${cloud[@]}" compute disks delete "$name" --zone="$where" || status=1
    done < <(existing_disks)
    "${cloud[@]}" storage rm --recursive "gs://$bucket/$objects/" > /dev/null 2>&1 || true
    left=$(existing)
    [[ -z "$left" ]] || { echo "Hosts still exist: $left" >&2; status=1; }
    left=$(existing_disks)
    [[ -z "$left" ]] || { echo "Disks still exist: $left" >&2; status=1; }
    left=$("${cloud[@]}" storage ls "gs://$bucket/$objects/" 2>/dev/null || true)
    [[ -z "$left" ]] || { echo "Objects still exist under gs://$bucket/$objects/" >&2; status=1; }
    ((status == 0)) && echo "Verified deletion of every $prefix host, its disks and its objects." >&2
    return "$status"
}

remote() {
    local host=$1
    shift
    "${cloud[@]}" compute ssh "$host" --zone="$zone" --ssh-flag='-o ConnectTimeout=10' --command="$*" < /dev/null
}

run() {
    local host ready status=0 staging index ip members="" binary
    local -a ips=()
    for host in "${hosts[@]}"; do check_owner instances "$host"; done
    check_owner disks "$shard"
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
    staging=$(mktemp -d "${TMPDIR:-/tmp}/sproutfs-move.XXXXXX")
    binary=${SPROUTFS_SHARD_BINARY:-}
    if [[ -z $binary ]]; then
        (cd "$repo" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$staging/sproutfs-shardbench" ./cmd/sproutfs-shardbench)
        binary=$staging/sproutfs-shardbench
    fi
    {
        echo "revision $(git -C "$repo" rev-parse HEAD)"
        git -C "$repo" status --porcelain=v1 -- cmd/sproutfs-shardbench host checkpoint membership platform | sed 's/^/changed /'
        shasum -a 256 "$binary"
        echo "machine $machine, shard $disk_type ${disk_gb}GB, fill ${fill_gb}GB, interval $interval, rounds $rounds," \
            "objects gs://$bucket/$objects"
    } > "$results/source.txt"
    for index in "${!hosts[@]}"; do
        host=${hosts[$index]}
        ip=$("${cloud[@]}" compute instances describe "$host" --zone="$zone" --format='value(networkInterfaces[0].networkIP)')
        ips+=("$ip")
        "${cloud[@]}" compute instances describe "$host" --zone="$zone" \
            --format='json(name,zone,machineType,cpuPlatform,networkInterfaces[].networkIP)' > "$results/instance-$host.json"
        {
            remote "$host" "sudo systemctl stop sproutfs-member 2>/dev/null || true; \
                sudo systemctl reset-failed sproutfs-member 2>/dev/null || true"
            "${cloud[@]}" compute scp --zone="$zone" "$binary" "$host:sproutfs-shardbench"
            remote "$host" "sudo systemd-run --unit=sproutfs-member --property=Restart=always \
                --property=RestartSec=1 --property=LimitNOFILE=65536 \
                \$HOME/sproutfs-shardbench member -advertise $ip:7500 -machine $host -bucket $bucket \
                -prefix $objects -interval $interval"
        } >> "$results/remote.log" 2>&1
        members+="${members:+,}http://$ip:7600"
    done
    rm -rf -- "$staging"
    local volume="projects/$project/zones/$zone/disks/$shard"
    remote "${hosts[0]}" "sudo systemctl reset-failed sproutfs-control 2>/dev/null || true; \
        sudo mkdir -p /var/tmp/sproutfs-control; sudo systemd-run --unit=sproutfs-control \
        --working-directory=/var/tmp/sproutfs-control \
        \$HOME/sproutfs-shardbench control -members $members -volume $volume -bucket $bucket -prefix $objects \
        -interval $interval -fill-bytes $((fill_gb << 30)) -rounds $rounds -out results.json" \
        >> "$results/remote.log" 2>&1 || status=$?
    local state=active unreachable=0
    while [[ $state == active || $state == activating || $state == unknown ]] && ((status == 0)); do
        sleep 20
        if state=$(remote "${hosts[0]}" "systemctl is-active sproutfs-control || true" 2>> "$results/remote.log"); then
            unreachable=0
        else
            state=unknown
            ((++unreachable < 10)) || { echo "${hosts[0]} did not answer ten times." >&2; status=1; }
        fi
    done
    [[ $state == inactive ]] || { echo "The controller ended $state." >&2; status=1; }
    remote "${hosts[0]}" "sudo journalctl -u sproutfs-control --no-pager" > "$results/control.log" 2>&1 || status=$?
    "${cloud[@]}" compute scp --zone="$zone" "${hosts[0]}:/var/tmp/sproutfs-control/results.json" \
        "$results/results.json" >> "$results/remote.log" 2>&1 || status=$?
    for host in "${hosts[@]}"; do
        remote "$host" "sudo journalctl -u sproutfs-member --no-pager" > "$results/member-$host.log" 2>&1 || status=$?
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
