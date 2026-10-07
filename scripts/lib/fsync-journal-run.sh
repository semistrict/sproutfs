#!/usr/bin/env bash
# The durable flush bench's steps on the cluster's server node
# (scripts/bench-fsync-journal-gce.sh), each a command of its own. The bench
# script copies this file to the node and runs one command at a time, since
# what lies between them is its own work: redeploying the hosts, powering a
# node off, adding one.
#
#   hosts N              wait until the orchestrator sees N ready hosts
#   create               create a VM from the alpine template, wait for its
#                        agent, and print its identity
#   place VM HOST        migrate VM to the host pod named HOST, unless it is
#                        there, and wait for a checkpoint of it there
#   flushes VM LABEL     in VM's guest, flushes of 4 KiB by 1, 8 and 64
#                        threads, and writes by 1 and 8 threads with no flush,
#                        each with the host's metrics before and after
#   interval VM LABEL S  one thread flushing for S seconds, with the host's
#                        journal metrics sampled every second
#   marker VM            write 4 KiB of random bytes into the guest's disk,
#                        flush it, and print its sha256
#   check VM SHA         require the marker to hold SHA
#   metrics HOST FILE    the host pod HOST's /metrics, into FILE
#   logs LABEL           every pod's whole log
#   delete VM            delete a VM
#
# Everything is written under FSYNC_BENCH_OUT, /tmp/sproutfs-fsync by default.
set -euo pipefail

export KUBECONFIG=${KUBECONFIG:-/etc/rancher/k3s/k3s.yaml}
namespace=sproutfs
out=${FSYNC_BENCH_OUT:-/tmp/sproutfs-fsync}
seconds=${FSYNC_BENCH_SECONDS:-10s}
[[ $seconds =~ ^[0-9]+s$ ]] || { echo "FSYNC_BENCH_SECONDS is a duration such as 10s" >&2; exit 2; }
witness=/usr/local/bin/sproutfs-guest-witness
mkdir -p "$out"

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
ctl() { kubectl exec -n "$namespace" -i deploy/sproutfs-orchestrator -- sproutfsctl "$@" < /dev/null; }
run_in() { ctl exec "$1" --timeout 300s -- "${@:2}"; }
stamp() { date -u +%FT%T.%3NZ; }

# The token goes into a file only this user reads, never onto a command line.
header=$out/.header
if [[ ! -s $header ]]; then
    (umask 077 && kubectl get secret -n "$namespace" sproutfs-api-token -o jsonpath='{.data.token}' |
        base64 -d | awk '{ printf "Authorization: Bearer %s\n", $0 }' > "$header")
fi

# address is the address of the host pod named $1, on its node's network.
address() {
    kubectl get pod -n "$namespace" "$1" -o jsonpath='{.status.podIP}'
}

# host_of is the host pod that runs VM $1.
host_of() { ctl list | awk -v vm="$1" '$1 == vm && $2 != "-" { print $2 }'; }

metrics() {
    curl -fsS --max-time 20 -H @"$header" "http://$(address "$1"):8080/metrics" > "$2"
}

# checkpoint_of is the checkpoint the record of VM $1 selects.
checkpoint_of() { ctl list | awk -v vm="$1" '$1 == vm { print $4 }'; }

hosts() {
    local want=$1 deadline=$(($(date +%s) + 900)) pod
    until (($(ctl hosts 2> /dev/null | awk 'NR > 1 && $2 == "true"' | wc -l) == want)); do
        (($(date +%s) < deadline)) || { ctl hosts >&2 || true; fail "the orchestrator does not see $want ready hosts"; }
        sleep 2
    done
    ctl hosts
    # Where durable flush is on, a host takes VMs once its journal disk is
    # served.
    for pod in $(ctl hosts | awk 'NR > 1 && $2 == "true" { print $1 }'); do
        until curl -fsS --max-time 5 -H @"$header" "http://$(address "$pod"):8080/metrics" |
            awk '$1 == "sproutfs_durable_flush" { on = $2 } $1 == "sproutfs_journal_served" { served = $2 }
                END { exit !(on == 0 || served == 1) }'; do
            (($(date +%s) < deadline)) || fail "the journal disk of $pod is not served"
            sleep 2
        done
        echo "$(stamp) $pod serves its journal disk or durable flush is off"
    done
}

create() {
    local created vm deadline last=''
    created=$(ctl create --template alpine) || fail "creating a VM failed"
    echo "$(stamp) $created" >> "$out/creates.log"
    vm=${created%% *}
    deadline=$(($(date +%s) + 180))
    until last=$(run_in "$vm" true 2>&1); do
        (($(date +%s) < deadline)) || fail "the agent of $vm did not answer: $last"
        sleep 1
    done
    echo "$vm"
}

place() {
    local vm=$1 want=$2 at before deadline
    at=$(host_of "$vm")
    if [[ $at != "$want" ]]; then
        ctl migrate "$vm" --to "$want" > /dev/null || fail "migrating $vm to $want failed"
    fi
    # A checkpoint taken on the host it is on now names only that host's
    # journal: the destination asks for one once its post-copy ends.
    before=$(checkpoint_of "$vm")
    ctl capture "$vm" > /dev/null || fail "checkpointing $vm failed"
    deadline=$(($(date +%s) + 120))
    until [[ $(checkpoint_of "$vm") != "$before" && $(host_of "$vm") == "$want" ]]; do
        (($(date +%s) < deadline)) || fail "$vm has no new checkpoint on $want"
        sleep 1
    done
}

# flush_case runs one witness flush in the guest with the host's metrics on
# both sides of it.
flush_case() {
    local vm=$1 label=$2 name=$3 host
    shift 3
    host=$(host_of "$vm")
    mkdir -p "$out/$label"
    metrics "$host" "$out/$label/$name.before.prom"
    run_in "$vm" "$witness flush --dir /var/flush-$name --seconds $seconds $*" |
        tee -a "$out/$label/flush.jsonl" | sed "s/^/$name /"
    metrics "$host" "$out/$label/$name.after.prom"
    run_in "$vm" "rm -rf /var/flush-$name" > /dev/null
    echo "$name $host" >> "$out/$label/hosts.txt"
}

flushes() {
    local vm=$1 label=$2
    flush_case "$vm" "$label" sync-1 --threads 1
    flush_case "$vm" "$label" sync-8 --threads 8
    flush_case "$vm" "$label" sync-64 --threads 64 --file 2M
    flush_case "$vm" "$label" write-1 --threads 1 --no-sync
    flush_case "$vm" "$label" write-8 --threads 8 --no-sync
}

interval() {
    local vm=$1 label=$2 length=$3 host address writer
    [[ $length =~ ^[0-9]+$ ]] || fail "the interval is a number of seconds"
    host=$(host_of "$vm")
    address=$(address "$host")
    mkdir -p "$out/$label"
    metrics "$host" "$out/$label/interval.before.prom"
    run_in "$vm" "$witness flush --dir /var/flush-interval --threads 1 --seconds ${length}s" \
        > "$out/$label/interval.json" &
    writer=$!
    {
        printf 'time\tlive_bytes\twritten_bytes\tflushes\tcheckpoints\n'
        while kill -0 "$writer" 2> /dev/null; do
            curl -fsS --max-time 5 -H @"$header" "http://$address:8080/metrics" | awk -v at="$(stamp)" '
                $1 == "sproutfs_journal_live_bytes" { live = $2 }
                $1 == "sproutfs_journal_written_bytes_total" { written = $2 }
                $1 ~ /^sproutfs_journal_flushes_total\{outcome="succeeded"\}/ { flushes = $2 }
                $1 == "sproutfs_checkpoints_total{outcome=\"published\"}" { published = $2 }
                END { printf "%s\t%s\t%s\t%s\t%s\n", at, live, written, flushes, published }' || true
            sleep 1
        done
    } > "$out/$label/interval.tsv"
    wait "$writer" || fail "the interval's writer failed"
    metrics "$host" "$out/$label/interval.after.prom"
    run_in "$vm" "rm -rf /var/flush-interval" > /dev/null
}

marker() {
    local vm=$1
    run_in "$vm" "dd if=/dev/urandom of=/var/marker bs=4096 count=1 conv=fsync 2> /dev/null && sha256sum /var/marker" |
        awk '{ print $1 }'
}

check() {
    local vm=$1 want=$2 got
    got=$(run_in "$vm" "sha256sum /var/marker" | awk '{ print $1 }')
    [[ $got == "$want" ]] || fail "the marker of $vm holds $got, want $want"
    echo "the marker of $vm holds $got"
}

logs() {
    local label=$1 pod
    mkdir -p "$out/$label"
    for pod in $(kubectl get pods -n "$namespace" -o jsonpath='{.items[*].metadata.name}'); do
        kubectl logs -n "$namespace" "$pod" --all-containers --tail=-1 > "$out/$label/$pod.log" 2>&1 || true
    done
    kubectl get pods -n "$namespace" -o wide > "$out/$label/pods.txt" 2>&1 || true
}

command=${1:-}
[[ $# -eq 0 ]] || shift
case $command in
    hosts) hosts "$@" ;;
    create) create ;;
    place) place "$@" ;;
    flushes) flushes "$@" ;;
    interval) interval "$@" ;;
    marker) marker "$@" ;;
    check) check "$@" ;;
    metrics) metrics "$@" ;;
    logs) logs "$@" ;;
    delete) ctl delete "$1" ;;
    list) ctl list ;;
    *) echo "Usage: $0 hosts|create|place|flushes|interval|marker|check|metrics|logs|delete|list ..." >&2; exit 2 ;;
esac
