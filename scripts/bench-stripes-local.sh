#!/usr/bin/env bash
# The stripe benchmark on this machine: six servers on loopback ports and one
# client, every code under every condition in both read modes, in about a
# minute and a half. It checks the program end to end over real TCP before a
# GCE run; its numbers say nothing about our hosts.
#
# Usage: scripts/bench-stripes-local.sh [results-directory]
set -euo pipefail
repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
results=${1:-$repo/docs/measurements/local-stripes-$(date -u +%Y%m%d-%H%M%S)}
mkdir -p "$results"
work=$(mktemp -d "${TMPDIR:-/tmp}/sproutfs-stripes.XXXXXX")
pids=()
cleanup() {
    local status=$?
    for pid in "${pids[@]}"; do
        kill "$pid" 2> /dev/null || true
    done
    for pid in "${pids[@]}"; do
        wait "$pid" 2> /dev/null || true
    done
    rm -f -- "$work"/store-* "$work/stripebench"
    rmdir -- "$work"
    exit "$status"
}
trap cleanup EXIT

(cd "$repo" && go build -o "$work/stripebench" ./cmd/sproutfs-stripebench)
objects=${SPROUTFS_STRIPES_OBJECTS:-256}
servers=()
for i in 0 1 2 3 4 5; do
    port=$((17401 + i))
    "$work/stripebench" server -listen "127.0.0.1:$port" -index "$i" -servers 6 \
        -objects "$objects" -file "$work/store-$i" > "$results/server-$i.log" 2>&1 &
    pids+=("$!")
    servers+=("127.0.0.1:$port")
done
list=$(IFS=,; echo "${servers[*]}")
"$work/stripebench" client -servers "$list" -objects "$objects" -name local \
    -conditions healthy,slow,stall,drained,drained-slow,drained-stall \
    -rate "${SPROUTFS_STRIPES_RATE:-200}" -duration 1500ms -gap 1100ms -warmup 1s \
    -dial-wait 30s -out "$results/local" 2> "$results/client.log"
