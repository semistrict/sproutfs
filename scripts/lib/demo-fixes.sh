#!/usr/bin/env bash
# The blocker fixes that only a live cluster can show, on the demo node.
#
# scripts/demo-gce.sh copies this one file to the node and runs it there:
#
#     bash demo-fixes.sh
#
# With no argument it runs all six; naming one or more of fork-destination,
# recovery, vmm-death, rollout, fanout-rollback and lost-migration runs those. The rollout is the one a model of
# the deployment can answer for, and scripts/test/fixes-test.sh runs it that way.
#
# demo-run.sh proves the five flows work. This proves six things that are
# about what happens when they do not, and that a unit or a simulation test
# cannot reach because they need a real pod to delete, a real VMM to kill and a
# real rollout to sit through:
#
#   1. A cross-host fork whose destination dies leaves the parent running and
#      checkpointing (plans/production-review-2026-09-14.md blocker 7). Before
#      the fix the parent stayed sealed forever: never checkpointed, never
#      fenced, never migratable. Two things now end the hold — the orchestrator
#      releases it, and failing that the host's own deadline expires it — so the
#      assertion is on the parent, not on which one got there first.
#   2. A recovery is refused while the VM's host is alive and answering, and
#      proceeds once that host's pod is gone (blocker 6). Before the fix one
#      failed Status call was enough to start a second guest.
#   3. A VMM killed underneath its host leaves a diagnostic naming the VM, the
#      cause and the console tail, and the host forgets the VM (blocker 8).
#      Before the fix the machine stayed registered and the only line was a
#      connection-refused checkpoint failure up to a minute later.
#   4. A rollout of the host Deployment migrates every VM rather than losing it
#      (plans/production-review-2026-09-14b.md, control plane). Before the fix
#      the Deployment used strategy Recreate, which deletes both host pods at
#      once: the preStop drain had no destination, so every rollout rewound
#      every VM to its last checkpoint. The assertion is the guest's own
#      memory — a witness written after that checkpoint, which only a migrated
#      guest still holds — and then a create on the pods that came back, which
#      is what says they were ready: they carry names of their own, and the
#      template they create from is the one their predecessors imported, named
#      by the guest image's bytes and found already published.
#   5. A same-host fan-out that fails part way takes back the children it
#      started, and the parent takes its sealed pages back and checkpoints
#      again. The host's logical cap is what refuses the second child.
#   6. A host lost during a real migration leaves every VM running or
#      reopenable. A destination lost mid-stream is retried on the pod that
#      replaces it, and the guest keeps the bytes no checkpoint had. A source
#      lost mid-stream ends the receive, and the VM comes back at its last
#      checkpoint, as does a bystander on that host.
#
# Overridable: SPROUTFS_DEMO_NAMESPACE, SPROUTFS_DEMO_HOLD_TIMEOUT.
set -euo pipefail

export KUBECONFIG=${KUBECONFIG:-/etc/rancher/k3s/k3s.yaml}
namespace=${SPROUTFS_DEMO_NAMESPACE:-sproutfs}
# The host retires a fork hold after four checkpoint intervals, so a release
# that nothing else beat it to lands inside five minutes. The orchestrator's own
# reconciliation is much faster; this is the outer bound on both.
hold_timeout=${SPROUTFS_DEMO_HOLD_TIMEOUT:-330}
boot_timeout=120
agent_timeout=90
exec_timeout=20s

ctl() { kubectl exec -n "$namespace" -i deploy/sproutfs-orchestrator -- sproutfsctl "$@" < /dev/null; }
now() { date +%s.%N; }
step() { printf '\n=== %s ===\n' "$*"; }
fail() { printf '\nFAIL: %s\n' "$*" >&2; exit 1; }

run_in() { ctl exec "$1" --timeout "$exec_timeout" -- "${@:2}" < /dev/null; }

agent_ready() {
    local vm=$1 deadline=$(($(date +%s) + agent_timeout)) last=
    while (($(date +%s) < deadline)); do
        if last=$(run_in "$vm" true 2>&1); then return 0; fi
    done
    printf '%s\n' "$last" >&2
    return 1
}

host_of() { ctl list | awk -v vm="$1" '$1 == vm && $2 != "-" { print $2 }'; }
other_host() { ctl hosts | awk -v host="$1" 'NR > 1 && $1 != host && $2 == "true" { print $1; exit }'; }
# serving_of is how many handovers one host still holds pages for: the VMs it
# migrated away and the children of every fork point it took, wherever those
# children landed. A parent whose fork hold was never released never leaves 1.
serving_of() { ctl hosts | awk -v host="$1" '$1 == host { print $4 }'; }
# checkpoint_of is the sequence the VM's latest checkpoint carries. A sealed
# parent cannot checkpoint, so this is what stops moving when a hold leaks. It is
# the first field after the host that is a number alone: a migrating VM's state
# is two fields, and the loss window and the private memory after the sequence
# may be two each.
checkpoint_of() {
    ctl list | awk -v vm="$1" '$1 == vm { for (i = 3; i <= NF; i++) if ($i ~ /^[0-9]+$/) { print $i; exit } }'
}
running_on() { ctl hosts | awk -v host="$1" '$1 == host { print $3 }'; }
# fill_timeout bounds a fill of a guest's memory and a read of it back.
fill_timeout=180s
ready_hosts() { ctl hosts | awk 'NR > 1 && $2 == "true" { print $1 }' | wc -l; }

await_hosts() {
    local want=$1 deadline=$(($(date +%s) + boot_timeout))
    while (($(date +%s) < deadline)); do
        (($(ready_hosts) < want)) || return 0
        sleep 2
    done
    ctl hosts >&2
    return 1
}

fix_fork_destination() {
    # --- 1. a cross-host fork whose destination dies ---------------------------
    step 'a cross-host fork whose destination pod is deleted'
    created=$(ctl create --template alpine)
    parent=${created%% *}
    [[ $parent == vm-* ]] || fail "create did not name a VM: $created"
    agent_ready "$parent" || fail "the agent in $parent never answered"
    parent_host=$(host_of "$parent")
    [[ -n $parent_host ]] || fail "no host reports running $parent"
    away=$(other_host "$parent_host")
    [[ -n $away ]] || fail "no other host to fork $parent onto"
    printf '%s runs on %s; forking it onto %s\n' "$parent" "$parent_host" "$away"

    before=$(checkpoint_of "$parent")
    printf 'the parent is at checkpoint %s\n' "$before"

    # The fork and the destination's death race on purpose: the point is that the
    # parent survives whichever order they land in. The fork itself is allowed to
    # fail — its destination is being deleted underneath it.
    ( ctl fork "$parent" --to "$away" > /tmp/sproutfs-fix-fork.out 2>&1; \
      printf '%s\n' "$?" > /tmp/sproutfs-fix-fork.rc ) &
    forking=$!
    kubectl delete pod -n "$namespace" "$away" --grace-period=0 --force > /dev/null 2>&1 || true
    printf 'deleted the destination pod %s\n' "$away"
    wait "$forking" || true
    printf 'the fork returned %s:\n' "$(cat /tmp/sproutfs-fix-fork.rc)"
    sed -n '1,10p' /tmp/sproutfs-fix-fork.out

    # The parent is what this is about. It never stopped running, so it has to keep
    # answering and keep checkpointing, and the host has to stop serving for it.
    [[ $(host_of "$parent") == "$parent_host" ]] ||
        fail "$parent left $parent_host, which never had anything asked of it"
    agent_ready "$parent" || fail "the parent $parent stopped answering after the fork"
    printf 'the parent still answers on %s\n' "$parent_host"

    began=$(now)
    deadline=$(($(date +%s) + hold_timeout))
    released=false
    advanced=false
    while (($(date +%s) < deadline)); do
        [[ $(serving_of "$parent_host") == 0 ]] && released=true
        [[ $(checkpoint_of "$parent") != "$before" ]] && advanced=true
        if "$released" && "$advanced"; then break; fi
        sleep 5
    done
    after=$(checkpoint_of "$parent")
    took=$(awk -v a="$began" -v b="$(now)" 'BEGIN { printf "%.0f", b - a }')
    "$released" || fail "$parent_host still serves a fork hold $took s after its destination died"
    "$advanced" || fail "$parent never checkpointed again: still at $before after $took s"
    printf 'the hold was released and %s checkpointed again (%s then %s) in %ss\n' \
        "$parent" "$before" "$after" "$took"
    # Whichever of the two mechanisms got there is worth naming in the log.
    kubectl logs -n "$namespace" -l app.kubernetes.io/part-of=sproutfs --tail=-1 --prefix 2>/dev/null |
        grep -E 'a handover outlived its deadline|released a handover nothing was waiting on|gave up a handover nothing will ever receive' | tail -5 || true

    await_hosts 2 || fail 'the deleted host pod never came back'
    printf 'both hosts are ready again\n'
}

fix_recovery() {
    # --- 2. a recovery while the host is alive ----------------------------------
    step 'a recovery is refused while the VM host is alive, and taken once it is gone'
    live_host=$(host_of "$parent")
    [[ -n $live_host ]] || fail "no host reports running $parent"
    refused=''
    if refused=$(ctl recover "$parent" 2>&1); then
        fail "recovering $parent was allowed while $live_host still runs it: $refused"
    fi
    printf 'refused, as it had to be:\n%s\n' "$refused"
    [[ $refused == *"$live_host"* ]] ||
        fail "the refusal did not name the host that runs $parent: $refused"

    # An explicit checkpoint first: what the kill costs is bounded by the interval,
    # and the run should not be waiting one out.
    ctl capture "$parent" > /dev/null
    ctl kill-host "$live_host"
    deadline=$(($(date +%s) + boot_timeout))
    while [[ -n $(host_of "$parent") ]]; do
        (($(date +%s) < deadline)) || fail "a host still reports running $parent after the kill"
        sleep 2
    done
    printf '%s is gone\n' "$live_host"

    deadline=$(($(date +%s) + boot_timeout))
    while :; do
        if recovered=$(ctl recover "$parent" 2>&1); then break; fi
        (($(date +%s) < deadline)) || fail "recovering $parent after the kill failed: $recovered"
        sleep 2
    done
    printf 'recovered once the pod was gone:\n%s\n' "$recovered"
    agent_ready "$parent" || fail "the recovered $parent never answered"
    printf 'the recovered guest answers\n'
    await_hosts 2 || fail 'the killed host pod never came back'
}

fix_vmm_death() {
    # --- 3. a VMM killed underneath its host ------------------------------------
    step 'a killed VMM leaves a diagnostic and the host forgets the VM'
    victim_host=$(host_of "$parent")
    [[ -n $victim_host ]] || fail "no host reports running $parent"
    # The host logs the death; reading from here on means the run's own earlier
    # lines are not what the grep finds.
    since=$(date -u +%FT%TZ)
    # The host image is debian-slim with no procps in it, so the VMM is found by
    # reading /proc rather than with pgrep.
    # shellcheck disable=SC2016  # p is the remote shell's loop variable.
    pid=$(kubectl exec -n "$namespace" "$victim_host" -- sh -c '
        for p in /proc/[0-9]*; do
            [ -r "$p/cmdline" ] || continue
            if tr "\0" " " < "$p/cmdline" | grep -q firecracker; then
                basename "$p"
                break
            fi
        done' 2>/dev/null | tr -d '\r')
    [[ -n $pid ]] || fail "no firecracker process in $victim_host"
    printf 'killing firecracker pid %s in %s\n' "$pid" "$victim_host"
    # kill through a shell: /bin/kill is procps, which this image does not carry.
    kubectl exec -n "$namespace" "$victim_host" -- sh -c "kill -9 $pid"

    # The diagnostic is the point: one record naming the VM, the cause and the
    # console, rather than a connection-refused checkpoint failure a minute later.
    deadline=$(($(date +%s) + boot_timeout))
    death=
    while (($(date +%s) < deadline)); do
        death=$(kubectl logs -n "$namespace" "$victim_host" --since-time="$since" --tail=-1 2>/dev/null |
            grep 'vmmachine: the VMM exited' || true)
        [[ -n $death ]] && break
        sleep 2
    done
    [[ -n $death ]] || {
        kubectl logs -n "$namespace" "$victim_host" --since-time="$since" --tail=40 >&2
        fail "$victim_host logged no VMM death diagnostic"
    }
    printf 'the host logged the death:\n%s\n' "$(printf '%s\n' "$death" | head -2)"
    printf '%s\n' "$death" | grep -q '"vm":' || fail 'the diagnostic does not name the VM'

    # And the VM is forgotten: the host stops reporting it, rather than going on
    # supervising a guest that no longer exists.
    deadline=$(($(date +%s) + boot_timeout))
    while [[ -n $(host_of "$parent") ]]; do
        (($(date +%s) < deadline)) || {
            ctl list >&2
            fail "$victim_host still reports running $parent after its VMM was killed"
        }
        sleep 2
    done
    printf '%s is forgotten: no host reports running it\n' "$parent"
    gave_up=$(kubectl logs -n "$namespace" "$victim_host" --since-time="$since" --tail=-1 2>/dev/null |
        grep 'gave up a VM whose VMM process ended' || true)
    [[ -n $gave_up ]] && printf 'and said so:\n%s\n' "$(printf '%s\n' "$gave_up" | head -1)"

    ctl delete "$parent" > /dev/null 2>&1 || true
}

fix_rollout() {
    # --- 4. a rollout drains rather than loses its hosts ------------------------
    step 'a rollout migrates every VM instead of losing it'
    rolled=$(ctl create --template alpine)
    witness_vm=${rolled%% *}
    [[ $witness_vm == vm-* ]] || fail "create did not name a VM: $rolled"
    agent_ready "$witness_vm" || fail "the agent in $witness_vm never answered"
    before_host=$(host_of "$witness_vm")
    [[ -n $before_host ]] || fail "no host reports running $witness_vm"

    # The witness is written after the VM's last checkpoint, so only the guest's
    # own memory carries it: a VM that was migrated still has it, and one that was
    # lost with its host and reopened from that checkpoint does not.
    ctl capture "$witness_vm" > /dev/null
    witness="sproutfs-rollout-$RANDOM"
    run_in "$witness_vm" "echo $witness > /tmp/witness" > /dev/null ||
        fail "writing the witness into $witness_vm failed"

    printf 'restarting the host Deployment with %s on %s\n' "$witness_vm" "$before_host"
    kubectl rollout restart -n "$namespace" deployment/sproutfs-host > /dev/null
    kubectl rollout status -n "$namespace" deployment/sproutfs-host --timeout=10m ||
        fail 'the host Deployment never finished rolling'
    await_hosts 2 || fail 'both hosts did not come back after the rollout'

    deadline=$(($(date +%s) + boot_timeout))
    while [[ -z $(host_of "$witness_vm") ]]; do
        (($(date +%s) < deadline)) || { ctl list >&2; fail "no host runs $witness_vm after the rollout"; }
        sleep 2
    done
    after_host=$(host_of "$witness_vm")
    printf '%s now runs on %s\n' "$witness_vm" "$after_host"
    agent_ready "$witness_vm" || fail "$witness_vm stopped answering after the rollout"
    held=$(run_in "$witness_vm" 'cat /tmp/witness' | tr -d '\r')
    [[ $held == *"$witness"* ]] ||
        fail "the rollout lost the guest's memory: /tmp/witness reads '$held', want $witness"
    printf 'the guest still holds what it wrote after its last checkpoint: %s\n' "$witness"

    # And the pods that came back create VMs. The pods carry names of their own
    # — the hosts are a Deployment — and a template is named by its guest
    # image's bytes, so what they found was the template their predecessors
    # imported, already published: they opened nothing, imported nothing and
    # were ready as fast as they could read one control record. A host that had
    # to import again would have sat out the wait above, and one that found an
    # identity it could not use would never have become ready at all.
    created=$(ctl create --template alpine)
    created_vm=${created%% *}
    [[ $created_vm == vm-* ]] ||
        fail "a host that came back could not create a VM from the deployment's template: $created"
    printf 'a host that came back created %s from the template already in the deployment\n' "$created_vm"
    ctl delete "$created_vm" > /dev/null 2>&1 || true

    ctl delete "$witness_vm" > /dev/null 2>&1 || true
}

fix_fanout_rollback() {
    # --- 5. a same-host fan-out that fails part way ------------------------------
    step 'a same-host fan-out that fails part way takes back the children it started'
    # The PMEM pager's logical cap is its arena times 32: 1.25 GiB of arena, so
    # 40 GiB of roots. A 16 GiB root leaves room for the parent's and one
    # child's, and not a second child's. The orchestrator admits a fan-out
    # against RAM alone, three children of 512 MiB, so the fork is accepted and
    # the host refuses the second child when it receives it.
    created=$(ctl create --template alpine --disk 16G)
    parent=${created%% *}
    [[ $parent == vm-* ]] || fail "create did not name a VM: $created"
    agent_ready "$parent" || fail "the agent in $parent never answered"
    parent_host=$(host_of "$parent")
    [[ -n $parent_host ]] || fail "no host reports running $parent"
    listed_before=$(ctl list | awk 'NR > 1' | wc -l)
    running_before=$(running_on "$parent_host")
    since=$(date -u +%FT%TZ)
    printf '%s runs on %s with %s VMs; forking it three times there\n' \
        "$parent" "$parent_host" "$running_before"
    if forked=$(ctl fork "$parent" --count 3 2>&1); then
        fail "the fan-out fitted, so nothing was taken back: $forked"
    fi
    printf 'the fork failed:\n%s\n' "$forked"
    [[ $forked == *'logical pages'* ]] ||
        fail "the fork did not fail on the host's logical cap: $forked"

    # A child started before the refusal is the case this is about. The host
    # logs the end of every child's receive, so the children it started are the
    # forks it finished receiving since the fork began.
    started=$(kubectl logs -n "$namespace" "$parent_host" --since-time="$since" --tail=-1 2>/dev/null |
        grep '"host: post-copy complete"' | grep '"fork":true' | grep -o '"vm":"[^"]*"' | cut -d'"' -f4 || true)
    [[ -n $started ]] || fail "no child started before the refusal, so there was nothing to take back"
    printf 'children started before the refusal: %s\n' "$(tr '\n' ' ' <<< "$started")"

    deadline=$(($(date +%s) + boot_timeout))
    while :; do
        left=''
        for child in $started; do
            if ctl list | awk -v vm="$child" '$1 == vm { found = 1 } END { exit !found }'; then
                left+="$child "
            fi
        done
        [[ -z $left && $(running_on "$parent_host") == "$running_before" &&
            $(serving_of "$parent_host") == 0 ]] && break
        (($(date +%s) < deadline)) || {
            ctl list >&2
            ctl hosts >&2
            fail "the partial fan-out left children behind: listed ${left:-none}, $(running_on "$parent_host") running"
        }
        sleep 2
    done
    listed_after=$(ctl list | awk 'NR > 1' | wc -l)
    ((listed_after == listed_before)) || fail "$listed_before VMs were listed before the fork and $listed_after after"
    printf 'every child it started is gone, and %s runs %s VMs and serves no hold\n' \
        "$parent_host" "$running_before"

    # The parent took its sealed pages back: it answers and checkpoints again.
    agent_ready "$parent" || fail "the parent $parent stopped answering after the fan-out failed"
    before=$(checkpoint_of "$parent")
    ctl capture "$parent" || fail "the parent $parent could not be checkpointed after the fan-out failed"
    after=$(checkpoint_of "$parent")
    [[ $after != "$before" ]] || fail "$parent is still at checkpoint $before after a capture"
    printf 'the parent checkpointed again: %s then %s\n' "$before" "$after"
    kubectl logs -n "$namespace" -l app.kubernetes.io/name=sproutfs-orchestrator --since-time="$since" \
        --tail=-1 2>/dev/null | grep -E 'gave up a handover|partial fork' | tail -5 || true
    ctl delete "$parent" > /dev/null
}

# fill_memory writes MiB of random bytes into a tmpfs in the guest and prints
# their md5. Nothing publishes them until the next checkpoint, so a migration
# right after streams them out of the source's own pages.
fill_memory() {
    local vm=$1 mib=$2
    ctl exec "$vm" --timeout "$fill_timeout" -- \
        "mkdir -p /fill && { grep -qs ' /fill ' /proc/mounts || mount -t tmpfs -o size=$((mib + 64))m tmpfs /fill; } &&
         dd if=/dev/urandom of=/fill/bytes bs=1M count=$mib 2>/dev/null && md5sum /fill/bytes" < /dev/null |
        awk '{ print $1 }'
}
md5_of() {
    ctl exec "$1" --timeout "$fill_timeout" -- 'md5sum /fill/bytes' < /dev/null | awk '{ print $1 }'
}

# await_receiving waits until a destination has started the migrating VM, which
# is when its receive is in flight and the post-copy is streaming.
await_receiving() {
    local destination=$1 before=$2 deadline=$(($(date +%s) + 60))
    while (($(date +%s) < deadline)); do
        (($(running_on "$destination") > before)) && return 0
        sleep 0.2
    done
    return 1
}

# migrate_losing migrates a VM and kills one host once the destination is
# receiving it, and prints what the migration returned.
migrate_losing() {
    local vm=$1 destination=$2 victim=$3 before migrating
    before=$(running_on "$destination")
    ( ctl migrate "$vm" --to "$destination" > /tmp/sproutfs-fix-migrate.out 2>&1
      printf '%s\n' "$?" > /tmp/sproutfs-fix-migrate.rc ) &
    migrating=$!
    if ! await_receiving "$destination" "$before"; then
        wait "$migrating" || true
        cat /tmp/sproutfs-fix-migrate.out >&2
        fail "$destination never started receiving $vm"
    fi
    ctl kill-host "$victim"
    printf 'killed %s while %s received %s\n' "$victim" "$destination" "$vm"
    wait "$migrating" || true
    printf 'the migration returned %s:\n%s\n' \
        "$(cat /tmp/sproutfs-fix-migrate.rc)" "$(cat /tmp/sproutfs-fix-migrate.out)"
}

fix_lost_migration() {
    # --- 6. a host lost during a real migration ----------------------------------
    # A guest whose memory holds 2.5 GiB that no checkpoint has, so the post-copy
    # takes long enough to lose a host in the middle of it.
    local fill_mib=2560
    step 'the destination of a migration is lost mid-stream, and the receive is retried'
    created=$(ctl create --template alpine --memory 3G)
    moving=${created%% *}
    [[ $moving == vm-* ]] || fail "create did not name a VM: $created"
    agent_ready "$moving" || fail "the agent in $moving never answered"
    source=$(host_of "$moving")
    destination=$(other_host "$source")
    [[ -n $destination ]] || fail "no other host to migrate $moving to"
    ctl capture "$moving" > /dev/null
    filled=$(fill_memory "$moving" "$fill_mib") || fail "filling $moving failed"
    [[ -n $filled ]] || fail "filling $moving printed no md5"
    printf '%s on %s holds %s MiB no checkpoint has, md5 %s\n' "$moving" "$source" "$fill_mib" "$filled"
    since=$(date -u +%FT%TZ)
    migrate_losing "$moving" "$destination" "$destination"
    await_hosts 2 || fail 'the killed destination never came back'
    deadline=$(($(date +%s) + 300))
    while [[ -z $(host_of "$moving") || $(host_of "$moving") == "$source" ]]; do
        (($(date +%s) < deadline)) || { ctl list >&2; fail "$moving never landed off $source after its destination died"; }
        sleep 2
    done
    landed=$(host_of "$moving")
    agent_ready "$moving" || fail "$moving stopped answering after the retried receive"
    [[ $(md5_of "$moving") == "$filled" ]] ||
        fail "$moving lost the bytes no checkpoint had: the retried receive rewound it"
    printf '%s runs on %s and still holds every byte it wrote after its checkpoint\n' "$moving" "$landed"
    kubectl logs -n "$namespace" -l app.kubernetes.io/name=sproutfs-orchestrator --since-time="$since" \
        --tail=-1 2>/dev/null | grep -E 'receive failed|tried again|migrated a VM' | tail -5 || true

    step 'the source of a migration is lost mid-stream, and every VM ends running or reopenable'
    source=$landed
    # A bystander on the source, which the loss leaves to an ordinary recovery.
    created=$(ctl create --template alpine)
    bystander=${created%% *}
    [[ $bystander == vm-* ]] || fail "create did not name a VM: $created"
    agent_ready "$bystander" || fail "the agent in $bystander never answered"
    [[ $(host_of "$bystander") == "$source" ]] || ctl migrate "$bystander" --to "$source" > /dev/null
    [[ $(host_of "$bystander") == "$source" ]] || fail "$bystander is not on $source"
    destination=$(other_host "$source")
    [[ -n $destination ]] || fail "no other host to migrate $moving to"
    # A recovery opens the checkpoint the record selects: the capture, which
    # resumes the guest, or an interval checkpoint after it, which holds the
    # disks alone and boots the guest cold over them. So the marker is on the
    # root, where either keeps it.
    run_in "$moving" 'echo before-the-checkpoint > /marker && sync' > /dev/null
    ctl capture "$moving" > /dev/null
    ctl capture "$bystander" > /dev/null
    [[ -n $(fill_memory "$moving" "$fill_mib") ]] || fail "filling $moving failed"
    since=$(date -u +%FT%TZ)
    migrate_losing "$moving" "$destination" "$source"
    for vm in "$moving" "$bystander"; do
        deadline=$(($(date +%s) + 300))
        while [[ -z $(host_of "$vm") ]]; do
            if recovered=$(ctl recover "$vm" 2>&1); then
                printf '%s\n' "$recovered"
                break
            fi
            (($(date +%s) < deadline)) || { ctl list >&2; fail "$vm is neither running nor reopenable: $recovered"; }
            sleep 2
        done
        agent_ready "$vm" || fail "$vm does not answer after losing $source"
        printf '%s runs on %s\n' "$vm" "$(host_of "$vm")"
    done
    marker=$(run_in "$moving" 'cat /marker' | tr -d '\r')
    [[ $marker == before-the-checkpoint ]] || fail "$moving came back without its checkpoint's marker: $marker"
    printf '%s came back at its checkpoint\n' "$moving"
    kubectl logs -n "$namespace" -l app.kubernetes.io/name=sproutfs-orchestrator --since-time="$since" \
        --tail=-1 2>/dev/null | grep -E 'no longer has them|lost its source|recovered' | tail -5 || true
    await_hosts 2 || fail 'the killed source never came back'
    ctl delete "$moving" > /dev/null
    ctl delete "$bystander" > /dev/null
}

# --- which of them to run ----------------------------------------------------
# With no argument, all four in order. The first three are one chain — they are
# about the VM the first of them creates — so naming a later one without it is
# refused rather than run against nothing.

fixes=("$@")
((${#fixes[@]} > 0)) || fixes=(fork-destination recovery vmm-death rollout fanout-rollback lost-migration)
for fix in "${fixes[@]}"; do
    case $fix in
        fork-destination | recovery | vmm-death | rollout | fanout-rollback | lost-migration) ;;
        *) fail "no fix named $fix: they are fork-destination, recovery, vmm-death, rollout, fanout-rollback and lost-migration" ;;
    esac
done
case " ${fixes[*]} " in
    *" recovery "* | *" vmm-death "*)
        [[ " ${fixes[*]} " == *" fork-destination "* ]] ||
            fail 'recovery and vmm-death act on the VM fork-destination leaves running, so they need it too'
        ;;
esac

printf 'sproutfs demo: the blocker fixes a live cluster shows\n'
ctl hosts
for fix in "${fixes[@]}"; do
    case $fix in
        fork-destination) fix_fork_destination ;;
        recovery) fix_recovery ;;
        vmm-death) fix_vmm_death ;;
        rollout) fix_rollout ;;
        fanout-rollback) fix_fanout_rollback ;;
        lost-migration) fix_lost_migration ;;
    esac
done

ctl hosts
if ((${#fixes[@]} == 1)); then
    printf '\nThe one live-cluster fix holds.\n'
else
    printf '\nAll %s live-cluster fixes hold.\n' "${#fixes[@]}"
fi
