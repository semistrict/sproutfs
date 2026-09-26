#!/usr/bin/env bash
# The isolated arena's worst cases beside the shared arena's, on the demo node,
# in one run of at most half an hour. This is TASK-2.6's second run.
# demo-arena.sh measured a fan-out whose pages were already published and
# shared. This one measures what the isolated arena copies.
#
# scripts/demo-gce.sh copies this one file to the node and runs it there:
#
#     bash demo-arena-worst.sh
#
# It measures each arena mode at a 2 MiB RAM page, and then at 4 KiB while the
# time allows. For each mode and page it sets the host deployment to them, with
# the interval checkpoint, the loss window and the flush bound off, so that only
# the run's own captures and forks publish anything. It restarts the pods, so
# that both pagers start empty. When it ends, however it ends, it puts back every
# setting it changed and restarts the pods again.
#
# The cases, each repeated as WORST_REPEATS says:
#
#   fork     A 512 MiB parent writes nearly all of its RAM and, with no
#            checkpoint since, forks three children on its own host. Each child
#            reads all of it.
#   inherit  A 1 GiB parent writes nearly all of its RAM, is captured, and forks
#            two children on its own host. Each child reads all of it, and then
#            the parent reads it again and is captured, which counts the pages
#            it copied without storing into them.
#   capture  A 3.5 GiB guest, alone on its host, writes nearly all of its RAM
#            and is captured. A host admits guests whose RAM adds up to its RAM
#            arena, 3840 MiB, so this is the largest guest the demo runs.
#   restore  A 2 GiB guest writes 1 GiB and is stopped with its memory. It is
#            started on the other host, stopped again, and started back on its
#            own host, where it reads the 1 GiB. This is the shape of the first
#            run's restores.
#
# Every guest checks what it reads against what its writer read. Before and
# after each step it saves the host's status: the pager's counters and sharing
# gauges, the host pod's cgroup memory and the host process's CPU time. It also
# counts, for the whole node, the faults KVM finished from its own worker (the
# kvm_try_async_get_page tracepoint), which asks for every page writable: the
# pager sees each as a store trap whatever the guest's access was.
#
# The run stops at WORST_BUDGET seconds, leaving time to put the deployment
# back: a case that would not finish in time is skipped and says so. Each step
# prints the time since the run began. Everything is recorded under
# /tmp/sproutfs-arena-worst, which scripts/demo-gce.sh copies back, and the
# summary gives each measure's median, minimum and maximum per mode.
#
# WORST_ARENAS names the arena modes to measure, in order. A mode may carry a
# word for the guest kernel's command line after a plus, such as
# isolated+no-kvmapf, which boots every guest of that mode with it.
#
# Overridable: WORST_BUDGET (1800), WORST_PAGES ("2097152 4096"),
# WORST_ARENAS ("shared isolated"),
# WORST_REPEATS ("fork=3 inherit=3 capture=1 restore=3"),
# WORST_REPEATS_4K ("fork=1 inherit=1 capture=1"), SPROUTFS_DEMO_NAMESPACE.
set -euo pipefail

export KUBECONFIG=${KUBECONFIG:-/etc/rancher/k3s/k3s.yaml}
namespace=${SPROUTFS_DEMO_NAMESPACE:-sproutfs}
budget=${WORST_BUDGET:-1800}
pages=${WORST_PAGES:-2097152 4096}
arenas=${WORST_ARENAS:-shared isolated}
# The host's own cold-boot command line (cmd/sproutfs-host/config.go), which a
# mode's word is added to where the deployment sets none.
default_boot_args='console=ttyS0 reboot=k panic=1 i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd init=/init rootfstype=ext4 rootflags=dax=always'
repeats_2m=${WORST_REPEATS:-fork=3 inherit=3 capture=1 restore=3}
repeats_4k=${WORST_REPEATS_4K:-fork=1 inherit=1 capture=1}
out=/tmp/sproutfs-arena-worst
long=600s
agent_timeout=180
began_run=$(date +%s)

fork_mib=512
fork_children=3
inherit_mib=1024
inherit_children=2
capture_mib=3584
restore_mib=2048
restore_fill_mib=1024
# What a guest leaves free when it fills its RAM, so that its agent can still
# start a command.
margin_mib=64
# What each step is expected to take, in seconds, which is what the budget
# checks before starting it: a restart of the pods, one repetition of each case
# at each page, and putting the deployment back at the end.
roll_seconds=150
declare -A cost=([fork-2097152]=60 [inherit-2097152]=50 [capture-2097152]=70 [restore-2097152]=60
    [fork-4096]=150 [inherit-4096]=90 [capture-4096]=90)

ctl() { kubectl exec -n "$namespace" -i deploy/sproutfs-orchestrator -- sproutfsctl "$@" < /dev/null; }
now() { date +%s.%N; }
since() { awk -v a="$1" -v b="$(now)" 'BEGIN { printf "%.3f", b - a }'; }
elapsed() { echo $(($(date +%s) - began_run)); }
step() { printf '\n=== [%ds] %s ===\n' "$(elapsed)" "$*"; }
fail() {
    printf '\nFAIL: %s\n' "$*" >&2
    exit 1
}
# fits reports whether this many seconds of work, and putting the deployment
# back after it, end within the budget.
fits() { (($(elapsed) + $1 + roll_seconds <= budget)); }
run_in() { ctl exec "$1" --timeout "$long" -- "${@:2}" < /dev/null; }
agent_ready() {
    local vm=$1 deadline=$(($(date +%s) + agent_timeout)) last=
    while (($(date +%s) < deadline)); do
        if last=$(ctl exec "$vm" --timeout 10s -- true < /dev/null 2>&1); then return 0; fi
    done
    printf '%s\n' "$last" >&2
    return 1
}
host_of() { ctl list | awk -v vm="$1" '$1 == vm && $2 != "-" { print $2 }'; }
other_host() { ctl hosts | awk -v host="$1" 'NR > 1 && $1 != host && $2 == "true" { print $1; exit }'; }
vm_of() { awk '{ print $1; exit }' <<< "$1"; }
# field prints the number after a word of a line such as "pause 0.005s,".
field() { awk -v name="$1" '{ for (i = 1; i < NF; i++) if ($i == name) { v = $(i + 1); gsub(/[s,]/, "", v); print v; exit } }'; }

# fill writes nearly all of a guest's free memory with random bytes, into a
# file of a tmpfs as large as its RAM, and prints how many MiB it wrote. The
# old file goes first, so a second fill writes as much as the first. Its
# length can be given instead.
fill() {
    local vm=$1 file=$2 mib=${3:-}
    local count="\$((\$(awk '/MemAvailable/ { print int(\$2 / 1024) }' /proc/meminfo) - $margin_mib))"
    [[ -n $mib ]] && count=$mib
    run_in "$vm" "mkdir -p /fill && { grep -qs ' /fill ' /proc/mounts || mount -t tmpfs -o size=100% tmpfs /fill; } &&
        rm -f /fill/$file && n=$count && dd if=/dev/urandom of=/fill/$file bs=1M count=\$n 2>/dev/null && echo \$n"
}
# digest reads a whole file in a guest. busybox's cat splices into /dev/null,
# which touches no page, so md5sum is what reads it.
digest() { run_in "$1" "md5sum /fill/$2" | awk '{ print $1 }'; }

# --- the host's own API, its cgroup and its process -----------------------------
work=$(mktemp -d /tmp/sproutfs-arena-worst-work.XXXXXX)
chmod 0700 "$work"
forwarding=''
configured=false
cleanup() {
    if [[ -n $forwarding ]]; then kill "$forwarding" 2> /dev/null || true; fi
    if [[ -n $async_trigger ]]; then
        echo "!$async_hist" | sudo tee "$async_trigger" > /dev/null || true
    fi
    if "$configured"; then
        configured=false
        step 'putting the deployment back'
        restore || printf 'the deployment could not be put back; a redeploy will\n' >&2
        step 'done'
    fi
    rm -rf -- "$work"
}
trap cleanup EXIT

# KVM's asynchronous faults, counted by a histogram trigger on the tracepoint
# for as long as the run lasts. A kernel without the tracepoint or histogram
# triggers records -1.
async_trigger=''
async_hist='hist:keys=common_pid'
tracepoint=/sys/kernel/tracing/events/kvm/kvm_try_async_get_page/trigger
if sudo test -e "$tracepoint" && echo "$async_hist" | sudo tee "$tracepoint" > /dev/null; then
    async_trigger=$tracepoint
fi
[[ -n $async_trigger ]] || printf 'KVM asynchronous faults are not counted on this kernel\n' >&2
# async_faults prints how many faults KVM has finished asynchronously on this
# node since the run began, -1 where they are not counted.
async_faults() {
    if [[ -z $async_trigger ]]; then
        echo -1
        return
    fi
    sudo cat "${async_trigger%/trigger}/hist" | awk '$1 == "Hits:" { print $2; found = 1; exit } END { if (!found) print 0 }'
}

# status_of saves one host's /status, through a port-forward from the node.
status_of() {
    local pod=$1 into=$2 deadline=$(($(date +%s) + 30))
    kubectl port-forward -n "$namespace" "pod/$pod" 18081:8080 > "$work/forward.log" 2>&1 &
    forwarding=$!
    until curl -fsS -H @"$work/header" -o "$into" http://127.0.0.1:18081/status 2> /dev/null; do
        (($(date +%s) < deadline)) || {
            cat "$work/forward.log" >&2
            fail "no status from $pod"
        }
        sleep 0.5
    done
    kill "$forwarding" 2> /dev/null || true
    wait "$forwarding" 2> /dev/null || true
    forwarding=''
}
# cgroup_of is the pod's own cgroup on the node, whichever driver k3s runs.
cgroup_of() {
    local uid dir
    uid=$(kubectl get pod -n "$namespace" "$1" -o jsonpath='{.metadata.uid}')
    for dir in /sys/fs/cgroup/kubepods*/*/pod"$uid" /sys/fs/cgroup/kubepods*/pod"$uid" \
        /sys/fs/cgroup/kubepods.slice/*/*pod"${uid//-/_}".slice /sys/fs/cgroup/kubepods.slice/*pod"${uid//-/_}".slice; do
        if [[ -d $dir ]]; then
            printf '%s\n' "$dir"
            return
        fi
    done
    fail "no cgroup on the node for $1"
}
# snap saves what one host holds at this moment as $dir/snap-NAME.json: its RAM
# and PMEM pagers' counters and gauges, its object store counters, its pod's
# memory, the node's, and its process's CPU time.
snap() {
    local pod=$1 name=$2 cgroup ticks_used
    status_of "$pod" "$work/status.json"
    cgroup=$(cgroup_of "$pod")
    ticks_used=$(kubectl exec -n "$namespace" "$pod" -- cat /proc/1/stat | awk '{ print $14 + $15 }')
    python3 - "$work/status.json" "$cgroup" "$ticks_used" "$(getconf CLK_TCK)" "$pod" "$(async_faults)" \
        > "$dir/snap-$name.json" <<'PY'
import json, pathlib, sys
status, cgroup, ticks, hz, pod = sys.argv[1], pathlib.Path(sys.argv[2]), int(sys.argv[3]), int(sys.argv[4]), sys.argv[5]
whole = json.load(open(status))
pager = whole["pager"]
snap = {"pod": pod, "cpu_s": ticks / hz, "kvm_async_faults": int(sys.argv[6])}
for op in ("get", "put"):
    count = whole.get("store", {}).get(op, {})
    snap["store_" + op + "_calls"] = count.get("calls", 0)
    snap["store_" + op + "_mib"] = count.get("bytes", 0) / 2**20
for kind in ("ram", "pmem"):
    p = pager[kind]
    snap[kind + "_page"] = p["page_bytes"]
    snap[kind + "_resident_mib"] = p["resident_pages"] * p["page_bytes"] / 2**20
    for name in ("unique_bytes", "mapped_bytes", "saved_bytes"):
        snap[kind + "_" + name.replace("_bytes", "_mib")] = p["sharing"][name] / 2**20
    for name in ("faults", "evictions", "spills", "shared_pages", "idle_pages", "loaded_pages",
                 "copy_on_writes", "unmapped_copy_on_writes", "unchanged_pages", "read_traps", "store_traps",
                 "protect_traps", "revocations", "revoked_pages", "moved_pages", "fork_copies", "tampered"):
        snap[kind + "_" + name] = p.get(name, 0)
stat = dict(line.split() for line in (cgroup / "memory.stat").read_text().splitlines())
snap["pod_memory_mib"] = int((cgroup / "memory.current").read_text()) / 2**20
for name in ("anon", "shmem", "file"):
    snap["pod_" + name + "_mib"] = int(stat[name]) / 2**20
huge = cgroup / "hugetlb.2MB.current"
snap["pod_hugetlb_mib"] = int(huge.read_text()) / 2**20 if huge.exists() else 0
meminfo = dict(line.split(":", 1) for line in open("/proc/meminfo"))
snap["node_available_mib"] = int(meminfo["MemAvailable"].split()[0]) / 1024
print(json.dumps(snap))
PY
}
# record appends one repetition's timings to $dir/runs.jsonl, from name=value
# arguments.
record() {
    python3 -c 'import json, sys
print(json.dumps({k: (v if not v.replace(".", "", 1).isdigit() else float(v))
                  for k, v in (a.split("=", 1) for a in sys.argv[1:])}))' "$@" >> "$dir/runs.jsonl"
    tail -1 "$dir/runs.jsonl"
}
# children_first_output waits for each child's first answered command, all at
# once, and appends each one's time since began to the file named.
children_first_output() {
    local began=$1 into=$2 child
    shift 2
    for child in "$@"; do
        (agent_ready "$child" && printf '%s %s\n' "$child" "$(since "$began")" >> "$into") &
    done
    wait
    if [[ ! -f $into ]] || (($(wc -l < "$into") != $#)); then fail 'not every child answered'; fi
}
# children_read has every child read the file at once, checks each one's digest
# against want, and appends each one's time to the file named.
children_read() {
    local file=$1 want=$2 into=$3 child began
    shift 3
    began=$(now)
    for child in "$@"; do
        (
            got=$(digest "$child" "$file")
            [[ $got == "$want" ]] || fail "$child read $got, not $want"
            printf '%s %s\n' "$child" "$(since "$began")" >> "$into"
        ) &
    done
    wait
    if [[ ! -f $into ]] || (($(wc -l < "$into") != $#)); then fail 'not every child read its memory back'; fi
}
slowest() { sort -k2 -n "$1" | awk 'END { print $2 }'; }

# --- the deployment -------------------------------------------------------------------
# The host settings this run changes, beside the ConfigMap's arena and prefix,
# which both deployments read: the ConfigMap is changed rather than the
# deployments' references to it, so a later apply of deploy/ finds them as it
# left them. saved holds what each setting had before: NAME=value, or NAME-
# where it was not set.
changed=(SPROUTFS_RAM_PAGE_BYTES SPROUTFS_RAM_DIRTY_PAGES GOMEMLIMIT SPROUTFS_TEMPLATES
    SPROUTFS_CHECKPOINT_INTERVAL SPROUTFS_LOSS_WINDOW SPROUTFS_FLUSH_BOUND SPROUTFS_BOOT_ARGS)
saved=()
saved_arena='' saved_prefix='' saved_boot_args=''

# roll restarts the hosts, so that both pagers start empty, and then the
# orchestrator, which reads the objects under the hosts' prefix. It returns
# once the orchestrator sees two ready hosts.
roll() {
    kubectl rollout restart -n "$namespace" deployment/sproutfs-host > /dev/null
    kubectl rollout status -n "$namespace" deployment/sproutfs-host --timeout=900s > /dev/null
    kubectl rollout restart -n "$namespace" deployment/sproutfs-orchestrator > /dev/null
    kubectl rollout status -n "$namespace" deployment/sproutfs-orchestrator --timeout=300s > /dev/null
    local deadline=$(($(date +%s) + 300))
    until (($(ctl hosts 2> /dev/null | awk 'NR > 1 && $2 == "true"' | wc -l) == 2)); do
        (($(date +%s) < deadline)) || fail 'the orchestrator does not see two ready hosts'
        sleep 2
    done
}
# set_config writes the ConfigMap's arena and prefix.
set_config() {
    kubectl patch configmap -n "$namespace" sproutfs-demo --type merge \
        -p "{\"data\":{\"arena\":\"$1\",\"prefix\":\"$2\"}}" > /dev/null
}
# save records what configure is about to change, once.
save() {
    local env name value
    env=$(kubectl get deployment -n "$namespace" sproutfs-host \
        -o jsonpath='{range .spec.template.spec.containers[0].env[*]}{.name}{"="}{.value}{"\n"}{end}')
    for name in "${changed[@]}"; do
        if value=$(awk -v name="$name" '{ split($0, kv, "=") } kv[1] == name { sub(/^[^=]*=/, ""); print; found = 1 }
            END { exit !found }' <<< "$env"); then
            saved+=("$name=$value")
            [[ $name != SPROUTFS_BOOT_ARGS ]] || saved_boot_args=$value
        else
            saved+=("$name-")
        fi
    done
    saved_arena=$(kubectl get configmap -n "$namespace" sproutfs-demo -o jsonpath='{.data.arena}')
    saved_prefix=$(kubectl get configmap -n "$namespace" sproutfs-demo -o jsonpath='{.data.prefix}')
    configured=true
}
# configure sets the hosts to an arena and a RAM page, and a word added to the
# guests' command line or none, and restarts them onto them. The dirty budget is counted in the RAM pager's own page: 9 GiB, as
# deploy/ gives it at 2 MiB. At 4 KiB the RAM arena is ordinary memory in the
# pod's 8 GiB, so Go's own ceiling comes down to leave room for it. A template
# is published in the page of the host that imported it, and a VM's RAM keeps
# that page, so a 4 KiB run keeps its objects under a prefix of its own, where
# the hosts import the template again at 4 KiB. Only the template the run uses
# is imported.
configure() {
    local arena=$1 page=$2 word=$3 dirty limit prefix
    case $page in
        2097152) dirty=4608 limit=6GiB prefix=$saved_prefix ;;
        4096) dirty=2359296 limit=3GiB prefix=$saved_prefix-ram4k ;;
        *) fail "a RAM page of $page bytes: want 2097152 or 4096" ;;
    esac
    set_config "$arena" "$prefix"
    kubectl set env -n "$namespace" deployment/sproutfs-host \
        SPROUTFS_RAM_PAGE_BYTES="$page" SPROUTFS_RAM_DIRTY_PAGES="$dirty" GOMEMLIMIT="$limit" \
        SPROUTFS_TEMPLATES=alpine=/usr/share/sproutfs/guest/guest.ext4 \
        SPROUTFS_CHECKPOINT_INTERVAL=-1s SPROUTFS_LOSS_WINDOW=0 SPROUTFS_FLUSH_BOUND=0 \
        SPROUTFS_BOOT_ARGS="${saved_boot_args:-$default_boot_args}${word:+ $word}" > /dev/null
    roll
}
# restore puts back what configure changed.
restore() {
    set_config "${saved_arena:-shared}" "$saved_prefix"
    kubectl set env -n "$namespace" deployment/sproutfs-host "${saved[@]}" > /dev/null
    roll
}

# --- fork: children of pages no checkpoint holds ---------------------------------------------
fork_case() {
    local i=$1 created parent host mib want began table children
    created=$(ctl create --template alpine --memory "${fork_mib}M")
    parent=$(vm_of "$created")
    agent_ready "$parent" || fail "the agent in $parent never answered"
    host=$(host_of "$parent")
    mib=$(fill "$parent" f)
    want=$(digest "$parent" f)
    snap "$host" "$i-before"
    began=$(now)
    table=$(ctl fork "$parent" --count "$fork_children")
    printf '%s\n' "$table" > "$dir/fork-$i.txt"
    children=$(awk 'NR > 1 { print $1 }' <<< "$table")
    # shellcheck disable=SC2086  # one word per child
    children_first_output "$began" "$dir/first-$i.txt" $children
    snap "$host" "$i-forked"
    # shellcheck disable=SC2086
    children_read f "$want" "$dir/read-$i.txt" $children
    snap "$host" "$i-after"
    record i="$i" host="$host" fill_mib="$mib" pause="$(awk 'NR == 2 { print $3 }' <<< "$table")" \
        fork_total="$(awk 'NR == 2 { print $5 }' <<< "$table")" \
        first_output="$(slowest "$dir/first-$i.txt")" read="$(slowest "$dir/read-$i.txt")"
    for vm in $children "$parent"; do ctl delete "$vm" > /dev/null; done
}

# --- inherit: children of pages a checkpoint published ----------------------------------
inherit_case() {
    local i=$1 created parent host mib want captured began table children reread
    created=$(ctl create --template alpine --memory "${inherit_mib}M")
    parent=$(vm_of "$created")
    agent_ready "$parent" || fail "the agent in $parent never answered"
    host=$(host_of "$parent")
    # What the guest kernel booted with, which a mode's word changes.
    run_in "$parent" 'cat /proc/cmdline' > "$dir/cmdline-$i.txt"
    mib=$(fill "$parent" f)
    want=$(digest "$parent" f)
    captured=$(ctl capture "$parent")
    snap "$host" "$i-before"
    began=$(now)
    table=$(ctl fork "$parent" --count "$inherit_children")
    printf '%s\n' "$table" > "$dir/fork-$i.txt"
    children=$(awk 'NR > 1 { print $1 }' <<< "$table")
    # shellcheck disable=SC2086
    children_first_output "$began" "$dir/first-$i.txt" $children
    # shellcheck disable=SC2086
    children_read f "$want" "$dir/read-$i.txt" $children
    snap "$host" "$i-read"
    # The owner reads its own memory again. Where its pages moved, its mappings
    # were taken away and each read faults.
    began=$(now)
    [[ $(digest "$parent" f) == "$want" ]] || fail "$parent no longer reads what it wrote"
    reread=$(since "$began")
    snap "$host" "$i-after"
    ctl capture "$parent" > /dev/null
    snap "$host" "$i-settled"
    record i="$i" host="$host" fill_mib="$mib" \
        capture_publish="$(field publish <<< "$captured")" \
        pause="$(awk 'NR == 2 { print $3 }' <<< "$table")" fork_total="$(awk 'NR == 2 { print $5 }' <<< "$table")" \
        first_output="$(slowest "$dir/first-$i.txt")" read="$(slowest "$dir/read-$i.txt")" owner_reread="$reread"
    for vm in $children "$parent"; do ctl delete "$vm" > /dev/null; done
}

# --- capture: a checkpoint of a guest that wrote all of its RAM ---------------------------
capture_case() {
    local i=$1 guest host mib captured since_logs
    guest=$(vm_of "$(ctl create --template alpine --memory "${capture_mib}M")")
    agent_ready "$guest" || fail "the agent in $guest never answered"
    host=$(host_of "$guest")
    mib=$(fill "$guest" f)
    snap "$host" "$i-before"
    since_logs=$(date -u +%FT%TZ)
    captured=$(ctl capture "$guest")
    snap "$host" "$i-after"
    kubectl logs -n "$namespace" "$host" --since-time="$since_logs" --tail=-1 |
        grep '"host: checkpoint"' | grep "\"$guest\"" > "$dir/log-$i.jsonl" || true
    record i="$i" host="$host" fill_mib="$mib" \
        pause="$(field pause <<< "$captured")" publish="$(field publish <<< "$captured")"
    ctl delete "$guest" > /dev/null
}

# --- restore: a stop with the memory and a start, away and back -------------------------
restore_guest=''
restore_case() {
    local i=$1 home away want began started away_start back_start back_output back_read
    if [[ -z $restore_guest ]]; then
        restore_guest=$(vm_of "$(ctl create --template alpine --memory "${restore_mib}M")")
        agent_ready "$restore_guest" || fail "the agent in $restore_guest never answered"
    fi
    home=$(host_of "$restore_guest")
    away=$(other_host "$home")
    fill "$restore_guest" f "$restore_fill_mib" > /dev/null
    want=$(digest "$restore_guest" f)
    ctl stop "$restore_guest" --suspend > /dev/null
    started=$(ctl start "$restore_guest" --to "$away")
    away_start=$(sed -n 's/.* in \([0-9.]*\)s$/\1/p' <<< "$started")
    agent_ready "$restore_guest" || fail "the agent in $restore_guest never answered on $away"
    ctl stop "$restore_guest" --suspend > /dev/null
    snap "$home" "$i-back-before"
    began=$(now)
    started=$(ctl start "$restore_guest" --to "$home")
    back_start=$(sed -n 's/.* in \([0-9.]*\)s$/\1/p' <<< "$started")
    snap "$home" "$i-back-started"
    agent_ready "$restore_guest" || fail "the agent in $restore_guest never answered back on $home"
    back_output=$(since "$began")
    began=$(now)
    [[ $(digest "$restore_guest" f) == "$want" ]] || fail "$restore_guest read other bytes back on $home"
    back_read=$(since "$began")
    snap "$home" "$i-back-after"
    record i="$i" away_start="$away_start" back_start="$back_start" back_output="$back_output" back_read="$back_read"
}

rm -rf -- "$out"
mkdir -p "$out"
kubectl get secret -n "$namespace" sproutfs-api-token -o jsonpath='{.data.token}' |
    base64 -d | awk '{ printf "Authorization: Bearer %s\n", $0 }' > "$work/header"
if [[ -n $(ctl list | awk 'NR > 1') ]]; then
    ctl list >&2
    fail 'the deployment runs VMs already; measure on hosts that run none'
fi
save
for page in $pages; do
    plan=$repeats_2m
    [[ $page == 4096 ]] && plan=$repeats_4k
    for mode in $arenas; do
        arena=${mode%%+*} word=''
        [[ $mode == "$arena" ]] || word=${mode#*+}
        # A mode is started only where at least its first case fits after it.
        first=${plan%% *}
        if ! fits $((roll_seconds + ${cost[${first%%=*}-$page]})); then
            step "no time left for the $mode arena at a $page-byte page"
            continue
        fi
        step "the $mode arena at a $page-byte page"
        configure "$arena" "$page" "$word"
        for entry in $plan; do
            name=${entry%%=*} count=${entry#*=}
            dir=$out/$mode-$page/$name
            mkdir -p "$dir"
            for i in $(seq 1 "$count"); do
                if ! fits "${cost[$name-$page]}"; then
                    step "no time left for $name $i of $count"
                    break
                fi
                step "$mode, $page, $name $i of $count"
                "${name}_case" "$i"
            done
        done
        if [[ -n $restore_guest ]]; then
            ctl delete "$restore_guest" > /dev/null
            restore_guest=''
        fi
    done
done

# --- the summary ------------------------------------------------------------------------
step 'the summary'
python3 - "$out" <<'PY' | tee "$out/summary.txt"
import json, pathlib, statistics, sys
out = pathlib.Path(sys.argv[1])
measures = {}
def add(config, case, key, value):
    measures.setdefault((case, key), {}).setdefault(config, []).append(float(value))
def snaps(d, i):
    return {p.stem.split("-", 2)[2]: json.loads(p.read_text()) for p in d.glob(f"snap-{i}-*.json")}
def delta(a, b, key):
    return b[key] - a[key]
counters = ("ram_moved_pages", "ram_fork_copies", "ram_revoked_pages", "ram_faults", "ram_loaded_pages",
            "ram_copy_on_writes", "ram_unmapped_copy_on_writes", "ram_read_traps", "ram_store_traps",
            "ram_protect_traps", "kvm_async_faults", "ram_evictions", "store_get_mib", "store_put_mib", "cpu_s")
configs = sorted(p.name for p in out.iterdir() if p.is_dir())
for config in configs:
    for d in sorted((out / config).iterdir()):
        if not (d / "runs.jsonl").exists():
            continue
        case = d.name
        for line in open(d / "runs.jsonl"):
            r = json.loads(line)
            s = snaps(d, int(r["i"]))
            for key, value in r.items():
                if key not in ("i", "host") and isinstance(value, float):
                    add(config, case, key, value)
            if case == "fork":
                for key in counters:
                    add(config, case, key + " fork", delta(s["before"], s["forked"], key))
                    add(config, case, key + " read", delta(s["forked"], s["after"], key))
                for key in ("ram_resident_mib", "ram_saved_mib", "pod_memory_mib", "pod_hugetlb_mib"):
                    add(config, case, key + " before", s["before"][key])
                    add(config, case, key + " forked", s["forked"][key])
                    add(config, case, key + " after", s["after"][key])
            if case == "inherit":
                for key in counters:
                    add(config, case, key + " fork+read", delta(s["before"], s["read"], key))
                    add(config, case, key + " owner", delta(s["read"], s["after"], key))
                add(config, case, "ram_unchanged_pages owner capture", delta(s["after"], s["settled"], "ram_unchanged_pages"))
                for key in ("ram_resident_mib", "ram_saved_mib", "pod_memory_mib", "pod_hugetlb_mib"):
                    add(config, case, key + " before", s["before"][key])
                    add(config, case, key + " read", s["read"][key])
                    add(config, case, key + " after", s["after"][key])
                    add(config, case, key + " settled", s["settled"][key])
            if case == "capture":
                add(config, case, "cpu_s", delta(s["before"], s["after"], "cpu_s"))
                for line in open(d / f"log-{int(r['i'])}.jsonl"):
                    log = json.loads(line)
                    add(config, case, "logged dirty_mib", log["dirty_bytes"] / 2**20)
                    add(config, case, "logged upload_s", log["upload_seconds"])
            if case == "restore":
                for key in counters + ("ram_shared_pages",):
                    add(config, case, key + " back start", delta(s["back-before"], s["back-started"], key))
                    add(config, case, key + " back read", delta(s["back-started"], s["back-after"], key))
                add(config, case, "ram_idle_pages back before", s["back-before"]["ram_idle_pages"])
def cell(values):
    if not values:
        return "-"
    return f"{statistics.median(values):.3f} ({min(values):.3f}-{max(values):.3f}) n={len(values)}"
print("case\tmeasure\t" + "\t".join(configs))
for (case, key), by in measures.items():
    print(f"{case}\t{key}\t" + "\t".join(cell(by.get(c, [])) for c in configs))
PY
