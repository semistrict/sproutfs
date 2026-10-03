#!/usr/bin/env bash
# The disk cache plan's first measurement on disposable GCE hosts: one 350 KB
# read from one host against 4+1 and 4+2 reads of its stripes, at the median
# and the tail (plans/disk-cache-2026-10-02.md, "How it is proved").
#
# Six hosts in one zone (n2-standard-8 unless SPROUTFS_STRIPES_MACHINE says
# otherwise), each with one local NVMe SSD, each
# serving its stripes of the same objects (cmd/sproutfs-stripebench). Four
# passes, each of every code under every condition (healthy, slow, drained,
# drained and slow, drained and stalled):
#
#   idle-memory  one host reads; the servers answer from the page cache
#   full-memory  all six hosts read at once at the full rate
#   idle-disk    one host reads; every stripe read reads the SSD
#   full-disk    all six read at once; every stripe read reads the SSD
#
# `all` always deletes the hosts. `create`, `run` and `delete` expose the
# same steps for an interrupted run. Each host also deletes itself after a
# few hours, whatever this shell does.
#
# SPROUTFS_STRIPES_DURATION is each case's length (default 20s).
# SPROUTFS_STRIPES_IDLE_RATE, _FULL_RATE and _DISK_RATE are reads per second
# per reading host for the idle passes, the full-memory pass and the
# full-disk pass (defaults 500, 1500 and 500). The disk pass reads less: at
# 500 a second from six hosts, each SSD serves about 260 MB/s of 4+2 stripes,
# well inside one local SSD's 660 MB/s.
# SPROUTFS_STRIPES_MACHINE is the hosts' machine type (default n2-standard-8).
# A project with a small CPU quota runs n2-standard-4, at 10 Gbps rather
# than 16.
set -euo pipefail
[[ ${SPROUTFS_STRIPES_DURATION:-} =~ ^([0-9]+(ms|s|m))?$ ]] || { echo "SPROUTFS_STRIPES_DURATION is a duration such as 20s" >&2; exit 2; }
for knob in SPROUTFS_STRIPES_IDLE_RATE SPROUTFS_STRIPES_FULL_RATE SPROUTFS_STRIPES_DISK_RATE SPROUTFS_STRIPES_OBJECTS; do
    [[ ${!knob:-} =~ ^[0-9]*$ ]] || { echo "$knob is a number" >&2; exit 2; }
done
duration=${SPROUTFS_STRIPES_DURATION:-20s}
idle_rate=${SPROUTFS_STRIPES_IDLE_RATE:-500}
full_rate=${SPROUTFS_STRIPES_FULL_RATE:-1500}
disk_rate=${SPROUTFS_STRIPES_DISK_RATE:-500}
objects=${SPROUTFS_STRIPES_OBJECTS:-4096}
machine=${SPROUTFS_STRIPES_MACHINE:-n2-standard-8}
[[ $machine =~ ^n2-standard-[0-9]+$ ]] || { echo "SPROUTFS_STRIPES_MACHINE is an n2-standard machine type" >&2; exit 2; }
conditions=healthy,slow,drained,drained-slow,drained-stall

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
project=${SPROUTFS_GCE_PROJECT:-$(gcloud config get-value project 2>/dev/null)}
zone=${SPROUTFS_GCE_ZONE:-us-east4-a}
action=${1:-all}
prefix=${2:-sproutfs-stripes-$(date -u +%Y%m%d-%H%M%S)}
results=${3:-$repo/docs/measurements/gce-stripes-$(date -u +%Y-%m-%d)}
[[ -n "$project" && "$project" != '(unset)' ]] || { echo 'Set SPROUTFS_GCE_PROJECT.' >&2; exit 2; }
[[ "$prefix" =~ ^sproutfs-stripes-[a-z0-9-]+$ ]] || { echo 'Use a sproutfs-stripes- name prefix.' >&2; exit 2; }
cloud=(gcloud --quiet --project="$project")
count=6
port=7400
hosts=()
for ((i = 0; i < count; i++)); do hosts+=("$prefix-$i"); done

# create makes the six hosts in SPROUTFS_GCE_ZONE, or where that zone has no
# room for them, in the next zone of the same region that does. All six are
# always in one zone. Hosts a failed attempt made are deleted before the next.
create() {
    local region candidates candidate log
    region=${zone%-*}
    candidates=("$zone")
    while read -r candidate; do
        [[ "$candidate" == "$zone" ]] || candidates+=("$candidate")
    done < <("${cloud[@]}" compute zones list --filter="region:$region" --format='value(name)' | sort)
    log=$(mktemp)
    for candidate in "${candidates[@]}"; do
        if create_in "$candidate" 2> "$log"; then
            zone=$candidate
            echo "Created ${hosts[*]} in $zone." >&2
            rm -f -- "$log"
            return 0
        fi
        # Read in the shell rather than with grep, which this machine's PATH
        # may put a wrapper in front of.
        if ! [[ $(< "$log") =~ ZONE_RESOURCE_POOL_EXHAUSTED|does\ not\ have\ enough\ resources ]]; then
            cat "$log" >&2
            rm -f -- "$log"
            return 1
        fi
        echo "$candidate has no room for six hosts; trying the next zone." >&2
        delete
    done
    cat "$log" >&2
    rm -f -- "$log"
    return 1
}

create_in() {
    "${cloud[@]}" compute instances create "${hosts[@]}" --zone="$1" \
        --machine-type="$machine" --min-cpu-platform='Intel Cascade Lake' \
        --image=ubuntu-2604-resolute-amd64-v20260907 --image-project=ubuntu-os-cloud \
        --boot-disk-size=20GB --boot-disk-type=pd-balanced --boot-disk-auto-delete \
        --local-ssd=interface=NVME \
        --network="${SPROUTFS_GCE_NETWORK:-default}" --no-service-account --no-scopes \
        --metadata=block-project-ssh-keys=TRUE \
        --metadata-from-file="startup-script=$repo/scripts/lib/gce-bench-startup.sh" \
        --labels=purpose=stripe-bench,lifecycle=temporary \
        --maintenance-policy=TERMINATE --no-restart-on-failure \
        --max-run-duration=4h --instance-termination-action=DELETE
}

# existing lists this run's hosts that exist, as name and zone.
existing() {
    "${cloud[@]}" compute instances list --filter="name ~ ^$prefix-[0-9]+\$" --format='value(name,zone.basename())'
}

# locate points zone at the zone the hosts are in, which create may have
# chosen.
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
    [[ "$labels" == $'stripe-bench\ttemporary' ]] || {
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
    echo "Verified deletion of every $prefix host and its disks." >&2
}

remote() {
    local host=$1
    shift
    "${cloud[@]}" compute ssh "$host" --zone="$zone" --ssh-flag='-o ConnectTimeout=10' --command="$*" < /dev/null
}

# pass runs one pass of the client: on the first host, or on every host at
# once from a common start, and returns the first failure.
pass() {
    local name=$1 together=$2 servers=$3 status=0 pid
    shift 3
    echo "Pass $name." >&2
    if [[ $together == one ]]; then
        remote "${hosts[0]}" "bash stripes-host.sh read $name $servers $*" >> "$results/remote.log" 2>&1 || status=$?
        return "$status"
    fi
    # Every client hashes its objects and dials every server before the start.
    local start
    start=$(( $(date +%s) * 1000 + 45000 ))
    local -a pids=()
    for host in "${hosts[@]}"; do
        remote "$host" "bash stripes-host.sh read $name $servers -start-at $start $*" >> "$results/remote-$host.log" 2>&1 &
        pids+=("$!")
    done
    for pid in "${pids[@]}"; do
        wait "$pid" || status=$?
    done
    return "$status"
}

run() {
    local host ready status=0 staging servers i
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

    staging=$(mktemp -d "${TMPDIR:-/tmp}/sproutfs-stripes.XXXXXX")
    (cd "$repo" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$staging/sproutfs-stripebench" ./cmd/sproutfs-stripebench)
    {
        echo "revision $(git -C "$repo" rev-parse HEAD)"
        git -C "$repo" status --porcelain=v1 -- cmd/sproutfs-stripebench scripts | sed 's/^/changed /'
        (cd "$staging" && shasum -a 256 sproutfs-stripebench)
        echo "objects $objects, duration $duration, idle $idle_rate/s, full $full_rate/s, disk $disk_rate/s"
    } > "$results/source.txt"

    servers=""
    for ((i = 0; i < count; i++)); do
        host=${hosts[$i]}
        "${cloud[@]}" compute instances describe "$host" --zone="$zone" \
            --format='json(name,zone,machineType,cpuPlatform,scheduling,disks[].interface,networkInterfaces[].networkIP)' \
            > "$results/instance-$i.json"
        servers+="${servers:+,}$("${cloud[@]}" compute instances describe "$host" --zone="$zone" --format='value(networkInterfaces[0].networkIP)'):$port"
        "${cloud[@]}" compute scp --zone="$zone" "$staging/sproutfs-stripebench" "$repo/scripts/lib/stripes-host.sh" "$host:" \
            >> "$results/remote.log" 2>&1
        remote "$host" "bash stripes-host.sh serve $i $count $objects" >> "$results/remote.log" 2>&1
    done
    rm -f -- "$staging/sproutfs-stripebench"
    rmdir -- "$staging"

    local common=(-objects "$objects" -conditions "$conditions" -duration "$duration")
    pass idle-memory one "$servers" -load idle -rate "$idle_rate" "${common[@]}" || status=$?
    pass full-memory all "$servers" -load full -rate "$full_rate" "${common[@]}" || status=$?
    pass idle-disk one "$servers" -load idle -disk -rate "$idle_rate" "${common[@]}" || status=$?
    pass full-disk all "$servers" -load full -disk -rate "$disk_rate" "${common[@]}" || status=$?

    for ((i = 0; i < count; i++)); do
        mkdir -p "$results/host-$i"
        "${cloud[@]}" compute scp --recurse --zone="$zone" "${hosts[$i]}:results/." "$results/host-$i/" \
            >> "$results/remote.log" 2>&1 || status=$?
    done
    for name in idle-memory idle-disk; do
        if [[ -e "$results/host-0/$name.json" ]]; then
            cp "$results/host-0/$name.json" "$results/host-0/$name.txt" "$results/"
        fi
    done
    for name in full-memory full-disk; do
        local -a records=()
        for ((i = 0; i < count; i++)); do
            [[ -e "$results/host-$i/$name.json" ]] && records+=("$results/host-$i/$name.json")
        done
        if ((${#records[@]} > 0)); then
            (cd "$repo" && go run ./cmd/sproutfs-stripebench report -out "$results/$name" "${records[@]}") > /dev/null || status=$?
        fi
    done
    return "$status"
}

case "$action" in
    create) create ;;
    run) locate; run ;;
    delete) delete ;;
    all)
        [[ -z "$(existing)" ]] || { echo "Hosts named $prefix-* already exist." >&2; exit 1; }
        # The hosts' own deadline also covers this shell being killed. Set the
        # trap before creating them to cover an ambiguous create.
        trap 'status=$?; trap - EXIT; if ! delete; then exit 1; fi; exit "$status"' EXIT
        create
        run
        ;;
    *) echo "Usage: $0 [all|create|run|delete] [name-prefix] [results-directory]" >&2; exit 2 ;;
esac
