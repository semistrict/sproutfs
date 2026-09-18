#!/usr/bin/env bash
# What scripts/lib/demo-soak.sh does, run against the model in
# fake-sproutfsctl.sh rather than a cluster.
#
# The soak is the flow that says whether a deployment keeps a guest's bytes
# across a fork, a migration, a stop, a start and a host loss, and its answer is
# only worth what its own bookkeeping is worth: an expectation that drifted, a
# list a loop only got the first line of, or a shape that is not the seed's
# would all report a clean run over work that was never done. None of that needs
# a cluster to find, and this is where it is found.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=scripts/test/lib.sh
. "$here/lib.sh"
shell=$(find_shell)
work=${SPROUTFS_TEST_WORK:-$(mktemp -d "${TMPDIR:-/tmp}/sproutfs-soak-test.XXXXXX")}
# What a run left behind is kept when something did not hold, because the first
# thing anyone wants then is the output and the recorded files themselves.
keep_or_clear() {
    if ((failures > 0)); then
        printf 'what these runs left is under %s\n' "$work" >&2
    else
        rm -rf -- "$work"
    fi
}
trap keep_or_clear EXIT

# soak_run runs one soak against a model of its own and leaves everything under
# $run: out the run output, status its exit status, and the recorded files where
# the flow put them. Its arguments after the directory are the settings that run
# is given, as NAME=VALUE.
#
# It prints nothing: a test says what it wanted, not what happened on the way.
record=
output=
status=
# leftover is how many VMs the deployment already has when the flow starts.
leftover=0
soak_run() {
    local run=$1 setting
    shift
    rm -rf -- "$run"
    mkdir -p "$run/bin" "$run/model"
    ln -s "$repo/scripts/test/fake-kubectl.sh" "$run/bin/kubectl"
    # VMs an earlier run left in the deployment, which is what a soak that
    # stopped on a failed check leaves behind for the next one to find.
    local left
    for ((left = 0; left < ${leftover:-0}; left++)); do
        SPROUTFS_FAKE_STATE=$run/model "$repo/scripts/test/fake-sproutfsctl.sh" create > /dev/null
    done
    (
        PATH=$run/bin:$PATH
        export PATH
        export SPROUTFS_FAKE_STATE=$run/model
        export SPROUTFS_FAKE_CTL=$repo/scripts/test/fake-sproutfsctl.sh
        export SPROUTFS_DEMO_RUN_DIR=$run/record
        export KUBECONFIG=$run/kubeconfig
        for setting in "$@"; do export "${setting?}"; done
        "$shell" "$repo/scripts/lib/demo-soak.sh" < /dev/null
    ) > "$run/out" 2>&1 && printf '0\n' > "$run/status" || printf '%s\n' "$?" > "$run/status"
    record=$run/record
    output=$run/out
    read -r status < "$run/status"
}

# small is a run short enough to be a test and wide enough to do every one of
# the things the soak does at least twice.
small=(SOAK_ROUNDS=2 SOAK_FORKS_LOCAL=2 SOAK_FORKS_REMOTE=1
    SOAK_MIGRATIONS=2 SOAK_STOPS=2 SOAK_MAX_VMS=4 SOAK_START_VMS=2)

# --- a run in which every guest held what it wrote ----------------------------

soak_run "$work/clean" SOAK_SEED=3 "${small[@]}"
want_status "$status" 0 'a soak whose every guest held what it wrote'
want_file_lacks "$record/checks.tsv" '	failed' 'a clean soak records no failed check'
want_file_has "$record/summary.txt" 'checks held, 0 found other bytes and 0 went unanswered' \
    'the summary counts the checks'
want_file_has "$record/summary.txt" '## operations per round' 'the summary has the round table'
want_file_has "$record/summary.txt" '## object store over the run' 'the summary has the store table'
want_file_has "$record/check.txt" 'agrees with itself' 'the deployment is checked at the end'
want_equal "$(wc -l < "$record/rounds.tsv" | tr -d ' ')" 2 'every round records its window'
want_file_has "$output" 'Soak complete' 'a soak that got to the end says so'
want "$([[ -f $record/host.log ]] && echo 1 || echo 0)" 'the hosts logs are collected'

# Every one of the soak's operations happened, and every kind of check with it.
for kind in fill mutate create fork-local fork-remote migrate stop start delete kill-host; do
    want_file_has "$record/operations.tsv" "	$kind	" "the soak $kind"
done
for what in filled mutated inherited-local inherited-remote forked-local diverged-local \
    before-migrate after-migrate before-stop after-start recovered final; do
    want_file_has "$record/checks.tsv" "	$what	" "a guest is checked $what"
done

# Every recorded line has the number of columns the summary reads, so that
# nothing a run recorded is dropped when it is tabulated.
want_equal "$(awk -F'\t' 'NF != 7' "$record/operations.tsv" | wc -l | tr -d ' ')" 0 \
    'every operation is recorded in seven columns'
want_equal "$(awk -F'\t' 'NF != 7' "$record/checks.tsv" | wc -l | tr -d ' ')" 0 \
    'every check is recorded in seven columns'
want_equal "$(awk -F'\t' 'NF != 7' "$record/store.tsv" | wc -l | tr -d ' ')" 0 \
    'every store reading is recorded in seven columns'

# --- a ctl in a loop must not eat the list the loop is reading -----------------
# Every VM is deleted at the end, and a round that is over its bound deletes its
# whole share rather than the first of it. The flow reads both lists from a
# process substitution and calls ctl for each line; `kubectl exec -i` forwards
# this process's stdin to the guest, so a ctl that does not take its stdin from
# elsewhere reads the rest of the list and the loop ends after one line.

soak_run "$work/drain" SOAK_SEED=11 SOAK_ROUNDS=2 SOAK_FORKS_LOCAL=2 SOAK_FORKS_REMOTE=2 \
    SOAK_MIGRATIONS=2 SOAK_STOPS=2 SOAK_MAX_VMS=3 SOAK_START_VMS=2
want_status "$status" 0 'a soak that has to delete more than one VM in a round'
want_file_lacks "$output" 'still listed after deleting them all' 'every VM is deleted at the end'
# Two rounds each trimming a population of six or more back to three.
deletes=$(awk -F'\t' '$2 == "delete"' "$record/operations.tsv" | wc -l | tr -d ' ')
want "$((deletes >= 4))" "a round deletes its whole share: $deletes deletes recorded, want 4 or more"
# And the same for the lists the round's own shares are read from.
migrations=$(awk -F'\t' '$2 == "migrate"' "$record/operations.tsv" | wc -l | tr -d ' ')
want "$((migrations >= 4))" "a round migrates its whole share: $migrations recorded, want 4 or more"
stops=$(awk -F'\t' '$2 == "stop"' "$record/operations.tsv" | wc -l | tr -d ' ')
want "$((stops >= 4))" "a round stops its whole share: $stops recorded, want 4 or more"

# And what all of that deleting is for: the population a round leaves behind is
# within the bound, because the time a round takes is linear in it. The flow
# lists every VM at the end of each round, so the widest of those lists is the
# high-water mark.
widest=$(awk '/^VM +HOST +STATE/ { if (count > most) most = count; count = 0; next }
              /^vm-[0-9]+ +(sproutfs-host-[0-9]+|-) +/ { count++ }
              END { if (count > most) most = count; print most + 0 }' "$output")
want "$((widest <= 3))" "the population stays within SOAK_MAX_VMS: it reached $widest, and the bound is 3"

# Both hosts are accounted for in the store table, which is what says a run used
# the deployment rather than one pod of it.
for host in sproutfs-host-0 sproutfs-host-1; do
    want_file_has "$record/store.tsv" "	$host	" "the store counters of $host are sampled"
    want_file_has "$record/summary.txt" "^$host " "the summary accounts for $host in the object store"
done

# --- a deployment an earlier run left VMs in ---------------------------------
# A soak ends by deleting every VM, and a soak that stopped on a failed check
# ends by not deleting any of them. The run after that one finds them, and what
# it must not do is walk a VM it knows nothing about: the whole of what it
# believes about a guest is a (seed, step) it wrote down when it filled it, and
# it has none for a VM it never created. It clears the deployment first, as it
# clears the directory it records into.

leftover=2
soak_run "$work/leftovers" SOAK_SEED=3 "${small[@]}"
leftover=0
want_status "$status" 0 'a soak starting on a deployment an earlier run left VMs in'
want_file_has "$output" 'deleting 2 VMs an earlier run left' 'the run says what it cleared away'
want_file_lacks "$output" 'unbound variable' 'no VM is walked that the run knows nothing about'

# --- one number reproduces a run ---------------------------------------------
# The seed decides the order VMs are walked in, which of them each share falls
# on, which round loses a host and every witness seed. A run that found
# something is worth nothing if it cannot be run again.

shape() { cut -f1-6 "$1/operations.tsv"; }
soak_run "$work/seed-a" SOAK_SEED=7 "${small[@]}"
want_status "$status" 0 'the first run of seed 7'
soak_run "$work/seed-b" SOAK_SEED=7 "${small[@]}"
want_status "$status" 0 'the second run of seed 7'
if diff <(shape "$work/seed-a/record") <(shape "$work/seed-b/record") > "$work/shape.diff"; then
    ok
else
    bad 'two runs of one seed do the same thing' "$(head -n 20 "$work/shape.diff")"
fi
soak_run "$work/seed-c" SOAK_SEED=8 "${small[@]}"
want_status "$status" 0 'a run of seed 8'
if diff -q <(shape "$work/seed-a/record") <(shape "$work/seed-c/record") > /dev/null; then
    bad 'two seeds do different things: seed 8 did exactly what seed 7 did'
else
    ok
fi

# --- a child is checked against its parent before it is anything else ---------
# What a fork inherited is what its parent held at the seal, and
# that is the one thing a fork has to be asked before it is given a seed of its
# own. The model records every request a guest was made, so the order is there
# to read.

log=$work/clean/model/log
first_witness() { grep "^witness $1 " "$log" | head -n 1; }
child=$(awk -F'\t' '$3 == "inherited-local" { print $2; exit }' "$work/clean/record/checks.tsv")
want_equal "$(first_witness "$child" | cut -d' ' -f3)" check \
    'the first thing a fork is asked is whether it holds its parents bytes'
parent_seed=$(awk -F'\t' -v c="$child" '$2 == c && $3 == "inherited-local" { print $4; exit }' \
    "$work/clean/record/checks.tsv")
want_equal "$(first_witness "$child" | cut -d' ' -f4)" "$parent_seed" \
    'a fork is checked against the seed its parent carries'
# And then it is filled under a seed of its own, which is not its parents.
own_seed=$(grep "^witness $child fill " "$log" | head -n 1 | cut -d' ' -f4)
want "$([[ -n $own_seed && $own_seed != "$parent_seed" ]] && echo 1 || echo 0)" \
    "a fork diverges under a seed of its own: it was filled with '$own_seed'," \
    "and its parent carries $parent_seed"

# --- what a recovered guest is checked at ------------------------------------
# A host that is killed takes everything its guests did since their last
# checkpoint with them, so the step a recovered guest holds is the one its last
# capture published. The model rewinds exactly that far, so a flow that checks a
# recovered guest against anything else is told it holds other bytes.

want_file_has "$work/clean/record/checks.tsv" '	recovered	.*	ok	' \
    'a recovered guest holds the step its last capture published'
# The capture that names it comes before the kill, and nothing mutates between.
want_equal "$(grep -c 'capture .* mutate' "$log" || true)" 0 'the model log is one line per request'
kill_line=$(grep -n '^kill-host ' "$log" | head -n 1 | cut -d: -f1)
last_mutate=$(grep -n '^witness .* mutate ' "$log" | awk -F: -v k="$kill_line" '$1 < k { n = $1 } END { print n + 0 }')
last_capture=$(grep -n '^capture ' "$log" | awk -F: -v k="$kill_line" '$1 < k { n = $1 } END { print n + 0 }')
want "$((last_capture > last_mutate))" \
    "every guest is captured after it last mutated and before the host is killed:" \
    "the last mutate is at line $last_mutate and the last capture at $last_capture"

# --- a placement the deployment refuses is recorded, not failed ---------------

soak_run "$work/refused" SOAK_SEED=3 "${small[@]}" \
    'SPROUTFS_FAKE_REFUSE=fork:1 migrate:1 stop:2 start:1'
want_status "$status" 0 'a soak in which four placements were refused'
want_equal "$(awk -F'\t' 'NF == 4' "$record/refusals.tsv" | wc -l | tr -d ' ')" 4 \
    'every refusal is recorded in four columns'
want_file_has "$record/summary.txt" 'placements the deployment refused' \
    'the summary names the refusals'
want_file_has "$output" 'refused ' 'a refusal is printed as it happens'

# A refusal whose reason runs over more than one line is still one row: a
# recorded file with a row the summary cannot read is a refusal that was
# swallowed.
soak_run "$work/multiline" SOAK_SEED=3 "${small[@]}" \
    'SPROUTFS_FAKE_REFUSE=fork:1' SPROUTFS_FAKE_MULTILINE=1
want_status "$status" 0 'a soak whose refusal carried a reason of several lines'
want_equal "$(wc -l < "$record/refusals.tsv" | tr -d ' ')" 1 \
    'a refusal is one recorded row however many lines its reason had'
want_equal "$(awk -F'\t' 'NF != 4' "$record/refusals.tsv" | wc -l | tr -d ' ')" 0 \
    'a refusal of several lines still has four columns'
want_file_has "$record/summary.txt" 'placements the deployment refused' \
    'the summary names a refusal whose reason had several lines'

# --- a guest that does not hold what it wrote ends the run --------------------

soak_run "$work/corrupt" SOAK_SEED=3 "${small[@]}" SPROUTFS_FAKE_CORRUPT=9
want_status "$status" 1 'a soak in which one guest held other bytes'
want_file_has "$record/checks.tsv" '	failed	' 'the check that failed is recorded'
want_file_has "$output" 'does not hold' 'the run says which VM did not hold what it wrote'

# --- a check the guest never answered is not a guest that lost its bytes ------
# The agent kills a command that runs past its timeout and reports 124, which is
# a guest that was too slow rather than a guest whose memory came back wrong.
# Both end the run, and a run that called the first the second would have an
# operator looking for a defect in the pager over a witness that wanted longer.

soak_run "$work/slow" SOAK_SEED=3 "${small[@]}" SPROUTFS_FAKE_TIMEOUT=9
want_status "$status" 1 'a soak in which a check ran past the agents timeout'
want_file_has "$record/checks.tsv" '	timeout	' 'a check that timed out is recorded as one'
want_file_lacks "$record/checks.tsv" '	failed	' 'a check that timed out is not a guest holding other bytes'
want_file_has "$output" 'did not answer' 'the run says the guest did not answer in time'

# --- a deployment that disagrees with itself ends the run --------------------

soak_run "$work/violations" SOAK_SEED=3 "${small[@]}" SPROUTFS_FAKE_VIOLATIONS=1
want_status "$status" 1 'a soak whose deployment disagreed with itself at the end'
want_file_has "$record/check.txt" 'demo/vm/vm-a' 'every violation is recorded'
want_file_has "$output" 'the deployment disagrees with itself' 'the run says so'

# --- the cold half of the stop and start --------------------------------------
# A cold start discards the guest's memory and the VMM state with it and boots
# the kernel from the root volume. What survives is the disk, exactly as the
# stop published it, so what the flow may ask of such a guest is its disk alone
# — with no witness resident, because the process that held the buffer went with
# the memory — and it must fill the witness again before anything mutates that
# VM. A flow that asked the old question would be told the guest holds nothing.

soak_run "$work/cold" SOAK_SEED=3 "${small[@]}" SOAK_COLD_STARTS=1 SOAK_COLD_RESIZES=0
want_status "$status" 0 'a soak whose every restarted VM came back cold'
want_file_lacks "$record/checks.tsv" '	failed' 'a cold-started guest holds what its disk held'
want_file_has "$record/operations.tsv" '	cold-start	' 'the soak cold starts'
want_file_lacks "$record/operations.tsv" '	start	' 'every start of this run was a cold one'
want_file_has "$record/checks.tsv" '	after-cold-start	' 'a cold-started guest is checked'
want_file_has "$work/cold/out" 'cold started on ' 'the run says a VM came back cold'
# The check of a cold-started guest is the disk alone, and it is followed by a
# fill: the memory is gone, so the expectation starts again.
log=$work/cold/model/log
cold_vm=$(awk -F'\t' '$2 == "cold-start" { print $3; exit }' "$record/operations.tsv")
want_file_has "$log" "^witness $cold_vm check-disk " 'a cold-started guest is asked for its disk alone'
want_equal "$(grep -c "^witness $cold_vm check .*" "$log" | tr -d ' ')" \
    "$(grep -c "^witness $cold_vm check " "$log" | tr -d ' ')" 'the model saw the checks it recorded'
refilled=$(grep -n "^witness $cold_vm " "$log" | grep -A 1 'check-disk' | grep -c ' fill ' || true)
want "$((refilled >= 1))" 'the witness is filled again after a cold start, before anything mutates that VM'
# And the memory really is gone: a check that wanted a resident witness would be
# answered by a socket nothing is listening on.
want_file_lacks "$record/checks.tsv" '	timeout	' 'no check went unanswered'

# --- a cold start that resizes the VM ----------------------------------------
# A cold boot is the one moment a VM's shape can change, because nothing in
# memory describes it any more. The larger volume's new pages read as zeroes, so
# the guest grows its filesystem over them with `witness grow`, and the witness
# file it was already holding has to survive that.

soak_run "$work/resized" SOAK_SEED=5 "${small[@]}" SOAK_COLD_STARTS=1 SOAK_COLD_RESIZES=1 \
    SOAK_COLD_MEMORY=768M SOAK_COLD_DISK=3G
want_status "$status" 0 'a soak whose every cold start also resized the VM'
want_file_has "$record/operations.tsv" '	cold-start	.*	768M/3G	' \
    'a resized cold start records the shape it asked for'
want_file_has "$record/operations.tsv" '	resize	' 'the guest grows its filesystem'
want_file_has "$work/resized/model/log" '^witness .* grow ' 'the guest is asked to grow its filesystem'
want_file_lacks "$record/checks.tsv" '	failed' 'the witness file survives a filesystem grown over it'
# The resize comes before the check: a filesystem that has not taken the pages
# its volume gained cannot read the file back at all.
log=$work/resized/model/log
resize_line=$(grep -n '^witness .* grow ' "$log" | head -n 1 | cut -d: -f1)
check_line=$(grep -n '^witness .* check-disk ' "$log" | head -n 1 | cut -d: -f1)
want "$((resize_line < check_line))" \
    "the filesystem is grown before the disk is checked: the grow at line $resize_line and the check at $check_line"

# --- and a run that draws no cold start at all --------------------------------

soak_run "$work/warm" SOAK_SEED=3 "${small[@]}" SOAK_COLD_STARTS=0
want_status "$status" 0 'a soak that draws no cold start'
want_file_lacks "$record/operations.tsv" '	cold-start	' 'no VM came back cold'
want_file_has "$record/operations.tsv" '	start	' 'every start of this run was a warm one'
want_file_has "$record/checks.tsv" '	after-start	' 'a warm-started guest is checked in full'

report soak-test.sh
