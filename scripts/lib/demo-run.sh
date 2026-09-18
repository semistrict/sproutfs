#!/usr/bin/env bash
# The demo's five flows, non-interactively, on the demo node.
#
# scripts/demo-gce.sh copies this one file to the node and runs it there:
#
#     bash demo-run.sh
#
# It drives the deployment through sproutfsctl, which is in the same image as
# the orchestrator, so nothing has to be forwarded out of the cluster. Every
# flow asserts what the guest printed back; the first one that does not hold
# ends the run non-zero, and the timings of everything that did hold are printed
# at the end.
#
# Overridable: SPROUTFS_DEMO_NAMESPACE, SPROUTFS_DEMO_FORKS.
set -euo pipefail

export KUBECONFIG=${KUBECONFIG:-/etc/rancher/k3s/k3s.yaml}
namespace=${SPROUTFS_DEMO_NAMESPACE:-sproutfs}
forks=${SPROUTFS_DEMO_FORKS:-5}
# The console window a scripted command is read over. A guest answers a shell
# command in well under a second; the rest is slack for a host under load.
answer=8s
# How long a boot, a recovery and a killed host's disappearance are waited for,
# and how long a guest has to start answering its agent after each of those.
boot_timeout=120
recover_timeout=120
agent_timeout=90
exec_timeout=20s

# ctl drives the deployment. sproutfsctl is in the orchestrator's own image and
# reaches the orchestrator at its default address, so the demo needs no
# port-forward to run here.
ctl() { kubectl exec -n "$namespace" -i deploy/sproutfs-orchestrator -- sproutfsctl "$@" < /dev/null; }

now() { date +%s.%N; }
elapsed() { awk -v a="$1" -v b="$(now)" 'BEGIN { printf "%.2f", b - a }'; }
step() { printf '\n=== %s ===\n' "$*"; }
fail() { printf '\nFAIL: %s\n' "$*" >&2; exit 1; }

# say runs one line in a guest's shell and prints everything the console holds.
# The marker is written with a quote in the middle of it, so that the line the
# serial terminal echoes back does not match what the guest prints: a flow that
# looks for the marker is then reading the guest's answer and not its own input.
say() {
    local vm=$1 line=$2
    printf '%s\n' "$line" | ctl console "$vm" --for "$answer"
}

# answered runs one line in a guest's shell and says whether the console came
# back holding the pattern. Nothing is piped into a matcher: a matcher that
# stops at the first hit closes the pipe under the reader, and under pipefail
# that reads as a failed flow rather than a satisfied one.
answered() {
    local vm=$1 line=$2 pattern=$3 seen
    seen=$(say "$vm" "$line")
    [[ $seen == *$pattern* ]]
}

# await_console polls a guest's console until it holds a pattern.
await_console() {
    local vm=$1 pattern=$2 deadline=$(($(date +%s) + boot_timeout)) seen=
    while (($(date +%s) < deadline)); do
        seen=$(ctl console "$vm" --for 1s < /dev/null)
        if [[ $seen == *$pattern* ]]; then
            return 0
        fi
    done
    printf '%s\n' "$seen" >&2
    return 1
}

# witness_holds asks a guest to print the shell variable set in it before the
# migration and the kill. Only that guest's own memory carries it, so a VM that
# still answers with it is the same guest and not a fresh boot. $W is expanded
# by the guest's shell, never by this one.
# shellcheck disable=SC2016
witness_holds() { answered "$1" 'echo $W' 'sproutfs-witness-[0-9]'; }

# run_in runs one command in a guest through the orchestrator, which reaches it
# over the VM's vsock. Only the guest's stdout comes back here: sproutfsctl
# keeps the guest's two streams apart, and a command that exited non-zero makes
# it exit non-zero too.
run_in() { ctl exec "$1" --timeout "$exec_timeout" -- "${@:2}" < /dev/null; }

# agent_ready polls a guest until its agent answers a command. A VM that has
# just booted, forked or migrated is reachable within a second or two; this is
# what makes the difference between "not yet" and "never" a timeout rather than
# a race.
agent_ready() {
    local vm=$1 deadline=$(($(date +%s) + agent_timeout)) last=
    while (($(date +%s) < deadline)); do
        if last=$(run_in "$vm" true 2>&1); then
            return 0
        fi
    done
    printf '%s\n' "$last" >&2
    return 1
}

# await_other_host waits for a ready host that is not the named one. After the
# kill flow the deployment is one pod short until its replacement is scheduled,
# and a migration has nowhere to go until it is.
await_other_host() {
    local current=$1 deadline=$(($(date +%s) + boot_timeout)) other=
    while (($(date +%s) < deadline)); do
        other=$(other_host "$current")
        if [[ -n $other ]]; then
            printf '%s\n' "$other"
            return 0
        fi
        sleep 2
    done
    return 1
}

# host_of is the host running one VM, and nothing for a VM whose host is gone,
# which the listing writes as a dash.
host_of() { ctl list | awk -v vm="$1" '$1 == vm && $2 != "-" { print $2 }'; }
shared_of() { ctl hosts | awk -v host="$1" '$1 == host { print $6 }'; }
# served_of is how many pages one host has handed to another out of its own
# pages, which is what a fork placed on another host pulls: it is the page
# server's own counter, from the host's /status.
served_of() { ctl hosts | awk -v host="$1" '$1 == host { print $7 }'; }
# other_host is a ready host that is not the named one, which is where a
# migration goes.
other_host() { ctl hosts | awk -v host="$1" 'NR > 1 && $1 != host && $2 == "true" { print $1; exit }'; }

printf 'sproutfs demo: five flows against %s\n' "$namespace"
ctl hosts

# --- 1. create and talk to the guest ---------------------------------------
step 'create a VM and run a command in it'
began=$(now)
created=$(ctl create --template alpine)
printf '%s\n' "$created"
vm=${created%% *}
[[ $vm == vm-* ]] || fail "create did not name a VM: $created"
await_console "$vm" 'sproutfs demo guest is up' || fail "$vm never reached a prompt"
create_seconds=$(elapsed "$began")
printf 'create-to-prompt: %ss\n' "$create_seconds"

answered "$vm" "uname -a; echo SPROUTFS'-'OK" 'SPROUTFS-OK' ||
    fail "$vm did not answer a command on its console"
printf 'the guest answered a command\n'

# The witness is what proves the migration and the recovery kept this guest
# rather than booting another: it is shell state, which only survives if the
# guest's own memory does.
say "$vm" "W=sproutfs-witness-\$\$" > /dev/null
witness_holds "$vm" || fail "the witness did not take in $vm"
printf 'witness set in the guest shell\n'

# --- 2. fork ----------------------------------------------------------------
# A fork is a migration handoff from a parent that keeps running: the parent
# pauses for its VMM state capture and the seal and publishes nothing. On its
# own host the child shares the sealed pages through the pager; on another the
# child pulls the pages no checkpoint holds out of the parent's page server.
step "fork $vm $forks times on its own host"
parent_host=$(host_of "$vm")
[[ -n $parent_host ]] || fail "no host reports running $vm"
before=$(shared_of "$parent_host")
table=$(ctl fork "$vm" --count "$forks")
printf '%s\n' "$table"
children=$(printf '%s\n' "$table" | awk 'NR > 1 { print $1 }')
count=$(wc -w <<< "$children")
((count == forks)) || fail "asked for $forks forks and got $count"
# Every child of one request comes from one pause of the parent, so the pause
# and the total are the fork's, not each child's.
fork_pause=$(printf '%s\n' "$table" | awk 'NR == 2 { print $3 }')
fork_total=$(printf '%s\n' "$table" | awk 'NR == 2 { print $5 }')

for child in $children; do
    answered "$child" "echo FORK'-'OK" 'FORK-OK' || fail "the fork $child did not answer"
done
printf 'every fork answered on its own console\n'
after=$(shared_of "$parent_host")
printf 'shared pages on %s: %s before the forks, %s after\n' "$parent_host" "$before" "$after"
((after > before)) || fail "forking shared no pages on $parent_host: $before then $after"

for child in $children; do
    ctl delete "$child" > /dev/null
done
printf 'the same-host forks are closed\n'

# The same fork, placed on the other host. Nothing about the parent changes:
# it keeps running here and serves the child the pages no checkpoint holds.
step "fork $vm onto another host"
away_host=$(other_host "$parent_host")
[[ -n $away_host ]] || fail "no other host to fork $vm onto"
served_before=$(served_of "$parent_host")
away_table=$(ctl fork "$vm" --to "$away_host")
printf '%s\n' "$away_table"
away_child=$(printf '%s\n' "$away_table" | awk 'NR == 2 { print $1 }')
[[ $away_child == vm-* ]] || fail "forking onto $away_host named no child: $away_table"
away_pause=$(printf '%s\n' "$away_table" | awk 'NR == 2 { print $3 }')
away_total=$(printf '%s\n' "$away_table" | awk 'NR == 2 { print $5 }')
[[ $(host_of "$away_child") == "$away_host" ]] || fail "$away_child is not on $away_host"
[[ $(host_of "$vm") == "$parent_host" ]] || fail "$vm left $parent_host to be forked"
answered "$away_child" "echo AWAY'-'FORK'-'OK" 'AWAY-FORK-OK' ||
    fail "the fork $away_child on $away_host did not answer"
served_after=$(served_of "$parent_host")
printf 'pages %s served out of its own pages: %s before the fork, %s after\n' \
    "$parent_host" "$served_before" "$served_after"
((served_after > served_before)) ||
    fail "the cross-host fork pulled nothing from $parent_host: $served_before then $served_after"
printf 'cross-host fork: pause %ss, total %ss\n' "$away_pause" "$away_total"
ctl delete "$away_child" > /dev/null
printf 'the cross-host fork is closed\n'

# --- 3. migrate -------------------------------------------------------------
step "migrate $vm off $parent_host"
destination=$(other_host "$parent_host")
[[ -n $destination ]] || fail "no other host to migrate $vm to"
moved=$(ctl migrate "$vm" --to "$destination")
printf '%s\n' "$moved"
migrate_pause=$(printf '%s' "$moved" | sed -n 's/.*pause \([0-9.]*\)s.*/\1/p')
migrate_stream=$(printf '%s' "$moved" | sed -n 's/.*stream \([0-9.]*\)s.*/\1/p')
[[ $(host_of "$vm") == "$destination" ]] || fail "$vm is not on $destination after the migration"
witness_holds "$vm" || fail "the console session did not continue on $destination"
printf 'the same guest answered on %s: pause %ss, stream %ss\n' "$destination" "$migrate_pause" "$migrate_stream"

# --- 4. lose a host and recover --------------------------------------------
step "kill $destination and recover $vm"
# Nothing since the last checkpoint survives the kill, so the run takes one
# explicitly rather than waiting out the interval: what a real loss rewinds the
# guest by is bounded by that interval and nothing else.
ctl capture "$vm"
ctl kill-host "$destination"
deadline=$(($(date +%s) + recover_timeout))
while [[ -n $(host_of "$vm") ]]; do
    (($(date +%s) < deadline)) || fail "a host still reports running $vm after the kill"
    sleep 2
done
printf '%s is gone and no host runs %s\n' "$destination" "$vm"

began=$(now)
deadline=$(($(date +%s) + recover_timeout))
recovered=''
while :; do
    if recovered=$(ctl recover "$vm" 2>&1); then break; fi
    (($(date +%s) < deadline)) || fail "recovering $vm failed: $recovered"
    sleep 2
done
recover_seconds=$(elapsed "$began")
printf '%s\n' "$recovered"
await_console "$vm" 'sproutfs' || fail "$vm printed nothing after being recovered"
witness_holds "$vm" || fail "the recovered $vm lost the state it had before the kill"
printf 'the recovered guest still holds the witness it had before the kill\n'

# --- 5. run commands in the guests -----------------------------------------
# The console proves a guest is alive to a person. This proves it to a program:
# the orchestrator carries a command into the VM over its vsock, and the same
# URL keeps working across a fork and a migration without anything being
# allocated, addressed or carried between hosts.
step "run commands in $vm, in a fork of it, and after moving it"
agent_ready "$vm" || fail "the agent in $vm never answered"

began=$(now)
seen=$(run_in "$vm" 'echo EXEC-OK; hostname') || fail "exec in $vm failed: $seen"
[[ $seen == *EXEC-OK* ]] || fail "exec in $vm printed $seen"
exec_seconds=$(elapsed "$began")
printf 'exec in %s:\n%s\n' "$vm" "$seen"

# A file written in one guest is what tells a fork from its parent: the child
# inherits everything the parent had at the checkpoint and diverges from there.
run_in "$vm" 'echo parent > /run/who' > /dev/null || fail "writing in $vm failed"

began=$(now)
child=$(ctl fork "$vm" --count 1 | awk 'NR == 2 { print $1 }')
[[ $child == vm-* ]] || fail "forking $vm named no child: $child"
agent_ready "$child" || fail "the agent in the fork $child never answered"
fork_exec_seconds=$(elapsed "$began")
seen=$(run_in "$child" 'cat /run/who; echo FORK-EXEC-OK') || fail "exec in $child failed: $seen"
[[ $seen == *parent* ]] || fail "the fork $child did not inherit the parent's file: $seen"
[[ $seen == *FORK-EXEC-OK* ]] || fail "exec in the fork $child printed $seen"
printf 'exec in the fork %s:\n%s\n' "$child" "$seen"
ctl delete "$child" > /dev/null

# The witness is shell state in the guest's own memory; this is a file in its
# own filesystem. Both have to cross the move.
run_in "$vm" 'echo before-the-move > /run/move' > /dev/null || fail "writing in $vm failed"
current=$(host_of "$vm")
elsewhere=$(await_other_host "$current") ||
    fail "no second host came back after the kill, so $vm has nowhere to move to"
moved=$(ctl migrate "$vm" --to "$elsewhere")
printf '%s\n' "$moved"
exec_pause=$(printf '%s' "$moved" | sed -n 's/.*pause \([0-9.]*\)s.*/\1/p')

began=$(now)
agent_ready "$vm" || fail "the agent in $vm did not answer after the move to $elsewhere"
moved_exec_seconds=$(elapsed "$began")
seen=$(run_in "$vm" 'cat /run/move; echo MOVED-EXEC-OK') || fail "exec in the moved $vm failed: $seen"
[[ $seen == *before-the-move* ]] || fail "the moved $vm lost the file written before the move: $seen"
[[ $seen == *MOVED-EXEC-OK* ]] || fail "exec in the moved $vm printed $seen"
printf 'exec in %s on %s:\n%s\n' "$vm" "$elsewhere" "$seen"

# The table is what routed every one of those without anyone naming a host.
ctl list

# --- what it cost -----------------------------------------------------------
step 'timings'
printf 'create to prompt      %ss\n' "$create_seconds"
printf 'fork pause (%d)        %ss\n' "$forks" "$fork_pause"
printf 'fork total (%d)        %ss\n' "$forks" "$fork_total"
printf 'cross-host fork       pause %ss, total %ss\n' "$away_pause" "$away_total"
printf 'migration pause       %ss\n' "$migrate_pause"
printf 'migration stream      %ss\n' "$migrate_stream"
printf 'recover to running    %ss\n' "$recover_seconds"
printf 'exec in a booted VM   %ss\n' "$exec_seconds"
printf 'fork to exec          %ss\n' "$fork_exec_seconds"
printf 'migration pause (5)   %ss\n' "$exec_pause"
printf 'move to exec          %ss\n' "$moved_exec_seconds"
ctl hosts
ctl delete "$vm" > /dev/null || true
printf '\nAll five flows passed.\n'
