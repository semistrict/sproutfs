#!/usr/bin/env bash
# The peer server's measurement on two disposable GCE hosts: a guest fault's
# latency while a post-copy stream fills the link, with the release before's
# page server and with this release's peer server, and stripe reads over the
# peer server beside both (docs/measurements/gce-peer-server-2026-10-03.md).
#
# One host serves a VM of four memory regions of 2 MiB pages of noise, and
# the other faults one page of each region every SPROUTFS_PEER_FAULT_EVERY
# (100ms by default) while the stream fetches every page. Each case times
# faults for SPROUTFS_PEER_DURATION (30s by default) after the stream has had
# two seconds to get going. Each round
# runs every case of both builds once, in an order that alternates by round,
# and SPROUTFS_PEER_ROUNDS rounds run (three by default). The host that
# serves also runs the transport's Linux-only tests: sendfile and
# TCP_USER_TIMEOUT.
#
# The release before is main at 6af0d3de, built with the same workload
# (cmd/sproutfs-peerbench/workload.go and scripts/lib/peerbench-before_test.go.txt)
# as vmmigrate's test binary, because its page server and its client are
# internal to vmmigrate.
#
# `all` always deletes the hosts. `create`, `run` and `delete` expose the same
# steps. Each host also deletes itself after two hours, whatever this shell
# does. SPROUTFS_PEER_MACHINE is the machine type (default n2-standard-4).
set -euo pipefail
[[ ${SPROUTFS_PEER_DURATION:-} =~ ^([0-9]+(s|m))?$ ]] || { echo "SPROUTFS_PEER_DURATION is a duration such as 30s" >&2; exit 2; }
[[ ${SPROUTFS_PEER_ROUNDS:-} =~ ^[0-9]*$ ]] || { echo "SPROUTFS_PEER_ROUNDS is a number" >&2; exit 2; }
[[ ${SPROUTFS_PEER_FAULT_EVERY:-} =~ ^([0-9]+(ms|s))?$ ]] || { echo "SPROUTFS_PEER_FAULT_EVERY is a duration such as 100ms" >&2; exit 2; }
duration=${SPROUTFS_PEER_DURATION:-30s}
rounds=${SPROUTFS_PEER_ROUNDS:-3}
every=${SPROUTFS_PEER_FAULT_EVERY:-100ms}
machine=${SPROUTFS_PEER_MACHINE:-n2-standard-4}
[[ $machine =~ ^n2-standard-[0-9]+$ ]] || { echo "SPROUTFS_PEER_MACHINE is an n2-standard machine type" >&2; exit 2; }
before_revision=6af0d3de

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
project=${SPROUTFS_GCE_PROJECT:-$(gcloud config get-value project 2>/dev/null)}
zone=${SPROUTFS_GCE_ZONE:-us-east4-a}
action=${1:-all}
prefix=${2:-sproutfs-peer-$(date -u +%Y%m%d-%H%M%S)}
results=${3:-$repo/docs/measurements/gce-peer-server-$(date -u +%Y-%m-%d)}
[[ -n "$project" && "$project" != '(unset)' ]] || { echo 'Set SPROUTFS_GCE_PROJECT.' >&2; exit 2; }
[[ "$prefix" =~ ^sproutfs-peer-[a-z0-9-]+$ ]] || { echo 'Use a sproutfs-peer- name prefix.' >&2; exit 2; }
cloud=(gcloud --quiet --project="$project")
hosts=("$prefix-0" "$prefix-1")

create() {
    "${cloud[@]}" compute instances create "${hosts[@]}" --zone="$zone" \
        --machine-type="$machine" --min-cpu-platform='Intel Cascade Lake' \
        --image=ubuntu-2604-resolute-amd64-v20260907 --image-project=ubuntu-os-cloud \
        --boot-disk-size=20GB --boot-disk-type=pd-balanced --boot-disk-auto-delete \
        --network="${SPROUTFS_GCE_NETWORK:-default}" --no-service-account --no-scopes \
        --metadata=block-project-ssh-keys=TRUE \
        --metadata-from-file="startup-script=$repo/scripts/lib/gce-bench-startup.sh" \
        --labels=purpose=peer-bench,lifecycle=temporary \
        --maintenance-policy=TERMINATE --no-restart-on-failure \
        --max-run-duration=2h --instance-termination-action=DELETE
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
    [[ "$labels" == $'peer-bench\ttemporary' ]] || {
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

# build makes this release's bench and test binaries, and the release before's
# bench, for linux/amd64, in staging.
build() {
    local staging=$1 before
    (cd "$repo" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$staging/sproutfs-peerbench" ./cmd/sproutfs-peerbench)
    (cd "$repo" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c -o "$staging/framer.test" ./platform/internal/framer)
    (cd "$repo" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c -o "$staging/real.test" ./platform/internal/real)
    (cd "$repo" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c -o "$staging/peer.test" ./peer)
    before=$staging/before
    mkdir -p "$before"
    git -C "$repo" archive "$before_revision" | tar -x -C "$before"
    cp "$repo/scripts/lib/peerbench-before_test.go.txt" "$before/vmmigrate/peerbench_before_test.go"
    sed 's/^package main$/package vmmigrate/' "$repo/cmd/sproutfs-peerbench/workload.go" \
        > "$before/vmmigrate/peerbench_workload_test.go"
    (cd "$before" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c -o "$staging/peerbench-before" ./vmmigrate)
}

run() {
    local host ready status=0 staging server_ip round order build name
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

    staging=$(mktemp -d "${TMPDIR:-/tmp}/sproutfs-peer.XXXXXX")
    build "$staging"
    {
        echo "revision $(git -C "$repo" rev-parse HEAD), before $before_revision"
        git -C "$repo" status --porcelain=v1 -- cmd/sproutfs-peerbench peer platform scripts | sed 's/^/changed /'
        (cd "$staging" && shasum -a 256 sproutfs-peerbench peerbench-before)
        echo "machine $machine, duration $duration, rounds $rounds, a fault of each region every $every"
    } > "$results/source.txt"
    for host in "${hosts[@]}"; do
        "${cloud[@]}" compute instances describe "$host" --zone="$zone" \
            --format='json(name,zone,machineType,cpuPlatform,networkInterfaces[].networkIP)' > "$results/instance-$host.json"
        {
            remote "$host" "test -e peer-host.sh && bash peer-host.sh stop || true"
            "${cloud[@]}" compute scp --zone="$zone" "$repo/scripts/lib/peer-host.sh" \
                "$staging/sproutfs-peerbench" "$staging/peerbench-before" "$staging/framer.test" \
                "$staging/real.test" "$staging/peer.test" "$host:"
        } >> "$results/remote.log" 2>&1
    done
    rm -rf -- "$staging"
    server_ip=$("${cloud[@]}" compute instances describe "${hosts[1]}" --zone="$zone" --format='value(networkInterfaces[0].networkIP)')

    echo "Linux-only transport tests on ${hosts[1]}." >&2
    remote "${hosts[1]}" "bash peer-host.sh linux-tests" >> "$results/remote.log" 2>&1 || status=$?

    for ((round = 0; round < rounds; round++)); do
        order=(before after)
        ((round % 2 == 1)) && order=(after before)
        for build in "${order[@]}"; do
            echo "Round $round, $build." >&2
            remote "${hosts[1]}" "bash peer-host.sh serve $build" >> "$results/remote.log" 2>&1 || { status=$?; continue; }
            local -a cases=(idle stream)
            [[ $build == after ]] && cases+=(stripes stream-stripes)
            for name in "${cases[@]}"; do
                local ticks_before ticks_after began ended
                ticks_before=$(remote "${hosts[1]}" "bash peer-host.sh cpu" 2>> "$results/remote.log") || status=$?
                began=$(date +%s)
                remote "${hosts[0]}" "bash peer-host.sh read $build $name $server_ip:7500 $duration $every" \
                    >> "$results/remote.log" 2>&1 || status=$?
                ended=$(date +%s)
                ticks_after=$(remote "${hosts[1]}" "bash peer-host.sh cpu" 2>> "$results/remote.log") || status=$?
                # The server's CPU time over the case, in clock ticks, and the
                # case's wall time in seconds, ssh included.
                echo "$round $build $name $((ticks_after - ticks_before)) $((ended - began))" >> "$results/server-cpu.txt"
            done
            remote "${hosts[1]}" "bash peer-host.sh stop" >> "$results/remote.log" 2>&1 || status=$?
        done
    done

    for host in "${hosts[@]}"; do
        mkdir -p "$results/$host"
        "${cloud[@]}" compute scp --recurse --zone="$zone" "$host:results/." "$results/$host/" \
            >> "$results/remote.log" 2>&1 || status=$?
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
