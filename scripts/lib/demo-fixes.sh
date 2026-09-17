#!/usr/bin/env bash
# The blocker fixes that only a live cluster can show, on the demo node.
#
# scripts/demo-gce.sh copies this one file to the node and runs it there:
#
#     bash demo-fixes.sh
#
# With no argument it runs all four; naming one or more of fork-destination,
# recovery, vmm-death and rollout runs those. The rollout is the one a model of
# the deployment can answer for, and scripts/test/fixes-test.sh runs it that way.
#
# demo-run.sh proves the five flows work. This proves four things that are
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
# serving_of is how many handovers one host still holds frames for: the VMs it
# migrated away and the children of every fork instant it took, wherever those
# children landed. A parent whose fork hold was never released never leaves 1.
serving_of() { ctl hosts | awk -v host="$1" '$1 == host { print $4 }'; }
# checkpoint_of is the sequence the VM's latest checkpoint carries. A sealed
# parent cannot checkpoint, so this is what stops moving when a hold leaks. The
# sequence is the last column because a migrating VM's state carries both of its
# hosts in the one before it.
checkpoint_of() { ctl list | awk -v vm="$1" '$1 == vm { print $NF }'; }
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

# --- which of them to run ----------------------------------------------------
# With no argument, all four in order. The first three are one chain — they are
# about the VM the first of them creates — so naming a later one without it is
# refused rather than run against nothing.

fixes=("$@")
((${#fixes[@]} > 0)) || fixes=(fork-destination recovery vmm-death rollout)
for fix in "${fixes[@]}"; do
    case $fix in
        fork-destination | recovery | vmm-death | rollout) ;;
        *) fail "no fix named $fix: they are fork-destination, recovery, vmm-death and rollout" ;;
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
    esac
done

ctl hosts
if ((${#fixes[@]} == 1)); then
    printf '\nThe one live-cluster fix holds.\n'
else
    printf '\nAll %s live-cluster fixes hold.\n' "${#fixes[@]}"
fi
