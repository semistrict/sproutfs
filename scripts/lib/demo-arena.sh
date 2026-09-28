#!/usr/bin/env bash
# What the hosts' arena mode costs, on the demo node: TASK-2.6's comparison of
# the shared and the isolated arena, one mode per run.
#
# scripts/demo-gce.sh copies this one file to the node and runs it there:
#
#     bash demo-arena.sh
#
# It measures whichever mode the host pods run, which it reads from their
# environment and prints; `SPROUTFS_DEMO_ARENA=isolated scripts/demo-gce.sh
# redeploy` is what changes it. Run it on hosts that have just restarted, which
# a redeploy leaves, so the pagers' counters are this run's alone. It measures:
#
#   1. A fan-out. A 512 MiB guest holds 256 MiB a checkpoint published and
#      64 MiB none did, and forks five children on its own host. The fork's
#      pause and total, each child's first output (fork to its first answered
#      exec), a read of those 320 MiB in every child, and then the host's
#      unique, mapped and saved bytes and the VMMs' mappings.
#   2. Checkpoints. A 2 GiB guest alone on the other host writes 1 GiB of
#      random bytes and is captured, three times: the pause, the upload, and
#      the CPU the host process spent between the capture's start and its end.
#   3. Restores. That guest is stopped with its memory and started again, on
#      the other host and then back on its own: the start, the start to its
#      first answered exec, and a read of the 1 GiB; then its VMM's mappings.
#
# Everything is recorded under /tmp/sproutfs-arena, which scripts/demo-gce.sh
# copies back, and the summary is printed at the end.
#
# Overridable: SPROUTFS_DEMO_NAMESPACE.
set -euo pipefail

export KUBECONFIG=${KUBECONFIG:-/etc/rancher/k3s/k3s.yaml}
namespace=${SPROUTFS_DEMO_NAMESPACE:-sproutfs}
out=/tmp/sproutfs-arena
agent_timeout=90
long=300s
forks=5
captures=3

ctl() { kubectl exec -n "$namespace" -i deploy/sproutfs-orchestrator -- sproutfsctl "$@" < /dev/null; }
now() { date +%s.%N; }
since() { awk -v a="$1" -v b="$(now)" 'BEGIN { printf "%.3f", b - a }'; }
step() { printf '\n=== %s ===\n' "$*"; }
fail() { printf '\nFAIL: %s\n' "$*" >&2; exit 1; }
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
# fill writes MiB of random bytes into a tmpfs file in the guest. The runs below
# read such a file back with md5sum rather than cat: busybox's cat splices into
# /dev/null, which never touches the pages and so faults none of them in.
fill() {
    local vm=$1 file=$2 mib=$3
    run_in "$vm" "mkdir -p /fill && { grep -qs ' /fill ' /proc/mounts || mount -t tmpfs -o size=1200m tmpfs /fill; } &&
        dd if=/dev/urandom of=/fill/$file bs=1M count=$mib 2>/dev/null" > /dev/null
}

# --- a host's own API and its processes -------------------------------------
work=$(mktemp -d /tmp/sproutfs-arena-work.XXXXXX)
chmod 0700 "$work"
forwarding=''
cleanup() {
    if [[ -n $forwarding ]]; then kill "$forwarding" 2> /dev/null || true; fi
    rm -rf -- "$work"
}
trap cleanup EXIT
kubectl get secret -n "$namespace" sproutfs-api-token -o jsonpath='{.data.token}' |
    base64 -d | awk '{ printf "Authorization: Bearer %s\n", $0 }' > "$work/header"

# status_of saves one host's /status, through a port-forward from the node.
status_of() {
    local pod=$1 into=$2 deadline=$(($(date +%s) + 30))
    kubectl port-forward -n "$namespace" "pod/$pod" 18081:8080 > "$work/forward.log" 2>&1 &
    forwarding=$!
    until curl -fsS -H @"$work/header" -o "$into" http://127.0.0.1:18081/status 2> /dev/null; do
        (($(date +%s) < deadline)) || { cat "$work/forward.log" >&2; fail "no status from $pod"; }
        sleep 0.5
    done
    kill "$forwarding" 2> /dev/null || true
    wait "$forwarding" 2> /dev/null || true
    forwarding=''
}
# sharing prints a saved status as one line per pager: its resident pages, and
# the unique, mapped and saved bytes of what it holds.
sharing() {
    python3 - "$1" <<'PY'
import json, sys
status = json.load(open(sys.argv[1]))
for kind in ("ram", "pmem"):
    p = status["pager"][kind]
    s = p["sharing"]
    print(f"{kind} resident_pages={p['resident_pages']} page_bytes={p['page_bytes']} "
          f"unique_mib={s['unique_bytes'] / 2**20:.1f} mapped_mib={s['mapped_bytes'] / 2**20:.1f} "
          f"saved_mib={s['saved_bytes'] / 2**20:.1f} faults={p['faults']} shared_pages={p['shared_pages']}")
PY
}
# mappings prints the number of mappings of every VMM in a host pod, one line
# per process. The host image has no procps, so /proc is read directly.
mappings() {
    # shellcheck disable=SC2016  # p is the pod shell's loop variable.
    kubectl exec -n "$namespace" "$1" -- sh -c '
        for p in /proc/[0-9]*; do
            [ -r "$p/cmdline" ] || continue
            if tr "\0" " " < "$p/cmdline" | grep -q "^/usr/local/bin/firecracker"; then
                printf "%s %s\n" "$(basename "$p")" "$(wc -l < "$p/maps")"
            fi
        done' | tr -d '\r'
}
# cpu_ticks is the user and system time the host process has used, in clock
# ticks: it is the container's first process, and the uploads run in it.
cpu_ticks() { kubectl exec -n "$namespace" "$1" -- cat /proc/1/stat | awk '{ print $14 + $15 }'; }
ticks=$(getconf CLK_TCK)

rm -rf -- "$out"
mkdir -p "$out"
modes=$(kubectl get pods -n "$namespace" -l app.kubernetes.io/name=sproutfs-host \
    -o jsonpath='{range .items[*]}{.metadata.name}={.spec.containers[0].env[?(@.name=="SPROUTFS_ARENA")]}{"\n"}{end}')
arena=$(kubectl get configmap -n "$namespace" sproutfs-demo -o jsonpath='{.data.arena}')
arena=${arena:-isolated}
printf 'sproutfs demo: the %s arena\n' "$arena" | tee "$out/mode.txt"
printf '%s\n' "$modes" >> "$out/mode.txt"
ctl hosts | tee "$out/hosts-before.txt"
if [[ -n $(ctl list | awk 'NR > 1') ]]; then
    ctl list >&2
    fail 'the deployment runs VMs already; measure on hosts that run none'
fi

# --- 1. a fan-out ---------------------------------------------------------------
step "a fan-out of $forks on the parent's own host"
created=$(ctl create --template alpine)
printf '%s\n' "$created" | tee "$out/fanout-create.txt"
parent=$(vm_of "$created")
agent_ready "$parent" || fail "the agent in $parent never answered"
fanout_host=$(host_of "$parent")
fill "$parent" published 256
ctl capture "$parent" | tee "$out/fanout-capture.txt"
fill "$parent" unpublished 64
began=$(now)
table=$(ctl fork "$parent" --count "$forks")
printf '%s\n' "$table" | tee "$out/fanout-fork.txt"
children=$(awk 'NR > 1 { print $1 }' <<< "$table")
((${#children} > 0)) || fail "the fork named no children"
fork_pause=$(awk 'NR == 2 { print $3 }' <<< "$table")
fork_total=$(awk 'NR == 2 { print $5 }' <<< "$table")
for child in $children; do
    ( agent_ready "$child" && printf '%s %s\n' "$child" "$(since "$began")" >> "$out/fanout-first-output.txt" ) &
done
wait
(($(wc -l < "$out/fanout-first-output.txt") == forks)) || fail 'not every child answered'
first_output=$(sort -k2 -n "$out/fanout-first-output.txt" | awk 'END { print $2 }')
began=$(now)
for child in $children; do
    ( run_in "$child" 'md5sum /fill/published /fill/unpublished > /dev/null' > /dev/null &&
        printf '%s %s\n' "$child" "$(since "$began")" >> "$out/fanout-read.txt" ) &
done
wait
(($(wc -l < "$out/fanout-read.txt") == forks)) || fail 'not every child read its memory back'
read_all=$(sort -k2 -n "$out/fanout-read.txt" | awk 'END { print $2 }')
status_of "$fanout_host" "$out/fanout-status.json"
sharing "$out/fanout-status.json" | tee "$out/fanout-sharing.txt"
mappings "$fanout_host" | tee "$out/fanout-mappings.txt"
fanout_saved=$(awk '{ split($6, s, "="); total += s[2] } END { printf "%.1f", total }' "$out/fanout-sharing.txt")
fanout_unique=$(awk '{ split($4, s, "="); total += s[2] } END { printf "%.1f", total }' "$out/fanout-sharing.txt")
fanout_maps=$(awk '{ total += $2; if ($2 > most) most = $2 } END { printf "%.0f mean, %d most", total / NR, most }' \
    "$out/fanout-mappings.txt")
for vm in $children "$parent"; do ctl delete "$vm" > /dev/null; done

# --- 2. checkpoints of a guest that wrote 1 GiB -----------------------------------
step "$captures checkpoints of 1 GiB of new memory"
created=$(ctl create --template alpine --memory 2G)
printf '%s\n' "$created" | tee "$out/capture-create.txt"
guest=$(vm_of "$created")
agent_ready "$guest" || fail "the agent in $guest never answered"
guest_host=$(host_of "$guest")
if [[ $guest_host == "$fanout_host" ]]; then
    elsewhere=$(other_host "$guest_host")
    [[ -n $elsewhere ]] && ctl migrate "$guest" --to "$elsewhere" > /dev/null && guest_host=$elsewhere
fi
since_logs=$(date -u +%FT%TZ)
for _ in $(seq 1 "$captures"); do
    fill "$guest" bytes 1024
    before=$(cpu_ticks "$guest_host")
    captured=$(ctl capture "$guest")
    after=$(cpu_ticks "$guest_host")
    cpu=$(awk -v a="$before" -v b="$after" -v t="$ticks" 'BEGIN { printf "%.2f", (b - a) / t }')
    printf '%s cpu %ss\n' "$captured" "$cpu" | tee -a "$out/captures.txt"
done
kubectl logs -n "$namespace" "$guest_host" --since-time="$since_logs" --tail=-1 |
    grep '"host: checkpoint"' | grep "\"$guest\"" > "$out/capture-log.jsonl" || true

# --- 3. restores ------------------------------------------------------------------
# Twice: onto the other host, whose arena holds none of this guest's pages, and
# back onto its own, whose arena may still hold them as idle pages.
restore() {
    local to=$1 name=$2 began started
    ctl stop "$guest" --suspend | tee "$out/restore-$name-stop.txt"
    began=$(now)
    started=$(ctl start "$guest" --to "$to")
    printf '%s\n' "$started" | tee "$out/restore-$name-start.txt"
    agent_ready "$guest" || fail "the agent in $guest never answered after its start on $to"
    since "$began" > "$out/restore-$name-output.txt"
    began=$(now)
    run_in "$guest" 'md5sum /fill/bytes > /dev/null' > /dev/null
    since "$began" > "$out/restore-$name-read.txt"
    status_of "$to" "$out/restore-$name-status.json"
    sharing "$out/restore-$name-status.json" | tee "$out/restore-$name-sharing.txt"
    mappings "$to" | tee "$out/restore-$name-mappings.txt"
}
step 'a stop with the memory and a start, on the other host and back'
restore "$(other_host "$guest_host")" away
restore "$guest_host" back
ctl delete "$guest" > /dev/null
ctl hosts | tee "$out/hosts-after.txt"

# --- the summary ------------------------------------------------------------------------
step "the $arena arena"
{
    printf 'arena                         %s\n' "$arena"
    printf 'fan-out pause (%d)             %ss\n' "$forks" "$fork_pause"
    printf 'fan-out total (%d)             %ss\n' "$forks" "$fork_total"
    printf 'fan-out first output, slowest %ss\n' "$first_output"
    printf 'fan-out read 320 MiB, slowest %ss\n' "$read_all"
    printf 'fan-out host unique           %s MiB\n' "$fanout_unique"
    printf 'fan-out host saved            %s MiB\n' "$fanout_saved"
    printf 'fan-out VMM mappings          %s\n' "$fanout_maps"
    awk '{ for (i = 1; i <= NF; i++) { if ($i == "pause") p = $(i + 1); if ($i == "publish") u = $(i + 1); if ($i == "cpu") c = $(i + 1) }
           printf "capture 1 GiB, round %d       pause %s publish %s cpu %s\n", NR, p, u, c }' "$out/captures.txt"
    python3 - "$out/capture-log.jsonl" <<'PY'
import json, sys
for line in open(sys.argv[1]):
    r = json.loads(line)
    print(f"  logged checkpoint {r['checkpoint']}: dirty {r['dirty_bytes'] / 2**20:.0f} MiB, "
          f"uploaded {r['uploaded_bytes'] / 2**20:.0f} MiB in {r['objects']} objects, "
          f"pause {r['pause_seconds']:.3f}s, upload {r['upload_seconds']:.3f}s")
PY
    for name in away back; do
        printf 'restore %-4s start            %s\n' "$name" \
            "$(sed -n 's/.* in \([0-9.]*s\)$/\1/p' "$out/restore-$name-start.txt")"
        printf 'restore %-4s first output     %ss\n' "$name" "$(cat "$out/restore-$name-output.txt")"
        printf 'restore %-4s read 1 GiB back  %ss\n' "$name" "$(cat "$out/restore-$name-read.txt")"
        printf 'restore %-4s VMM mappings     %s\n' "$name" \
            "$(awk '{ printf "%s ", $2 }' "$out/restore-$name-mappings.txt")"
    done
} | tee "$out/summary.txt"
