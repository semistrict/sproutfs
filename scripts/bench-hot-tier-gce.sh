#!/usr/bin/env bash
# Dependent single reads on disposable GCE hosts against real buckets: from
# the regional bucket, from a hot tier in a second bucket, and from the
# hosts' cluster cache, and the hot tier's fills on a cold run
# (docs/measurements/gce-hot-tier-2026-10-03.md).
#
# Every host runs `sproutfs-restorebench node` with the hot tier's bucket:
# the real checkpoint store, page cache, peer server and table of peers, its
# cache on a local NVMe SSD. The second host runs `sproutfs-restorebench walk`,
# which publishes from the first, then reads one page at a time on the second.
#
# SPROUTFS_GCE_BUCKET names the regional bucket, and SPROUTFS_GCE_SERVICE_ACCOUNT
# the account the hosts reach both buckets as. The hot tier's bucket is made
# for the run in SPROUTFS_HOT_TIER_LOCATION (the hosts' region by default),
# and deleted with every object of the run afterwards.
# SPROUTFS_WALK_HOSTS is how many hosts run (six by default, at least two),
# SPROUTFS_WALK_CODE the cluster's code (4+2 by default), SPROUTFS_WALK_READS
# the reads of one walk, SPROUTFS_WALK_PAGES and SPROUTFS_WALK_PAGES_4K the
# guests' pages, and SPROUTFS_WALK_ROUNDS the rounds.
#
# `all` always deletes the hosts and the hot tier's bucket. `create`, `run`
# and `delete` expose the same steps. Each host also deletes itself after
# three hours.
set -euo pipefail
for name in SPROUTFS_WALK_HOSTS SPROUTFS_WALK_READS SPROUTFS_WALK_PAGES SPROUTFS_WALK_PAGES_4K SPROUTFS_WALK_ROUNDS; do
    [[ ${!name:-} =~ ^[0-9]*$ ]] || { echo "$name is a number" >&2; exit 2; }
done
count=${SPROUTFS_WALK_HOSTS:-6}
code=${SPROUTFS_WALK_CODE:-4+2}
reads=${SPROUTFS_WALK_READS:-500}
pages=${SPROUTFS_WALK_PAGES:-1024}
pages4k=${SPROUTFS_WALK_PAGES_4K:-131072}
rounds=${SPROUTFS_WALK_ROUNDS:-3}
((count >= 2)) || { echo "SPROUTFS_WALK_HOSTS is at least 2" >&2; exit 2; }
[[ $code =~ ^[0-9]+\+[0-9]+$ ]] || { echo "SPROUTFS_WALK_CODE is k+m" >&2; exit 2; }
machine=${SPROUTFS_RESTORE_MACHINE:-n2-standard-4}
[[ $machine =~ ^n2-standard-[0-9]+$ ]] || { echo "SPROUTFS_RESTORE_MACHINE is an n2-standard machine type" >&2; exit 2; }
bucket=${SPROUTFS_GCE_BUCKET:-}
account=${SPROUTFS_GCE_SERVICE_ACCOUNT:-}
[[ -n $bucket && -n $account ]] || { echo "Set SPROUTFS_GCE_BUCKET and SPROUTFS_GCE_SERVICE_ACCOUNT." >&2; exit 2; }

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
project=${SPROUTFS_GCE_PROJECT:-$(gcloud config get-value project 2>/dev/null)}
zone=${SPROUTFS_GCE_ZONE:-us-east4-a}
location=${SPROUTFS_HOT_TIER_LOCATION:-${zone%-*}}
action=${1:-all}
prefix=${2:-sproutfs-hot-$(date -u +%Y%m%d-%H%M%S)}
results=${3:-$repo/docs/measurements/gce-hot-tier-$(date -u +%Y-%m-%d)}
[[ -n "$project" && "$project" != '(unset)' ]] || { echo 'Set SPROUTFS_GCE_PROJECT.' >&2; exit 2; }
[[ "$prefix" =~ ^sproutfs-hot-[a-z0-9-]+$ ]] || { echo 'Use a sproutfs-hot- name prefix.' >&2; exit 2; }
cloud=(gcloud --quiet --project="$project")
hosts=()
for ((index = 0; index < count; index++)); do hosts+=("$prefix-$index"); done
objects="sproutfs-bench/$prefix"
hot_bucket="$prefix-hot"

create() {
    "${cloud[@]}" storage buckets create "gs://$hot_bucket" --location="$location" \
        --default-storage-class=STANDARD --uniform-bucket-level-access
    "${cloud[@]}" storage buckets update "gs://$hot_bucket" \
        --update-labels=purpose=hot-tier-bench,lifecycle=temporary > /dev/null
    "${cloud[@]}" storage buckets add-iam-policy-binding "gs://$hot_bucket" \
        --member="serviceAccount:$account" --role=roles/storage.objectAdmin > /dev/null
    "${cloud[@]}" compute instances create "${hosts[@]}" --zone="$zone" \
        --machine-type="$machine" --min-cpu-platform='Intel Cascade Lake' \
        --image=ubuntu-2604-resolute-amd64-v20260907 --image-project=ubuntu-os-cloud \
        --boot-disk-size=20GB --boot-disk-type=pd-balanced --boot-disk-auto-delete \
        --local-ssd=interface=NVME \
        --network="${SPROUTFS_GCE_NETWORK:-default}" \
        --service-account="$account" --scopes=storage-rw \
        --metadata=block-project-ssh-keys=TRUE \
        --metadata-from-file="startup-script=$repo/scripts/lib/gce-bench-startup.sh" \
        --labels=purpose=hot-tier-bench,lifecycle=temporary \
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
    [[ "$labels" == $'hot-tier-bench\ttemporary' ]] || {
        echo "Refusing to operate on $1, which lacks the benchmark ownership labels." >&2
        return 1
    }
}

delete() {
    local name where left where_of="" labels
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
    if "${cloud[@]}" storage buckets describe "gs://$hot_bucket" > /dev/null 2>&1; then
        labels=$("${cloud[@]}" storage buckets describe "gs://$hot_bucket" --format='value(labels.purpose,labels.lifecycle)')
        [[ "$labels" == *hot-tier-bench* && "$labels" == *temporary* ]] || {
            echo "Refusing to delete gs://$hot_bucket, which lacks the benchmark ownership labels." >&2
            return 1
        }
        "${cloud[@]}" storage rm --recursive "gs://$hot_bucket" > /dev/null
    fi
    if "${cloud[@]}" storage buckets describe "gs://$hot_bucket" > /dev/null 2>&1; then
        echo "The hot tier's bucket gs://$hot_bucket still exists." >&2
        return 1
    fi
    echo "Verified deletion of every $prefix host, its disks, its objects and gs://$hot_bucket." >&2
}

remote() {
    local host=$1
    shift
    "${cloud[@]}" compute ssh "$host" --zone="$zone" --ssh-flag='-o ConnectTimeout=10' --command="$*" < /dev/null
}

run() {
    local host ready status=0 staging index ip nodes="" run_objects
    run_objects="$objects/$(date -u +%Y%m%dT%H%M%SZ)"
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
    staging=$(mktemp -d "${TMPDIR:-/tmp}/sproutfs-hot.XXXXXX")
    (cd "$repo" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$staging/sproutfs-restorebench" ./cmd/sproutfs-restorebench)
    {
        echo "revision $(git -C "$repo" rev-parse HEAD)"
        git -C "$repo" status --porcelain=v1 -- cmd/sproutfs-restorebench checkpoint peer rank stripe | sed 's/^/changed /'
        (cd "$staging" && shasum -a 256 sproutfs-restorebench)
        echo "machine $machine, hosts $count, code $code, pages $pages, pages-4k $pages4k, reads $reads, rounds $rounds"
        echo "regional gs://$bucket/$run_objects ($("${cloud[@]}" storage buckets describe "gs://$bucket" --format='value(location,location_type)'))"
        echo "hot tier gs://$hot_bucket/$run_objects ($("${cloud[@]}" storage buckets describe "gs://$hot_bucket" --format='value(location,location_type)'))"
    } > "$results/source.txt"
    for index in "${!hosts[@]}"; do
        host=${hosts[$index]}
        ip=$("${cloud[@]}" compute instances describe "$host" --zone="$zone" --format='value(networkInterfaces[0].networkIP)')
        "${cloud[@]}" compute instances describe "$host" --zone="$zone" \
            --format='json(name,zone,machineType,cpuPlatform,networkInterfaces[].networkIP)' > "$results/instance-$host.json"
        {
            remote "$host" "sudo systemctl stop sproutfs-node 2>/dev/null || true; \
                sudo systemctl reset-failed sproutfs-node 2>/dev/null || true"
            "${cloud[@]}" compute scp --zone="$zone" "$staging/sproutfs-restorebench" "$host:"
            remote "$host" "set -e; if mountpoint -q /mnt/ssd; then sudo umount /mnt/ssd; fi; \
                ssd=\$(ls /dev/disk/by-id/google-local-nvme-ssd-0); \
                sudo mkfs.ext4 -q -F \$ssd; sudo mkdir -p /mnt/ssd; sudo mount \$ssd /mnt/ssd; \
                sudo systemd-run --unit=sproutfs-node --property=LimitNOFILE=65536 \
                \$HOME/sproutfs-restorebench node -advertise $ip:7500 -bucket $bucket -prefix $run_objects \
                -hot-bucket $hot_bucket -dir /mnt/ssd"
        } >> "$results/remote.log" 2>&1
        nodes+="${nodes:+,}$ip:7600"
    done
    rm -rf -- "$staging"
    sleep 5
    echo "Walking from ${hosts[1]}." >&2
    remote "${hosts[1]}" "./sproutfs-restorebench walk -nodes $nodes -code $code -pages $pages -pages-4k $pages4k \
        -reads $reads -rounds $rounds -out walk.json" > "$results/walk.log" 2>&1 || status=$?
    "${cloud[@]}" compute scp --zone="$zone" "${hosts[1]}:walk.json" "$results/walk.json" \
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
