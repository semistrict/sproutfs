#!/usr/bin/env bash
# Many forks, migrations, stops and starts under a workload, on the demo node,
# with every guest's memory and disk checked after every one of them.
#
# scripts/demo-gce.sh copies this one file to the node and runs it there:
#
#     bash demo-soak.sh
#
# The demo flows prove each operation once. This takes many forks across the
# same host and the other one, migrates and stops and starts them with work
# happening in between, and after every step asks each guest whether its memory
# and its disk still hold exactly the bytes that guest wrote. The simulated
# campaigns do that with model guests; this is the same discipline over
# Firecracker, KVM, the pager, GCS and k3s.
#
# What asks the guest is sproutfs-guest-witness, which both guest images carry.
# It holds a resident buffer and a file of the same size filled with the pattern
# of (seed, step), and checks every byte of both against a (seed, step) this
# script names. The pattern is a pure function of those two numbers and the
# page, so the expectation is arithmetic this script does for itself: nothing
# about what a VM should hold is carried across a fork, a migration or a stop,
# and a guest cannot agree with itself about the wrong thing.
#
# Each round, over every VM that is running, in an order drawn from the seed:
#
#   1. mutate memory and disk, then check
#   2. fork the fan-outs, on the parent's host and on the other one; check every
#      child against its parent's (seed, step) before anything else, then the
#      parent; then give each child a seed of its own and check it again
#   3. migrate the round's share, checking each before and after
#   4. stop the round's share, check that no host runs them, start them — some
#      on the other host — and check each. A seeded share of them come back
#      cold: their memory is discarded and their kernel booted, so what is asked
#      of them is their disk alone, and a share of those are given a larger
#      memory and disk, the filesystem grown in the guest with `witness grow`
#   5. delete a seeded share, so the population stays within what two hosts
#      admit
#
# Once per soak, at a round the seed picks, every VM is captured, one host pod
# is killed without grace, its VMs are recovered on the other host, and each is
# checked against what its last checkpoint published. That happens at the top of
# the round, before anything in it has mutated a guest: every host checkpoints on
# its own interval, so a kill taken after a mutation comes back at a state this
# script cannot name, and a guest checked against a state it was never in is a
# failure that says nothing.
#
# At the end every VM is deleted and `sproutfsctl check` runs over the bucket,
# when nothing but the deployment's templates — one per guest image, named by
# its bytes — and the lineages the deleted VMs pinned may remain.
#
# What it records, under $SPROUTFS_DEMO_RUN_DIR:
#   operations.tsv  every operation, the VMs it touched and what it cost
#   checks.tsv      every check: the VM, the (seed, step) and what it said
#   refusals.tsv    every placement the deployment refused, which is not a failure
#   store.tsv       each host's object-store counters at every round boundary
#   rounds.tsv      the wall-clock window each round occupied
#   summary.txt     the tables this prints
#   host.log        the hosts' own logs over the run
#
# Overridable: SPROUTFS_DEMO_NAMESPACE, SPROUTFS_DEMO_SOAK_TEMPLATE,
# SOAK_SEED, SOAK_ROUNDS, SOAK_FORKS_LOCAL, SOAK_FORKS_REMOTE, SOAK_MIGRATIONS,
# SOAK_STOPS, SOAK_COLD_STARTS, SOAK_COLD_RESIZES, SOAK_COLD_MEMORY,
# SOAK_COLD_DISK, SOAK_WITNESS_BYTES, SOAK_MAX_VMS, SOAK_START_VMS,
# SPROUTFS_DEMO_RUN_DIR, SPROUTFS_DEMO_EXEC_TIMEOUT.
#
# Non-zero exit on any check that failed, on any guest that stopped answering,
# and on a deployment that disagrees with itself at the end.
set -euo pipefail

# The per-VM expectation is an associative array, which is bash 4 and up. The
# demo node is Ubuntu and has bash 5; saying so beats failing on the declare.
((BASH_VERSINFO[0] >= 4)) || { echo "This needs bash 4 or newer; this is $BASH_VERSION." >&2; exit 2; }

export KUBECONFIG=${KUBECONFIG:-/etc/rancher/k3s/k3s.yaml}
namespace=${SPROUTFS_DEMO_NAMESPACE:-sproutfs}
template=${SPROUTFS_DEMO_SOAK_TEMPLATE:-alpine}
# The seed decides the order VMs are walked in, which VMs each round's shares
# fall on, which round kills a host and every witness seed. It is printed and
# recorded: one number reproduces a run's shape.
seed=${SOAK_SEED:-1}
rounds=${SOAK_ROUNDS:-6}
forks_local=${SOAK_FORKS_LOCAL:-2}
forks_remote=${SOAK_FORKS_REMOTE:-2}
migrations=${SOAK_MIGRATIONS:-2}
stops=${SOAK_STOPS:-2}
# How many of the restarted VMs come back cold, as one in this many, and how
# many of those are given a larger memory and disk while they are at it. A cold
# boot is the one moment a VM's shape can change, because nothing in memory
# describes it any more. Zero draws none and one draws every time.
cold_starts=${SOAK_COLD_STARTS:-2}
cold_resizes=${SOAK_COLD_RESIZES:-2}
# What a resized VM comes back as. The template gives a guest 512 MiB of RAM and
# a 2 GiB root, so both of these are a step up from that, and the witness fits
# in either.
cold_memory=${SOAK_COLD_MEMORY:-768M}
cold_disk=${SOAK_COLD_DISK:-3G}
# The witness's memory buffer and its file are each this size, inside a guest
# whose template gives it 512 MiB of RAM and a 2 GiB root.
witness_bytes=${SOAK_WITNESS_BYTES:-256M}
# How many VMs the population is allowed to reach before a round deletes its way
# back, and how many it starts with. Two 5 GiB arenas hold far more than this;
# what bounds it is the time a round takes, which is linear in the population.
max_vms=${SOAK_MAX_VMS:-6}
start_vms=${SOAK_START_VMS:-2}
min_vms=2

run_dir=${SPROUTFS_DEMO_RUN_DIR:-/tmp/sproutfs-soak}
# A witness fill writes its whole size to memory and through the guest's
# filesystem, and a check reads all of it back; the agent's own ceiling is ten
# minutes.
exec_timeout=${SPROUTFS_DEMO_EXEC_TIMEOUT:-300s}
agent_timeout=240
boot_timeout=240
recover_timeout=240

witness=/usr/local/bin/sproutfs-guest-witness
witness_file=/var/sproutfs-witness
# The mount point of the guest's root filesystem, which is what `witness grow`
# is given. A cold start that grew the root volume leaves the filesystem the
# size it was, and the grow is what takes the rest: an ioctl on this mount
# point, which needs no write open of the device the filesystem is on — and a
# 6.18 kernel allows no such open of a device something has mounted, which is
# why resize2fs cannot do it here at all.
witness_mount=${SOAK_WITNESS_MOUNT:-/}

run_began=$(date -u +%FT%TZ)

rm -rf -- "$run_dir"
mkdir -p "$run_dir"
: > "$run_dir/operations.tsv"
: > "$run_dir/checks.tsv"
: > "$run_dir/refusals.tsv"
: > "$run_dir/store.tsv"
: > "$run_dir/rounds.tsv"

# ctl takes its standard input from nowhere, which every call below wants and
# most of them need. `kubectl exec -i` forwards this process's standard input to
# the far side and reads it whether the command there wants it or not, and the
# loops below read a list of VMs on standard input and call ctl once per line:
# without this the first ctl of the first iteration reads the rest of the list,
# every one of those loops ends after one line, and a round that mutated one
# guest and migrated one VM reports having checked them all.
ctl() { kubectl exec -n "$namespace" -i deploy/sproutfs-orchestrator -- sproutfsctl "$@" < /dev/null; }
now() { date -u +%s.%N; }
step() { printf '\n=== %s ===\n' "$*"; }
fail() { printf '\nFAIL: %s\n' "$*" >&2; exit 1; }
since() { awk -v a="$1" -v b="$2" 'BEGIN { printf "%.3f", b - a }'; }

run_in() { ctl exec "$1" --timeout "$exec_timeout" -- "${@:2}"; }

host_of() { ctl list | awk -v vm="$1" '$1 == vm && $2 != "-" { print $2 }'; }
running_vms() { ctl list | awk 'NR > 1 && $2 != "-" { print $1 }'; }
ready_hosts() { ctl hosts | awk 'NR > 1 && $2 == "true" { print $1 }'; }
other_host() { ctl hosts | awk -v host="$1" 'NR > 1 && $1 != host && $2 == "true" { print $1; exit }'; }

# --- the seeded draws -------------------------------------------------------
# What a run does — the order VMs are walked in, which of them each round's
# shares fall on, which round loses a host — comes from the one number this
# prints and from nothing else, so that a run which found something can be run
# again.
#
# Not from $RANDOM. Every draw below is made inside a subshell: a $(...), or the
# < <(...) a loop reads its list from. Bash re-seeds $RANDOM in a subshell, so a
# shape drawn from it differs between two runs of the same seed, and the seed
# names nothing. Each draw is numbered here instead, in the one shell, and is
# arithmetic on the seed and that number: the number is what crosses into the
# subshell, and the same number draws the same values every time.
draws=0

# next_draw numbers the draw that is about to be made. It is a statement of its
# own because the draw itself happens in a subshell, and nothing a subshell
# assigns comes back to here.
next_draw() { draws=$((draws + 1)); }

# mix is one 32-bit avalanche: every bit of the answer depends on every bit of
# what it was given, so two draws a number apart share nothing. It leaves its
# answer in `mixed` rather than printing it, because printing would mean a
# command substitution, which is the very thing this avoids. Every step is
# masked to 32 bits, so the numbers are the same on every machine rather than
# wherever a shell's own integers happen to overflow.
mixed=0
mix() {
    local x=$(($1 & 0xffffffff))
    x=$((((x ^ (x >> 16)) * 0x45d9f3b) & 0xffffffff))
    x=$((((x ^ (x >> 16)) * 0x45d9f3b) & 0xffffffff))
    mixed=$((x ^ (x >> 16)))
}

# stream folds the run's seed and the current draw's number into the value the
# draw is made from.
stream() {
    mix "$seed"
    mix $((mixed ^ draws))
}

# shuffle prints its arguments in the order this draw names: Fisher-Yates, so
# what a run does comes from the seed and not from the order `ctl list` happened
# to print.
shuffle() {
    (($# > 0)) || return 0
    local -a items=("$@")
    local index swap keep key
    stream
    key=$mixed
    for ((index = ${#items[@]} - 1; index > 0; index--)); do
        mix $((key ^ index))
        swap=$((mixed % (index + 1)))
        keep=${items[index]}
        items[index]=${items[swap]}
        items[swap]=$keep
    done
    printf '%s\n' "${items[@]}"
}

# one_in reports true for one draw in N, which is how a share of the VMs a round
# restarts is picked out. The draw is arithmetic on the seed and this draw's
# number, made here rather than in a subshell, so it costs no command
# substitution and the number it lands on is the seed's. N of zero draws nothing
# and N of one draws every time.
one_in() {
    local n=$1
    ((n > 0)) || return 1
    next_draw
    stream
    ((mixed % n == 0))
}

# sample prints up to N of its arguments, drawn in a seeded order.
sample() {
    local count=$1
    shift
    (($# > 0 && count > 0)) || return 0
    shuffle "$@" | head -n "$count"
}

# --- what each VM holds -----------------------------------------------------
# The witness's (seed, step) per VM, which is the whole of the expectation, and
# the step each VM's last explicit capture published, which is what a VM
# recovered from a host loss must come back at.
declare -A witness_seed=()
declare -A witness_step=()
declare -A captured_step=()
next_seed=$((seed * 1000 + 1))

record_operation() {
    printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$@" >> "$run_dir/operations.tsv"
}

record_check() {
    printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$@" >> "$run_dir/checks.tsv"
}

# record_refusal keeps one refusal as one row. The reason is whatever the far
# side said, and a deployment that turned a placement down names the hosts it
# asked over several lines; a row that ran over more than one of them, or that
# carried a tab of its own, would be a refusal the summary below reads as
# something else or drops.
record_refusal() {
    local reason
    reason=$(printf '%s' "$4" | tr '\n\t' '  ' | tr -s ' ')
    printf '%s\t%s\t%s\t%s\n' "$1" "$2" "$3" "$reason" >> "$run_dir/refusals.tsv"
    printf 'refused (%s %s): %s\n' "$2" "$3" "$reason"
}

# sample_store records what every host's object store has served at one
# boundary. Two samples subtracted are one round's cost. These are the same
# counters the hosts' /metrics exposition carries; the CLI is what already
# carries the deployment's token, and neither pod image has an HTTP client in it.
sample_store() {
    local mark=$1 stamp
    stamp=$(now)
    ctl store | awk -v mark="$mark" -v at="$stamp" 'NR > 1 {
        printf "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", mark, at, $1, $2, $3, $4, $5 }' \
        >> "$run_dir/store.tsv"
}

# --- the guest ---------------------------------------------------------------
agent_ready() {
    local vm=$1 deadline=$(($(date +%s) + agent_timeout)) last=
    while (($(date +%s) < deadline)); do
        if last=$(run_in "$vm" true 2>&1); then return 0; fi
    done
    printf '%s\n' "$last" >&2
    return 1
}

# witness_fill makes a guest's memory and its disk the pattern of (seed, step 0)
# and leaves the witness resident. It is what a VM this soak created runs, and
# what a fork's child runs once it has been checked against its parent: from
# there the child diverges under a seed of its own, which is what makes two
# children of one parent tell each other apart.
witness_fill() {
    local vm=$1 with=$2 began ended
    began=$(now)
    run_in "$vm" "$witness" fill --memory "$witness_bytes" --disk "$witness_file" \
        --seed "$with" > /dev/null || fail "filling the witness in $vm failed"
    ended=$(now)
    witness_seed[$vm]=$with
    witness_step[$vm]=0
    record_operation "$round" fill "$vm" "$(host_of "$vm")" "$with" 0 "$(since "$began" "$ended")"
}

# witness_mutate rewrites the pages the next step takes, in memory and in the
# file, and moves this VM's expectation on by one.
witness_mutate() {
    local vm=$1 began ended next
    next=$(( ${witness_step[$vm]} + 1 ))
    began=$(now)
    run_in "$vm" "$witness" mutate --step "$next" > /dev/null ||
        fail "mutating the witness in $vm to step $next failed"
    ended=$(now)
    witness_step[$vm]=$next
    record_operation "$round" mutate "$vm" "$(host_of "$vm")" "${witness_seed[$vm]}" "$next" \
        "$(since "$began" "$ended")"
}

# witness_check requires every byte of one guest's memory and of its file to be
# what a (seed, step) says. The two numbers are this script's, not the guest's:
# a guest that is whole can still be the wrong guest, and a check against the
# seed this script believes that VM carries is what catches a child that came
# back holding a sibling's bytes.
#
# A failure is the end of the run. It is the one thing this whole flow exists to
# find, and going on from it would only bury it.
#
# A guest that did not answer at all is the other outcome, and it is recorded as
# itself. The agent kills a command that runs past the timeout and reports 124,
# which is a check that wanted longer than $exec_timeout — a witness of this
# size reading its file back over pages a remote fork is still pulling is the
# slow case — and not a guest whose memory came back holding the bytes of another
# bytes. Both end the run. Calling the first the second would have an operator
# looking for a defect in the pager over a check that needed more time.
# A fifth argument of "disk" asks the disk alone, with no witness resident and
# no memory to check. That is what a cold-started guest can answer: the memory
# and the process that held it were discarded, and the file on its disk is
# exactly what the last checkpoint published.
witness_check() {
    local vm=$1 with=$2 at=$3 what=$4 only=${5:-} began ended outcome
    local -a request=("$witness" check --seed "$with" --step "$at")
    [[ $only == disk ]] && request+=(--disk-only --disk "$witness_file")
    began=$(now)
    if outcome=$(run_in "$vm" "${request[@]}" 2>&1); then
        ended=$(now)
        record_check "$round" "$vm" "$what" "$with" "$at" ok "$(since "$began" "$ended")"
        printf '  check %-18s %-22s seed %s step %s: ok\n' "$what" "$vm" "$with" "$at"
        return 0
    fi
    ended=$(now)
    if [[ $outcome == *"exited 124"* || $outcome == *"ran past its timeout"* ]]; then
        record_check "$round" "$vm" "$what" "$with" "$at" timeout "$(since "$began" "$ended")"
        printf '%s\n' "$outcome" >&2
        fail "$vm did not answer a check within $exec_timeout after $what"
    fi
    record_check "$round" "$vm" "$what" "$with" "$at" failed "$(since "$began" "$ended")"
    printf '%s\n' "$outcome" >&2
    fail "$vm does not hold (seed $with, step $at) after $what"
}

# check_at is witness_check against what this script believes that VM holds.
check_at() { witness_check "$1" "${witness_seed[$1]}" "${witness_step[$1]}" "$2"; }

# disk_check_at is check_at over the disk alone, which is the whole of what a
# cold-started guest has left to be asked.
disk_check_at() { witness_check "$1" "${witness_seed[$1]}" "${witness_step[$1]}" "$2" disk; }

# --- the operations ---------------------------------------------------------

# create_vm makes one VM, waits for its guest and fills its witness.
create_vm() {
    local created vm began ended
    began=$(now)
    if ! created=$(ctl create --template "$template" 2>&1); then
        record_refusal "$round" create - "$created"
        return 1
    fi
    ended=$(now)
    vm=${created%% *}
    [[ $vm == vm-* ]] || fail "create did not name a VM: $created"
    local deadline=$(($(date +%s) + boot_timeout))
    until agent_ready "$vm"; do
        (($(date +%s) < deadline)) || fail "the agent in $vm never answered"
    done
    record_operation "$round" create "$vm" "$(host_of "$vm")" - - "$(since "$began" "$ended")"
    printf 'created %s on %s\n' "$vm" "$(host_of "$vm")"
    witness_fill "$vm" "$((next_seed++))"
    check_at "$vm" filled
}

# fork_onto forks one parent count times onto a host, checks every child against
# the parent's own (seed, step) before anything else — which is what says the
# inheritance was whole — and then gives each child a seed of its own.
#
# A child's root checkpoint is taken as soon as it answers: a fork's parent keeps
# its pages sealed until the last child has published one, and a sealed parent
# cannot be checkpointed, forked again, migrated or stopped.
fork_onto() {
    local parent=$1 count=$2 to=$3 where=$4 table began ended pause start
    local -a children=()
    ((count > 0)) || return 0
    began=$(now)
    if [[ $to == "-" ]]; then
        table=$(ctl fork "$parent" --count "$count" 2>&1) || {
            record_refusal "$round" fork "$parent" "$table"
            return 0
        }
    else
        table=$(ctl fork "$parent" --count "$count" --to "$to" 2>&1) || {
            record_refusal "$round" fork "$parent" "$table"
            return 0
        }
    fi
    ended=$(now)
    mapfile -t children < <(printf '%s\n' "$table" | awk 'NR > 1 { print $1 }')
    ((${#children[@]} == count)) ||
        fail "asked for $count forks of $parent $where and got ${#children[@]}"
    pause=$(printf '%s\n' "$table" | awk 'NR == 2 { print $3 }')
    start=$(printf '%s\n' "$table" | awk 'NR == 2 { print $4 }')
    local child
    for child in "${children[@]}"; do
        record_operation "$round" "fork-$where" "$child" "$(host_of "$child")" \
            "$pause" "$start" "$(since "$began" "$ended")"
        agent_ready "$child" || fail "the agent in the fork $child never answered"
        ctl capture "$child" > /dev/null || fail "publishing the root checkpoint of $child failed"
        # What the child inherited is exactly what its parent held at the
        # seal, which is the parent's own expectation.
        witness_seed[$child]=${witness_seed[$parent]}
        witness_step[$child]=${witness_step[$parent]}
        check_at "$child" "inherited-$where"
    done
    # And the parent is unchanged by having been forked.
    check_at "$parent" "forked-$where"
    for child in "${children[@]}"; do
        witness_fill "$child" "$((next_seed++))"
        witness_mutate "$child"
        check_at "$child" "diverged-$where"
        ctl capture "$child" > /dev/null || fail "capturing the diverged $child failed"
    done
}

# migrate_vm moves one VM to the other host and checks it at both ends: a
# migration carries the guest whole or it is a defect, and the check before is
# what says the check after is about the migration.
migrate_vm() {
    local vm=$1 from to line began ended
    from=$(host_of "$vm")
    [[ -n $from ]] || return 0
    to=$(other_host "$from")
    [[ -n $to ]] || return 0
    check_at "$vm" before-migrate
    began=$(now)
    if ! line=$(ctl migrate "$vm" --to "$to" 2>&1); then
        record_refusal "$round" migrate "$vm" "$line"
        return 0
    fi
    ended=$(now)
    record_operation "$round" migrate "$vm" "$to" \
        "$(printf '%s\n' "$line" | sed -n 's/.*pause \([0-9.]*\)s.*/\1/p')" \
        "$(printf '%s\n' "$line" | sed -n 's/.*stream \([0-9.]*\)s.*/\1/p')" \
        "$(since "$began" "$ended")"
    printf '%s\n' "$line"
    agent_ready "$vm" || fail "$vm stopped answering after migrating to $to"
    check_at "$vm" after-migrate
}

# stop_and_start stops one VM, requires that no host runs it, starts it — on the
# other host when there is one — and checks it. A stop publishes everything the
# guest held, so what comes back has to be exactly what went away; a stop that
# lost the writes since the last checkpoint would look like a host loss, and
# this is what tells them apart.
stop_and_start() {
    local vm=$1 was line began ended
    was=$(host_of "$vm")
    [[ -n $was ]] || return 0
    check_at "$vm" before-stop
    began=$(now)
    if ! line=$(ctl stop "$vm" 2>&1); then
        record_refusal "$round" stop "$vm" "$line"
        return 0
    fi
    ended=$(now)
    record_operation "$round" stop "$vm" "$was" \
        "$(printf '%s\n' "$line" | sed -n 's/.*checkpoint \([0-9]*\) .*/\1/p')" - \
        "$(since "$began" "$ended")"
    printf '%s\n' "$line"
    [[ -z $(host_of "$vm") ]] || fail "a host still runs $vm after it was stopped"
    # A stopped VM keeps its identity and its objects: it is still listed.
    ctl list | awk -v vm="$vm" '$1 == vm { found = 1 } END { exit !found }' ||
        fail "$vm stopped being listed when it was stopped"

    local to
    to=$(other_host "$was")
    [[ -n $to ]] || to=$was
    # A share of them come back cold, which is its own flow: what a cold start
    # keeps is the disk, so what it can be asked is the disk.
    if one_in "$cold_starts"; then
        cold_start "$vm" "$to"
        return 0
    fi
    began=$(now)
    if ! line=$(ctl start "$vm" --to "$to" 2>&1); then
        record_refusal "$round" start "$vm" "$line"
        return 0
    fi
    ended=$(now)
    record_operation "$round" start "$vm" "$to" \
        "$(printf '%s\n' "$line" | sed -n 's/.*checkpoint \([0-9]*\) .*/\1/p')" - \
        "$(since "$began" "$ended")"
    printf '%s\n' "$line"
    agent_ready "$vm" || fail "$vm never answered after being started on $to"
    check_at "$vm" after-start
}

# cold_start brings one stopped VM back without its memory. The host discards
# every page of it and the VMM state with it in one checkpoint, and boots the
# kernel from the root volume, which is exactly what the last checkpoint
# published: the guest's filesystem sees that as a power cut after that
# checkpoint and its journal recovers what a journal recovers.
#
# So the disk is what can be asked of it, and it is asked with --disk-only,
# which reads the file with no witness resident: the process that held the
# buffer went with the memory, and /run is a tmpfs the boot cleared. The (seed,
# step) is unchanged — a cold start publishes the disk exactly as the stop did —
# and the witness is then filled again under a seed of its own, which puts this
# VM back at step 0 before anything mutates it.
#
# A share of them are grown while they are at it, because a cold boot is the one
# moment a VM's shape can change: a larger memory, a larger root volume, and
# `witness grow` in the guest to take the pages the volume gained, which read as
# zeroes. The disk is checked after that too — a filesystem grown over a witness
# file is a filesystem that must still hold it.
cold_start() {
    local vm=$1 to=$2 line began ended grow=-
    local -a request=(start "$vm" --to "$to" --cold)
    if one_in "$cold_resizes"; then
        grow=$cold_memory/$cold_disk
        request+=(--memory "$cold_memory" --disk "$cold_disk")
    fi
    began=$(now)
    if ! line=$(ctl "${request[@]}" 2>&1); then
        record_refusal "$round" cold-start "$vm" "$line"
        return 0
    fi
    ended=$(now)
    record_operation "$round" cold-start "$vm" "$to" \
        "$(printf '%s\n' "$line" | sed -n 's/.*checkpoint \([0-9]*\) .*/\1/p')" "$grow" \
        "$(since "$began" "$ended")"
    printf '%s\n' "$line"
    agent_ready "$vm" || fail "$vm never answered after being cold started on $to"
    if [[ $grow != - ]]; then
        began=$(now)
        run_in "$vm" "$witness" grow "$witness_mount" > /dev/null ||
            fail "growing the filesystem in $vm after a cold start failed"
        ended=$(now)
        record_operation "$round" resize "$vm" "$to" "$cold_disk" - "$(since "$began" "$ended")"
    fi
    disk_check_at "$vm" after-cold-start
    # The memory is gone, so the expectation starts again: the witness is filled
    # under a seed of its own before anything mutates this VM.
    witness_fill "$vm" "$((next_seed++))"
}

# delete_down deletes a seeded share so that the population stays within what
# two hosts admit and within the time a round may take, which is linear in it.
delete_down() {
    local -a live=()
    mapfile -t live < <(running_vms)
    local over=$(( ${#live[@]} - max_vms ))
    ((over > 0)) || return 0
    ((${#live[@]} - over >= min_vms)) || over=$(( ${#live[@]} - min_vms ))
    ((over > 0)) || return 0
    local vm
    next_draw
    while read -r vm; do
        [[ -n $vm ]] || continue
        ctl delete "$vm" > /dev/null || fail "deleting $vm failed"
        record_operation "$round" delete "$vm" - - - 0
        unset "witness_seed[$vm]" "witness_step[$vm]" "captured_step[$vm]"
        printf 'deleted %s\n' "$vm"
    done < <(sample "$over" "${live[@]}")
}

# delete_every_vm deletes every VM the deployment lists and requires that none
# is left. It is what ends a run, and what begins one: see listed_before.
delete_every_vm() {
    local vm remaining
    while read -r vm; do
        [[ -n $vm ]] || continue
        ctl delete "$vm" > /dev/null || fail "deleting $vm failed"
    done < <(ctl list | awk 'NR > 1 { print $1 }')
    remaining=$(ctl list | awk 'NR > 1 { print $1 }' | wc -l)
    ((remaining == 0)) || { ctl list >&2; fail "$remaining VMs are still listed after deleting them all"; }
}

# lose_a_host kills one host pod without grace, which skips the preStop drain,
# and requires every VM that was on it to come back on the other host holding
# exactly what it held when it went.
#
# It runs straight after the round's capture and before anything in that round
# has mutated a guest, and that ordering is the whole of what makes the check
# mean anything. Every host checkpoints each of its VMs on its own interval, so
# a kill taken after a mutation comes back at either the capture or an interval
# checkpoint that landed since — or, worse, at one that landed in the middle of
# a mutation and published a state no step describes. A guest checked against a
# state it was never in is a failure that says nothing. With nothing changed
# since the capture, every checkpoint the VM might come back at holds the same
# bytes, and the expectation is exact whichever one it was.
lose_a_host() {
    local -a victims=() candidates=()
    local victim survivor vm began ended line
    mapfile -t candidates < <(ready_hosts)
    next_draw
    victim=$(sample 1 "${candidates[@]}")
    [[ -n $victim ]] || return 0
    survivor=$(other_host "$victim")
    [[ -n $survivor ]] || return 0
    mapfile -t victims < <(ctl list | awk -v host="$victim" '$2 == host { print $1 }')
    step "killing $victim, which runs ${#victims[@]} VMs"
    began=$(now)
    ctl kill-host "$victim" || fail "killing $victim failed"
    for vm in "${victims[@]}"; do
        local deadline=$(($(date +%s) + recover_timeout))
        while [[ -n $(host_of "$vm") ]]; do
            (($(date +%s) < deadline)) || fail "a host still reports running $vm after the kill"
        done
        while :; do
            if line=$(ctl recover "$vm" 2>&1); then break; fi
            (($(date +%s) < deadline)) || fail "recovering $vm after the kill failed: $line"
        done
        printf '%s\n' "$line"
        agent_ready "$vm" || fail "the recovered $vm never answered"
        # Everything since its last checkpoint went with the host, so what it
        # holds is the step that checkpoint published and not the one this round
        # mutated it to.
        witness_step[$vm]=${captured_step[$vm]}
        check_at "$vm" recovered
    done
    ended=$(now)
    record_operation "$round" kill-host "${victims[*]:-none}" "$victim" "${#victims[@]}" - \
        "$(since "$began" "$ended")"
    local deadline=$(($(date +%s) + boot_timeout))
    while (($(ready_hosts | wc -l) < 2)); do
        (($(date +%s) < deadline)) || fail "the killed host pod never came back"
    done
    printf 'both hosts are ready again\n'
}

# --- the run ----------------------------------------------------------------
printf 'sproutfs soak: seed %s, %s rounds, witness %s of memory and disk\n' \
    "$seed" "$rounds" "$witness_bytes"
printf 'forks %s local and %s remote, %s migrations, %s stops and starts per round\n' \
    "$forks_local" "$forks_remote" "$migrations" "$stops"
printf 'one start in %s is cold, and one cold start in %s is grown to %s of memory and %s of disk\n' \
    "$cold_starts" "$cold_resizes" "$cold_memory" "$cold_disk"
ctl hosts
(($(ready_hosts | wc -l) >= 2)) || fail 'the soak needs two ready hosts'
# The round a host is lost in, drawn now so it is the seed's and not a moment's.
next_draw
stream
kill_round=$((1 + mixed % rounds))
printf 'a host is killed in round %s\n' "$kill_round"

round=0
# A soak that stopped on a failed check left its VMs where they were, and this
# run would otherwise walk them: everything it believes about a guest is the
# (seed, step) it wrote down when it filled that guest, and it has none for a VM
# it never created. So the deployment starts as empty as the directory this
# records into, which is cleared above for the same reason.
listed_before=$(ctl list | awk 'NR > 1 { print $1 }' | wc -l | tr -d ' ')
if ((listed_before > 0)); then
    step "deleting $listed_before VMs an earlier run left in the deployment"
    delete_every_vm
fi
sample_store begin
step "creating $start_vms VMs from the $template template"
for _ in $(seq "$start_vms"); do
    create_vm || fail 'the soak could not create the VMs it starts with'
done
ctl list

for ((round = 1; round <= rounds; round++)); do
    round_began=$(now)
    step "round $round of $rounds"
    mapfile -t live < <(running_vms)
    ((${#live[@]} > 0)) || fail 'no VM is running'

    # The kill round captures every VM and loses a host straight away, before
    # anything in this round has mutated a guest: see lose_a_host for why that
    # ordering is what makes the check exact.
    if ((round == kill_round)); then
        for vm in "${live[@]}"; do
            ctl capture "$vm" > /dev/null || fail "capturing $vm before the kill failed"
            captured_step[$vm]=${witness_step[$vm]}
        done
        lose_a_host
        mapfile -t live < <(running_vms)
        ((${#live[@]} > 0)) || fail 'no VM is running after the host was lost'
    fi

    # 1. every running VM does some work and is checked.
    next_draw
    while read -r vm; do
        [[ -n $vm ]] || continue
        witness_mutate "$vm"
        check_at "$vm" mutated
    done < <(shuffle "${live[@]}")

    # 2. one parent fans out, on its own host and on the other one.
    next_draw
    parent=$(sample 1 "${live[@]}")
    if [[ -n $parent && -n $(host_of "$parent") ]]; then
        step "forking $parent: $forks_local here and $forks_remote on the other host"
        fork_onto "$parent" "$forks_local" - local
        away=$(other_host "$(host_of "$parent")")
        if [[ -n $away ]]; then
            fork_onto "$parent" "$forks_remote" "$away" remote
        fi
    fi

    # 3, 4. the round's shares migrate, and stop and start.
    mapfile -t live < <(running_vms)
    step "migrating $migrations of ${#live[@]} VMs"
    next_draw
    while read -r vm; do
        [[ -n $vm ]] || continue
        migrate_vm "$vm"
    done < <(sample "$migrations" "${live[@]}")

    mapfile -t live < <(running_vms)
    step "stopping and starting $stops of ${#live[@]} VMs"
    next_draw
    while read -r vm; do
        [[ -n $vm ]] || continue
        stop_and_start "$vm"
    done < <(sample "$stops" "${live[@]}")

    # 5. the population is trimmed back to what two hosts admit.
    delete_down

    printf '%s\t%s\t%s\n' "$round" "$round_began" "$(now)" >> "$run_dir/rounds.tsv"
    sample_store "round-$round"
    ctl list
done

# --- the end ----------------------------------------------------------------
round=end
step 'a last check of every VM, then deleting them all'
while read -r vm; do
    [[ -n $vm ]] || continue
    check_at "$vm" final
done < <(running_vms)

delete_every_vm
sample_store end

step 'checking the deployment'
# Nothing but the templates, one per guest image, and the lineages the deleted
# VMs pinned may remain.
# A deployment that disagrees with itself is the answer rather than a failed
# request, so the violations are printed and the exit status is what fails.
if ! checked=$(ctl check 2>&1); then
    printf '%s\n' "$checked" | tee "$run_dir/check.txt"
    fail 'the deployment disagrees with itself'
fi
printf '%s\n' "$checked" | tee "$run_dir/check.txt"

step 'collecting'
kubectl logs -n "$namespace" -l app.kubernetes.io/name=sproutfs-host \
    --since-time="$run_began" --tail=-1 --prefix=false > "$run_dir/host.log" || true
grep '"msg":"host: checkpoint"' "$run_dir/host.log" > "$run_dir/checkpoints.jsonl" || true

python3 - "$run_dir" "$seed" "$kill_round" > "$run_dir/summary.txt" <<'SUMMARY'
"""Tabulate one soak: what each round did, what it cost, and every check."""
import collections, pathlib, sys

run = pathlib.Path(sys.argv[1])
seed, kill_round = sys.argv[2], sys.argv[3]
MIB = 1024 * 1024


def rows(name, width):
    path = run / name
    if not path.exists():
        return []
    found = []
    for line in path.read_text().splitlines():
        parts = line.split("\t")
        if len(parts) == width:
            found.append(parts)
    return found


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


def seconds(text):
    try:
        return float(text)
    except ValueError:
        return None


operations = rows("operations.tsv", 7)
checks = rows("checks.tsv", 7)
refusals = rows("refusals.tsv", 4)
store = rows("store.tsv", 7)

print(f"seed {seed}; a host was killed in round {kill_round}")
print(f"{len(operations)} operations, {len(checks)} checks, {len(refusals)} refusals")
print()
print("The two reported columns are what each operation itself reported:")
print("  fork      the parent's pause, and how long the children took to run")
print("  migrate   the pause the guest paid, and the stream behind it")
print("  stop      the checkpoint it published")
print("  start     the checkpoint it came back at")
print("  kill-host how many VMs the lost host was running")
print("The wall columns are what the whole call took from here, control plane")
print("round trips and all.")

# Every operation of one kind in one round, with the timings it reported.
counted = collections.defaultdict(list)
for round_name, kind, _vm, _host, first, second, elapsed in operations:
    counted[(round_name, kind)].append((seconds(first), seconds(second), seconds(elapsed)))

lines = []
for (round_name, kind), entries in counted.items():
    firsts = [e[0] for e in entries if e[0] is not None]
    seconds_reported = [e[1] for e in entries if e[1] is not None]
    elapsed = [e[2] for e in entries if e[2] is not None]
    lines.append([
        round_name, kind, len(entries),
        f"{max(firsts):.3f}" if firsts else "-",
        f"{max(seconds_reported):.3f}" if seconds_reported else "-",
        f"{sum(elapsed):.1f}" if elapsed else "-",
        f"{max(elapsed):.1f}" if elapsed else "-",
    ])
table("operations per round",
      ["round", "operation", "count", "reported", "reported 2", "total s", "slowest s"], lines)

# What each kind of operation cost across the whole run, which is the line a
# reader wants: a fork's pause, a migration's pause and stream, a stop, a start.
across = collections.defaultdict(list)
for round_name, kind, _vm, _host, first, second, elapsed in operations:
    across[kind].append((seconds(first), seconds(second), seconds(elapsed)))
lines = []
for kind, entries in sorted(across.items()):
    firsts = [e[0] for e in entries if e[0] is not None]
    later = [e[1] for e in entries if e[1] is not None]
    elapsed = [e[2] for e in entries if e[2] is not None]
    lines.append([
        kind, len(entries),
        f"{sum(firsts) / len(firsts):.3f}" if firsts else "-",
        f"{max(firsts):.3f}" if firsts else "-",
        f"{sum(later) / len(later):.3f}" if later else "-",
        f"{sum(elapsed) / len(elapsed):.2f}" if elapsed else "-",
        f"{max(elapsed):.2f}" if elapsed else "-",
    ])
table("what each operation cost",
      ["operation", "count", "mean reported", "worst reported", "mean reported 2",
       "mean wall s", "worst wall s"], lines)

# Every check, by round and outcome. A failed one ends the run, so a complete
# table is a run in which every guest held exactly what it wrote.
outcomes = collections.Counter()
by_kind = collections.defaultdict(collections.Counter)
for round_name, _vm, what, _seed, _step, outcome, _elapsed in checks:
    outcomes[outcome] += 1
    by_kind[what][outcome] += 1
lines = [[what, counts.get("ok", 0), counts.get("failed", 0), counts.get("timeout", 0)]
         for what, counts in sorted(by_kind.items())]
table("checks", ["after", "held", "failed", "no answer"], lines)
print(f"\n{outcomes.get('ok', 0)} checks held, {outcomes.get('failed', 0)} found other bytes "
      f"and {outcomes.get('timeout', 0)} went unanswered")

if refusals:
    table("placements the deployment refused",
          ["round", "operation", "vm", "reason"], refusals)

# The object store over the run, per host and operation: the whole of what this
# checkpoint model cost in object traffic.
counters = collections.defaultdict(dict)
for mark, _at, host, op, calls, failures, byte in store:
    counters[(host, op)][mark] = (int(calls), int(failures), int(byte))
lines = []
for (host, op), marks in sorted(counters.items()):
    first = marks.get("begin", (0, 0, 0))
    last = marks.get("end", first)
    lines.append([host, op, last[0] - first[0], last[1] - first[1],
                  f"{(last[2] - first[2]) / MIB:.1f}"])
table("object store over the run", ["host", "op", "calls", "failed", "MiB"], lines)
SUMMARY
cat "$run_dir/summary.txt"

printf '\nSoak complete. Everything it recorded is under %s.\n' "$run_dir"
