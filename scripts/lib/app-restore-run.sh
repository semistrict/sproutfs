#!/usr/bin/env bash
# The rounds of the application restore bench, on the cluster's server node.
#
# scripts/bench-app-restore-gce.sh copies this file to the node and runs it
# there once the hosts are up. Each case of each round:
#
#   1. creates a VM from the valkey template, starts Valkey in it and loads it
#      with sproutfs-guest-chase: string keys whose values name the next key
#      of one random cycle, and a sorted set whose ranks are a second random
#      order (cmd/sproutfs-guest-chase);
#   2. lets it settle, then suspends it, which publishes its memory, and
#      waits for the publication's fills of the cluster to end;
#   3. drops the kernel's page cache on every node, restores the VM on the
#      case's host, and at once walks the chain and scans the set in the guest,
#      one request at a time, timing each;
#   4. reads every host's /status and /metrics before and after each step, and
#      deletes the VM.
#
# The cases:
#   cluster  the cluster cache's share at 100 %, the VM restored on another
#            host, which reads its pages from the cluster's disks;
#   store    the share at 0 %, the VM restored on another host, which reads its
#            pages from the object store;
#   memory   the share at 100 %, the VM restored on the host that suspended it,
#            whose memory may still hold its pages.
# Changing the share restarts every host pod. Even rounds run cluster and
# memory, then store; odd rounds store, then memory and cluster; so each round
# has its own order and the share changes once a round.
#
# Everything is written under APP_RESTORE_OUT, one directory per case.
set -euo pipefail

export KUBECONFIG=${KUBECONFIG:-/etc/rancher/k3s/k3s.yaml}
namespace=sproutfs
out=${APP_RESTORE_OUT:-/tmp/sproutfs-app-restore}
rounds=${APP_RESTORE_ROUNDS:-3}
# The data of the reports: a heap of about 4 GiB in an 8 GiB guest.
keys=${APP_RESTORE_KEYS:-24000000}
members=${APP_RESTORE_MEMBERS:-10000000}
value=${APP_RESTORE_VALUE:-100}
steps=${APP_RESTORE_STEPS:-20000}
scan=${APP_RESTORE_SCAN:-4000000}
settle=${APP_RESTORE_SETTLE:-30}
seed_base=${APP_RESTORE_SEED:-1000}
only=${APP_RESTORE_CASES:-cluster,store,memory}
for number in "$rounds" "$keys" "$members" "$value" "$steps" "$scan" "$settle" "$seed_base"; do
    [[ $number =~ ^[0-9]+$ ]] || { echo "APP_RESTORE_* settings are numbers: $number" >&2; exit 2; }
done

ctl() { kubectl exec -n "$namespace" -i deploy/sproutfs-orchestrator -- sproutfsctl "$@" < /dev/null; }
step() { printf '\n=== %s ===\n' "$*"; }
fail() { printf '\nFAIL: %s\n' "$*" >&2; exit 1; }
now() { date +%s.%N; }
elapsed() { python3 -c "import sys; print(f'{float(sys.argv[2]) - float(sys.argv[1]):.3f}')" "$1" "$2"; }
run_in() { ctl exec "$1" --timeout "${3:-120s}" -- "$2"; }

agent_ready() {
    local vm=$1 deadline=$(($(date +%s) + 180)) last=
    while (($(date +%s) < deadline)); do
        if last=$(run_in "$vm" true 2>&1); then return 0; fi
        sleep 1
    done
    printf '%s\n' "$last" >&2
    return 1
}

mkdir -p "$out"
work=$(mktemp -d /tmp/sproutfs-app-restore.XXXXXX)
chmod 0700 "$work"
# The VM a case is measuring, which a run that fails deletes on its way out: a
# VM left behind would be recovered onto a host and hold its memory through
# every later case.
current=''
cleanup() {
    if [[ -n $current ]]; then ctl delete "$current" > /dev/null 2>&1 || true; fi
    rm -rf -- "$work"
}
trap cleanup EXIT
# The token goes into a header file only this user can read, rather than onto
# a command line every process on the node can list.
kubectl get secret -n "$namespace" sproutfs-api-token -o jsonpath='{.data.token}' |
    base64 -d | awk '{ printf "Authorization: Bearer %s\n", $0 }' > "$work/header"

# The host pods, as "pod address" lines in name order. They run on their
# nodes' own network, so this node reaches each one's API at its address.
pods=()
addresses=()
read_pods() {
    pods=()
    addresses=()
    local pod address
    while read -r pod address; do
        pods+=("$pod")
        addresses+=("$address")
    done < <(kubectl get pods -n "$namespace" -l app.kubernetes.io/name=sproutfs-host \
        --field-selector=status.phase=Running \
        -o jsonpath='{range .items[*]}{.metadata.name} {.status.podIP}{"\n"}{end}' | sort)
}
host_api() { curl -fsS --max-time 20 -H @"$work/header" "http://$1:8080$2"; }
address_of() {
    local at
    for at in "${!pods[@]}"; do
        if [[ ${pods[$at]} == "$1" ]]; then
            printf '%s\n' "${addresses[$at]}"
            return 0
        fi
    done
    return 1
}

# wait_hosts returns once every host pod is ready, the orchestrator sees them
# all, and each holds the same membership: every node serving its disk under
# the code.
wait_hosts() {
    local want=$1 deadline=$(($(date +%s) + 900)) ready at listed
    kubectl rollout status -n "$namespace" deployment/sproutfs-host --timeout=900s > /dev/null
    while :; do
        (($(date +%s) < deadline)) || fail "the hosts did not come up: $(ctl hosts 2>&1)"
        read_pods
        ready=$(ctl hosts 2> /dev/null | awk 'NR > 1 && $2 == "true"' | wc -l)
        if ((ready == want && ${#pods[@]} == want)); then
            listed=0
            for at in "${!pods[@]}"; do
                if host_api "${addresses[$at]}" /status 2> /dev/null | python3 -c '
import json, sys
status = json.load(sys.stdin)
held = status["membership"]
mine = (status.get("member") or {}).get("disks") or [{}]
sys.exit(0 if held["disks"] == int(sys.argv[1]) and held["k"] + held["m"] > 1 and mine[0].get("state") == "serving" else 1)' "$want"; then
                    listed=$((listed + 1))
                fi
            done
            ((listed == want)) && return 0
        fi
        sleep 5
    done
}

share_now() {
    kubectl get deployment -n "$namespace" sproutfs-host \
        -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="SPROUTFS_CACHE_CLUSTER_PERCENT")].value}'
}

# set_share restarts the host pods at another share of the cluster cache.
set_share() {
    local share=$1
    if [[ $(share_now) != "$share" ]]; then
        step "the cluster cache's share to $share %"
        kubectl set env -n "$namespace" deployment/sproutfs-host SPROUTFS_CACHE_CLUSTER_PERCENT="$share" > /dev/null
    fi
    wait_hosts "$host_count"
}

# snapshot keeps every host's status, metrics and its container's CPU under one
# label.
snapshot() {
    local dir=$1 label=$2 at
    for at in "${!pods[@]}"; do
        host_api "${addresses[$at]}" /status > "$dir/$label.${pods[$at]}.status.json"
        host_api "${addresses[$at]}" /metrics > "$dir/$label.${pods[$at]}.prom"
        kubectl exec -n "$namespace" "${pods[$at]}" -- cat /sys/fs/cgroup/cpu.stat \
            > "$dir/$label.${pods[$at]}.cpu" < /dev/null
    done
}

# drop_caches empties the kernel's page cache on every node, so a stripe a
# holder serves is read from its SSD and not from what the fill left in memory.
drop_caches() {
    local pod
    for pod in "${pods[@]}"; do
        kubectl exec -n "$namespace" "$pod" -- sh -c 'sync && echo 1 > /proc/sys/vm/drop_caches' < /dev/null
    done
}

# wait_fills returns once no host has a fill or a keep queued and no stripe
# was kept or dropped anywhere for five seconds.
wait_fills() {
    local deadline=$(($(date +%s) + 600)) last='' seen at
    while (($(date +%s) < deadline)); do
        seen=''
        for at in "${!pods[@]}"; do
            seen+=$(host_api "${addresses[$at]}" /status | python3 -c '
import json, sys
fill = json.load(sys.stdin).get("cache_fill") or {}
print(fill.get("queued_bytes", 0), fill.get("kept", 0), fill.get("sent", 0), sum((fill.get("dropped") or {}).values()), end=";")')
        done
        if [[ $seen == "$last" && ! $seen =~ (^|;)[1-9] ]]; then
            return 0
        fi
        last=$seen
        sleep 5
    done
    fail "the fills did not settle: $seen"
}

host_count=$(kubectl get deployment -n "$namespace" sproutfs-host -o jsonpath='{.spec.replicas}')
wait_hosts "$host_count"
step "$host_count hosts"
ctl hosts
# Every case has the cluster to itself.
[[ $(ctl list | awk 'NR > 1' | wc -l) == 0 ]] || fail "the deployment already has VMs: $(ctl list)"

run_case() {
    local round=$1 name=$2 index=$3
    local dir=$out/r$round-$name seed=$((seed_base + round * 10 + index))
    local shape="--keys $keys --members $members --value $value --seed $seed"
    local created vm source destination at began ended
    rm -rf -- "$dir"
    mkdir -p "$dir"
    step "round $round, $name: seed $seed"

    created=$(ctl create --template valkey)
    printf '%s\n' "$created" | tee "$dir/create.txt"
    vm=$(awk '{ print $1; exit }' <<< "$created")
    source=$(awk '{ for (i = 1; i < NF; i++) if ($i == "on") { print $(i + 1); exit } }' <<< "$created")
    [[ $vm == vm-* && -n $source ]] || fail "create named no VM and host: $created"
    current=$vm
    agent_ready "$vm" || fail "the agent in $vm never answered"

    run_in "$vm" "setsid valkey-server --daemonize yes --save '' --appendonly no --port 0 \
        --unixsocket /tmp/valkey.sock --unixsocketperm 700 --dir /tmp --logfile /tmp/valkey.log &&
        until valkey-cli -s /tmp/valkey.sock ping > /dev/null 2>&1; do sleep 0.1; done &&
        cat /etc/sproutfs-valkey-version && valkey-server --version" > "$dir/valkey.txt"
    # The load is one exec, within the agent's ten minutes. A load started in
    # the background would race the agent, which ends the command's process
    # group as soon as its shell exits.
    began=$(now)
    run_in "$vm" "sproutfs-guest-chase load $shape" 600s > "$dir/load.json" ||
        fail "the load in $vm failed"
    printf 'loaded in %ss: %s\n' "$(elapsed "$began" "$(now)")" "$(cat "$dir/load.json")"
    sleep "$settle"

    read_pods
    snapshot "$dir" loaded
    began=$(now)
    ctl stop "$vm" --suspend | tee "$dir/suspend.txt"
    ended=$(now)
    wait_fills
    snapshot "$dir" suspended
    local suspend_seconds fills_seconds
    suspend_seconds=$(elapsed "$began" "$ended")
    fills_seconds=$(elapsed "$ended" "$(now)")

    if [[ $name == memory ]]; then
        destination=$source
    else
        # Another host, a different one each round.
        for at in "${!pods[@]}"; do
            [[ ${pods[$at]} == "$source" ]] && break
        done
        destination=${pods[$(((at + 1 + round % (host_count - 1)) % host_count))]}
    fi
    drop_caches
    snapshot "$dir" before
    began=$(now)
    ctl start "$vm" --to "$destination" | tee "$dir/start.txt"
    local started
    started=$(now)
    local status=0
    ctl exec "$vm" --timeout 600s -- \
        "sproutfs-guest-chase walk $shape --steps $steps --scan $scan --budget 240s" \
        > "$dir/walk.json" 2> "$dir/walk.err" || status=$?
    ended=$(now)
    snapshot "$dir" after
    python3 - "$dir/case.json" <<PY
import json, sys
json.dump({"round": $round, "case": "$name", "seed": $seed, "vm": "$vm", "source": "$source",
           "destination": "$destination", "share": $(share_now), "keys": $keys, "members": $members,
           "value_bytes": $value, "steps": $steps, "scan": $scan,
           "suspend_seconds": $suspend_seconds, "fills_seconds": $fills_seconds,
           "start_seconds": $(elapsed "$began" "$started"), "walk_exec_seconds": $(elapsed "$started" "$ended"),
           "walk_exit": $status}, open(sys.argv[1], "w"), indent=1)
PY
    ((status == 0)) || fail "the walk in $vm failed: $(cat "$dir/walk.err")"
    python3 - "$dir/walk.json" <<'PY'
import json, sys
walk = json.load(open(sys.argv[1]))
for name in ("chase", "scan"):
    micros = sorted(walk[name]["micros"])
    if micros:
        print(f"{name}: {walk[name]['requests']} requests in {walk[name]['seconds']:.2f}s, "
              f"p50 {micros[len(micros) // 2]} us, p99 {micros[int(len(micros) * 0.99)]} us, max {micros[-1]} us")
PY
    ctl delete "$vm" > /dev/null
    current=""
}

case_index() {
    case $1 in cluster) echo 0 ;; store) echo 1 ;; memory) echo 2 ;; esac
}
wanted() { [[ ",$only," == *",$1,"* ]]; }

for ((round = 0; round < rounds; round++)); do
    if ((round % 2 == 0)); then
        order=("100 cluster" "100 memory" "0 store")
    else
        order=("0 store" "100 memory" "100 cluster")
    fi
    for entry in "${order[@]}"; do
        read -r share name <<< "$entry"
        wanted "$name" || continue
        set_share "$share"
        run_case "$round" "$name" "$(case_index "$name")"
    done
done
step 'done'
