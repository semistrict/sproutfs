#!/usr/bin/env bash
# What the checkpoint model costs under a guest that is used, on the demo node.
#
# scripts/demo-gce.sh copies this one file to the node and runs it there:
#
#     bash demo-workload.sh
#
# It boots a VM from the workload template — Alpine with git, ripgrep, Node,
# pnpm and three MIT-licensed TypeScript repositories whose dependencies are in
# a pnpm store inside the image — checkpoints it, forks it, and drives the forks
# through four phases of work over `sproutfsctl exec`: ripgrep searches across
# the repositories, `git status` and `git log` in each, an offline
# `pnpm install`, and a build. A fifth phase does nothing at all, so that the
# cost of an idle guest's interval checkpoint is measured too. Every phase ends
# with an explicit checkpoint of every VM in play; the host's own 60 s interval
# goes on running underneath, so both kinds are in the numbers.
#
# What it records, under $SPROUTFS_DEMO_RUN_DIR:
#   phases.tsv       each phase's name and the wall-clock window it occupied
#   checkpoints.jsonl  every "host: checkpoint" line the hosts logged
#   store.tsv        each host's object-store counters at every phase boundary
#   shared.tsv       each host's resident and shared frame counts, likewise
#   summary.txt      the tables this prints
#
# Overridable: SPROUTFS_DEMO_NAMESPACE, FORKS_BASE, FORKS_PER_REPO,
# SPROUTFS_DEMO_RUN_DIR, SPROUTFS_DEMO_IDLE_SECONDS, SPROUTFS_DEMO_EXEC_TIMEOUT.
#
# FORKS_BASE is how many forks come off the base image's checkpoint and
# FORKS_PER_REPO how many come off each of those forks once it has installed its
# dependencies, which is a later checkpoint of a VM that has diverged. Both
# default to numbers that fit a host's 5 GiB arena; raising them is how a run
# asks what more sharing costs.
set -euo pipefail

export KUBECONFIG=${KUBECONFIG:-/etc/rancher/k3s/k3s.yaml}
namespace=${SPROUTFS_DEMO_NAMESPACE:-sproutfs}
template=${SPROUTFS_DEMO_WORKLOAD_TEMPLATE:-workload}
forks_base=${FORKS_BASE:-2}
forks_per_repo=${FORKS_PER_REPO:-1}
run_dir=${SPROUTFS_DEMO_RUN_DIR:-/tmp/sproutfs-workload}
# How long the idle phase lasts. It has to be longer than the host's checkpoint
# interval for an interval checkpoint of an untouched guest to land inside it,
# which is the whole point of the phase.
idle_seconds=${SPROUTFS_DEMO_IDLE_SECONDS:-75}
# A command's bound in the guest. An offline install and a build take tens of
# seconds; the agent's own ceiling is ten minutes.
exec_timeout=${SPROUTFS_DEMO_EXEC_TIMEOUT:-300s}
agent_timeout=180
boot_timeout=180

repos=(h3 unstorage ofetch)

# When this run began, to the second. The hosts' logs outlive it — an earlier
# run's VMs are still in them — so the checkpoint lines this one accounts for
# are only the ones logged from here on. Without it every earlier checkpoint
# lands in the "between" row and is counted as this run's work.
run_began=$(date -u +%FT%TZ)

rm -rf -- "$run_dir"
mkdir -p "$run_dir"
: > "$run_dir/phases.tsv"
: > "$run_dir/store.tsv"
: > "$run_dir/shared.tsv"

ctl() { kubectl exec -n "$namespace" -i deploy/sproutfs-orchestrator -- sproutfsctl "$@" < /dev/null; }
now() { date -u +%s.%N; }
step() { printf '\n=== %s ===\n' "$*"; }
fail() { printf '\nFAIL: %s\n' "$*" >&2; exit 1; }

# run_in runs one command in a guest. Only the guest's stdout comes back, and a
# command that exited non-zero fails this run.
run_in() { ctl exec "$1" --timeout "$exec_timeout" -- "${@:2}" < /dev/null; }

# agent_ready polls a guest until its agent answers, which is what a VM that has
# just booted or been forked needs before anything is asked of it.
agent_ready() {
    local vm=$1 deadline=$(($(date +%s) + agent_timeout)) last=
    while (($(date +%s) < deadline)); do
        if last=$(run_in "$vm" true 2>&1); then return 0; fi
    done
    printf '%s\n' "$last" >&2
    return 1
}

# root_each checkpoints every VM named, which for a VM that has just been forked
# is its own root index. A fork's parent keeps its frames sealed until the last
# child has published one — the child reads the parent's sealed frames until it
# owns those pages itself — so a parent cannot be checkpointed, forked again or
# migrated in the meantime. Taking the children's roots here is what gives the
# parent back to the phases below, and it is real work the numbers should carry:
# a child's root republishes the pages of the parent that no checkpoint held.
root_each() {
    local vm
    for vm in "$@"; do
        ctl capture "$vm" > /dev/null || fail "publishing the root checkpoint of $vm failed"
    done
}

# hosts_ready lists every host that can take work, in table order.
hosts_ready() { ctl hosts | awk 'NR > 1 && $2 == "true" { print $1 }'; }

# balance deals the VMs named round-robin over the ready hosts, moving only the
# ones that are not already where they should be. A migration carries the guest
# whole, so what it costs is in the numbers like everything else.
balance() {
    local -a all=("$@") ready
    mapfile -t ready < <(hosts_ready)
    ((${#ready[@]} > 1)) || return 0
    local index=0 vm want at
    for vm in "${all[@]}"; do
        want=${ready[index % ${#ready[@]}]}
        index=$((index + 1))
        at=$(ctl list | awk -v vm="$vm" '$1 == vm && $2 != "-" { print $2 }')
        [[ -n $at && $at != "$want" ]] || continue
        ctl migrate "$vm" --to "$want" || fail "spreading $vm onto $want failed"
    done
}

# sample records what every host's store and pager hold at one instant, marked
# with the boundary it belongs to. Two samples subtracted are one phase's cost.
sample() {
    local mark=$1 stamp
    stamp=$(now)
    ctl store | awk -v mark="$mark" -v at="$stamp" 'NR > 1 {
        printf "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", mark, at, $1, $2, $3, $4, $5 }' \
        >> "$run_dir/store.tsv"
    ctl hosts | awk -v mark="$mark" -v at="$stamp" 'NR > 1 {
        printf "%s\t%s\t%s\t%s\t%s\t%s\n", mark, at, $1, $3, $5, $6 }' \
        >> "$run_dir/shared.tsv"
}

# The VMs in play. Every phase runs on all of them and ends with a checkpoint of
# each, so one phase window covers the same work on every VM.
active=()
phase_name=
phase_began=

phase_start() {
    phase_name=$1
    phase_began=$(now)
    step "phase $phase_name on ${#active[@]} VMs: ${active[*]}"
}

# phase_end checkpoints every VM in play and closes the window. The capture
# waits for its publication, so the phase ends when its last byte is durable.
phase_end() {
    local vm
    for vm in "${active[@]}"; do
        ctl capture "$vm" > /dev/null || fail "capturing $vm at the end of $phase_name failed"
    done
    printf '%s\t%s\t%s\n' "$phase_name" "$phase_began" "$(now)" >> "$run_dir/phases.tsv"
    sample "$phase_name"
    printf 'phase %s done\n' "$phase_name"
}

# each runs one command in every VM in play, at the same time, because forks
# running together is the case being measured.
each() {
    local vm pids=() failed=0
    for vm in "${active[@]}"; do
        ( run_in "$vm" "$1" > "$run_dir/$phase_name.$vm.out" 2>&1 ) &
        pids+=($!)
    done
    for pid in "${pids[@]}"; do
        wait "$pid" || failed=1
    done
    ((failed == 0)) || {
        tail -n 40 "$run_dir/$phase_name."*.out >&2
        fail "the $phase_name phase failed in at least one guest"
    }
}

printf 'sproutfs workload: %d forks off the base, %d off each later checkpoint\n' \
    "$forks_base" "$forks_per_repo"
ctl hosts
sample begin

# --- the base VM ------------------------------------------------------------
step "create a VM from the $template template"
created=$(ctl create --template "$template")
printf '%s\n' "$created"
base=${created%% *}
[[ $base == vm-* ]] || fail "create did not name a VM: $created"
deadline=$(($(date +%s) + boot_timeout))
until agent_ready "$base"; do
    (($(date +%s) < deadline)) || fail "the agent in $base never answered"
done
printf 'the base VM %s is up\n' "$base"
run_in "$base" 'cat /root/repos/MANIFEST' | tee "$run_dir/repos.tsv"

active=("$base")
phase_start boot
phase_end

# --- fork the base ----------------------------------------------------------
step "fork $base $forks_base times"
table=$(ctl fork "$base" --count "$forks_base")
printf '%s\n' "$table"
mapfile -t workers < <(printf '%s\n' "$table" | awk 'NR > 1 { print $1 }')
((${#workers[@]} == forks_base)) || fail "asked for $forks_base forks and got ${#workers[@]}"
for vm in "${workers[@]}"; do
    agent_ready "$vm" || fail "the agent in the fork $vm never answered"
done
root_each "${workers[@]}"

# Every fork lands on its parent's host, and every fork of a fork lands on that
# one's, so left alone the whole run piles onto the host the base was created
# on while the other sits idle with its whole arena unused. One pause still
# starts every worker — that is the number this measures — and the workers are
# spread over the ready hosts afterwards, so that what each host is asked to
# keep resident is what its arena can hold. Without this a run of more than
# about six 2 GiB guests spends its time in one host's spill file, and a guest
# starved there stops answering long enough to fail the run.
balance "${workers[@]}"
sample forked
active=("${workers[@]}")

# --- the phases -------------------------------------------------------------
phase_start search
each "cd /root/repos && rg --stats --no-messages 'export (async )?function' ${repos[*]} | tail -12"
phase_end

phase_start history
each "for r in ${repos[*]}; do cd /root/repos/\$r && git status --short && git log --oneline -20; done"
phase_end

phase_start install
each "for r in ${repos[*]}; do cd /root/repos/\$r && pnpm install --offline --frozen-lockfile; done"
phase_end

# --- fork each worker off a checkpoint it has diverged into -----------------
if ((forks_per_repo > 0)); then
    step "fork each worker $forks_per_repo times off its post-install checkpoint"
    children=()
    for vm in "${workers[@]}"; do
        table=$(ctl fork "$vm" --count "$forks_per_repo")
        printf '%s\n' "$table"
        mapfile -t born < <(printf '%s\n' "$table" | awk 'NR > 1 { print $1 }')
        ((${#born[@]} == forks_per_repo)) ||
            fail "asked for $forks_per_repo forks of $vm and got ${#born[@]}"
        children+=("${born[@]}")
    done
    for vm in "${children[@]}"; do
        agent_ready "$vm" || fail "the agent in the fork $vm never answered"
    done
    root_each "${children[@]}"
    sample refork
    active=("${workers[@]}" "${children[@]}")
fi

phase_start build
each 'cd /root/repos/h3 && pnpm build'
phase_end

# The idle phase is the baseline: nothing is asked of any guest, and the only
# checkpoints in its window are the hosts' own interval ones. It runs longer
# than that interval so at least one of them lands inside it.
phase_start idle
sleep "$idle_seconds"
printf '%s\t%s\t%s\n' idle "$phase_began" "$(now)" >> "$run_dir/phases.tsv"
sample idle
printf 'phase idle done\n'

# --- what it cost -----------------------------------------------------------
step 'collecting'
sample end
kubectl logs -n "$namespace" -l app.kubernetes.io/name=sproutfs-host \
    --since-time="$run_began" --tail=-1 --prefix=false > "$run_dir/host.log"
grep '"msg":"host: checkpoint"' "$run_dir/host.log" > "$run_dir/checkpoints.jsonl" ||
    fail 'no host logged a checkpoint line'
printf '%d checkpoint lines\n' "$(wc -l < "$run_dir/checkpoints.jsonl")"

python3 - "$run_dir" "$forks_base" "$forks_per_repo" > "$run_dir/summary.txt" <<'SUMMARY'
"""Join the hosts' checkpoint lines to the phases they fell in, and tabulate."""
import collections, datetime, json, pathlib, sys

run = pathlib.Path(sys.argv[1])
forks_base, forks_per_repo = sys.argv[2], sys.argv[3]
MIB = 1024 * 1024


def epoch(value):
    text = value.replace("Z", "+00:00")
    return datetime.datetime.fromisoformat(text).timestamp()


phases = []
for line in (run / "phases.tsv").read_text().splitlines():
    name, began, ended = line.split("\t")
    phases.append((name, float(began), float(ended)))


def phase_of(at):
    for name, began, ended in phases:
        if began <= at <= ended:
            return name
    return "between"


rows = []
for line in (run / "checkpoints.jsonl").read_text().splitlines():
    entry = json.loads(line)
    rows.append(
        {
            "phase": phase_of(epoch(entry["time"])),
            "vm": entry["vm"],
            "sequence": entry["checkpoint"],
            "pages": entry["dirty_pages"],
            "dirty": entry["dirty_bytes"],
            "uploaded": entry["uploaded_bytes"],
            "objects": entry["objects"],
            "deleted": entry.get("deleted", 0),
            "read": entry.get("read_bytes", 0),
            "pause": entry["pause_seconds"],
            "upload": entry["upload_seconds"],
            "dirty4k": entry.get("dirty_4k_pages"),
        }
    )

order = [name for name, _, _ in phases] + ["between"]


def table(title, columns, lines):
    width = [len(c) for c in columns]
    for line in lines:
        for index, cell in enumerate(line):
            width[index] = max(width[index], len(str(cell)))
    print(f"\n## {title}\n")
    print("  ".join(c.ljust(width[i]) for i, c in enumerate(columns)))
    print("  ".join("-" * w for w in width))
    for line in lines:
        print("  ".join(str(c).ljust(width[i]) for i, c in enumerate(line)))


print(f"forks off the base: {forks_base}; forks off each later checkpoint: {forks_per_repo}")
print(f"checkpoints seen: {len(rows)}")

grouped = collections.defaultdict(list)
for row in rows:
    grouped[(row["phase"], row["vm"])].append(row)

lines = []
for phase in order:
    for (name, vm), group in sorted(grouped.items()):
        if name != phase:
            continue
        lines.append(
            [
                phase,
                vm,
                len(group),
                sum(g["pages"] for g in group),
                f"{sum(g['dirty'] for g in group) / MIB:.1f}",
                f"{sum(g['uploaded'] for g in group) / MIB:.1f}",
                sum(g["objects"] for g in group),
                f"{max(g['pause'] for g in group):.3f}",
                f"{max(g['upload'] for g in group):.2f}",
            ]
        )
table(
    "per phase and VM",
    ["phase", "vm", "checkpoints", "2MiB pages", "dirty MiB", "up MiB", "objects", "max pause s", "max upload s"],
    lines,
)

lines = []
for phase in order:
    group = [r for r in rows if r["phase"] == phase]
    if not group:
        continue
    dirty = sum(g["dirty"] for g in group)
    fine = [g["dirty4k"] for g in group if g["dirty4k"] is not None]
    ratio = f"{dirty / (sum(fine) * 4096):.1f}x" if fine and sum(fine) else "n/a"
    lines.append(
        [
            phase,
            len(group),
            sum(g["pages"] for g in group),
            f"{dirty / MIB:.1f}",
            f"{sum(g['uploaded'] for g in group) / MIB:.1f}",
            sum(g["objects"] for g in group),
            sum(g["deleted"] for g in group),
            f"{sum(g['pause'] for g in group) / len(group):.3f}",
            ratio,
        ]
    )
table(
    "per phase",
    ["phase", "checkpoints", "2MiB pages", "dirty MiB", "up MiB", "objects", "deletes", "mean pause s", "2MiB:4KiB"],
    lines,
)

store = collections.defaultdict(dict)
for line in (run / "store.tsv").read_text().splitlines():
    mark, _at, host, op, calls, failures, byte = line.split("\t")
    store[(host, op)][mark] = (int(calls), int(failures), int(byte))

lines = []
for (host, op), marks in sorted(store.items()):
    first = marks.get("begin", (0, 0, 0))
    last = marks.get("end", first)
    lines.append([host, op, last[0] - first[0], last[1] - first[1], f"{(last[2] - first[2]) / MIB:.1f}"])
table("object store over the run", ["host", "op", "calls", "failed", "MiB"], lines)

lines = []
for line in (run / "shared.tsv").read_text().splitlines():
    mark, _at, host, running, resident, shared = line.split("\t")
    lines.append([mark, host, running, resident, shared])
table("frames at each boundary", ["mark", "host", "running", "resident", "shared"], lines)
SUMMARY
cat "$run_dir/summary.txt"

# --- clean up ---------------------------------------------------------------
step 'closing the VMs'
for vm in "${active[@]}" "${workers[@]}" "$base"; do
    ctl delete "$vm" > /dev/null 2>&1 || true
done
ctl list || true
printf '\nWorkload run complete. Everything it recorded is under %s.\n' "$run_dir"
