#!/usr/bin/env bash
# The start latency bench's run, on the cluster's server node.
#
# scripts/bench-start-gce.sh copies this file and sproutfs-startbench to the
# node and runs it once the hosts are up. It:
#
#   1. waits for every host pod to be ready, which is once it has imported the
#      template;
#   2. runs sproutfs-startbench drive against the hosts' API, on the node's
#      network: START_BENCH_COUNT starts of each case in START_BENCH_CASES;
#   3. runs sproutfs-startbench store against the bench's bucket, under the
#      run's own prefix, which the bench script deletes;
#   4. keeps every host's status and the whole log of every pod, which holds
#      each start's "host: a VM runs" line.
#
# Everything is written under START_BENCH_OUT.
#
#   bash start-run.sh <bucket> <objects prefix>
set -euo pipefail

export KUBECONFIG=${KUBECONFIG:-/etc/rancher/k3s/k3s.yaml}
namespace=sproutfs
out=${START_BENCH_OUT:-/tmp/sproutfs-start}
count=${START_BENCH_COUNT:-300}
cases=${START_BENCH_CASES:-cold,restore,fork-local,fork-remote}
interval=${START_BENCH_INTERVAL:-250ms}
bucket=${1:?the bucket}
objects=${2:?the objects prefix}
[[ $count =~ ^[0-9]+$ ]] || { echo "START_BENCH_COUNT is a number" >&2; exit 2; }
[[ $cases =~ ^[a-z,-]+$ ]] || { echo "START_BENCH_CASES names cases, separated by commas" >&2; exit 2; }
[[ $interval =~ ^[0-9]+(ms|s)$ ]] || { echo "START_BENCH_INTERVAL is a duration such as 250ms" >&2; exit 2; }
bench=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/sproutfs-startbench
[[ -x $bench ]] || { echo "No sproutfs-startbench beside this script." >&2; exit 2; }

step() { printf '\n=== %s ===\n' "$*"; }
fail() { printf '\nFAIL: %s\n' "$*" >&2; exit 1; }

mkdir -p "$out"
work=$(mktemp -d /tmp/sproutfs-start.XXXXXX)
chmod 0700 "$work"
trap 'rm -rf -- "$work"' EXIT
# The token goes into files only this user can read, rather than onto a
# command line every process on the node can list.
kubectl get secret -n "$namespace" sproutfs-api-token -o jsonpath='{.data.token}' | base64 -d > "$work/token"
awk '{ printf "Authorization: Bearer %s\n", $0 }' "$work/token" > "$work/header"

# The host pods' addresses, in pod name order. They run on their nodes' own
# network, so this node reaches each one's API at its address.
pod_addresses() {
    kubectl get pods -n "$namespace" -l app.kubernetes.io/name=sproutfs-host \
        --field-selector=status.phase=Running \
        -o jsonpath='{range .items[*]}{.metadata.name} {.status.podIP}{"\n"}{end}' | sort
}

step "waiting for the hosts"
kubectl rollout status -n "$namespace" deployment/sproutfs-host --timeout=900s
want=$(kubectl get deployment -n "$namespace" sproutfs-host -o jsonpath='{.spec.replicas}')
deadline=$(($(date +%s) + 900))
while :; do
    ready=0
    while read -r _ address; do
        if curl -fsS --max-time 5 -H @"$work/header" "http://$address:8080/healthz" > /dev/null 2>&1; then
            ready=$((ready + 1))
        fi
    done < <(pod_addresses)
    ((ready == want)) && break
    (($(date +%s) < deadline)) || fail "only $ready of $want hosts are ready"
    sleep 5
done
pod_addresses | tee "$out/pods.txt"
hosts=$(awk '{ printf "%shttp://%s:8080", (NR > 1 ? "," : ""), $2 }' "$out/pods.txt")

snapshot() {
    local pod address
    while read -r pod address; do
        curl -fsS --max-time 20 -H @"$work/header" "http://$address:8080/status" > "$out/$1.$pod.status.json"
    done < "$out/pods.txt"
}

snapshot before
tag=$(date -u +%H%M%S)
step "driving $cases, $count starts each, tag $tag"
status=0
"$bench" drive -hosts "$hosts" -token-file "$work/token" -template alpine -cases "$cases" -count "$count" \
    -tag "$tag" -interval "$interval" -out "$out/samples.jsonl" || status=$?
snapshot after

step "timing the store's calls"
"$bench" store -bucket "$bucket" -prefix "$objects/storebench/" -count "$count" -out "$out/store.json" ||
    status=$?

# Every pod's whole log: each start's line, each fork's and each first faults.
while read -r pod _; do
    echo "=== pod/$pod"
    kubectl logs -n "$namespace" "$pod" --tail=-1
done < "$out/pods.txt" > "$out/pods.log" 2>&1
echo "=== orchestrator" >> "$out/pods.log"
kubectl logs -n "$namespace" deployment/sproutfs-orchestrator --tail=-1 >> "$out/pods.log" 2>&1 || true
exit "$status"
