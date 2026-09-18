#!/usr/bin/env bash
# What the model CLI prints, against what the real one prints.
#
# scripts/lib/demo-soak.sh reads sproutfsctl's output by column and by phrase —
# the host a VM is on is the second field of `list`, a fork's children are the
# first field of every row after the header, a stop's checkpoint is the number
# after the word "checkpoint" — so a model it is run against is only worth
# anything if it prints what the real one prints. The formats are
# cmd/sproutfsctl/main.go's, and cmd/sproutfsctl's own output tests are what
# pins them: TestListPrintsEveryVMAndItsHost, TestHostsPrintsTheSharedPageCount,
# TestForkPrintsEveryForksTimings, TestCreatePrintsWhereTheVMWentAndWhatItCost,
# TestMigratePrintsThePauseAndTheStream, TestStopPrintsWhichHostClosedTheVM,
# TestStartPrintsWhereTheVMCameBackAndAtWhichCheckpoint and
# TestCheckOnADeploymentThatAgreesWithItselfSaysSoAndSucceeds. Every string
# below is one of those, with the model's own names in it; the demo hosts are
# pods of a Deployment named sproutfs-host-0 and sproutfs-host-1, and the pod
# that replaces one is sproutfs-host-2 rather than the name that went. All of
# them are as wide as the sproutfs-host-a of those tests, so the columns fall in
# the same places.
#
# The tables go through text/tabwriter with a padding of two, which is what
# makes every column as wide as its widest cell and two spaces more, and the
# last column of a row carry no padding at all.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=scripts/test/lib.sh
. "$here/lib.sh"

work=${SPROUTFS_TEST_WORK:-$(mktemp -d "${TMPDIR:-/tmp}/sproutfs-format-test.XXXXXX")}
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
export SPROUTFS_FAKE_STATE=$work/model
ctl() { "$repo/scripts/test/fake-sproutfsctl.sh" "$@"; }

want_printed() {
    local what=$1 wanted=$2
    shift 2
    local got
    got=$("$@" 2>&1) || true
    [[ $got == "$wanted" ]] && { ok; return; }
    bad "$what" "it printed:" "$got" "and the CLI prints:" "$wanted"
}

# create names the VM, where it went and what each phase of it cost.
want_printed 'create says where the VM went and what it cost' \
    'vm-1 on sproutfs-host-0 (template 4.00s, fork 0.50s, boot 1.25s, root 0.75s, total 6.50s)' \
    ctl create --template alpine

# list is the table demo-soak.sh reads a VM's host out of the second column of.
ctl create --template alpine > /dev/null
want_printed 'list is the table of every VM and its host' \
    'VM    HOST             STATE    CHECKPOINT
vm-1  sproutfs-host-0  running  1
vm-2  sproutfs-host-0  running  1' \
    ctl list

# A stopped VM keeps its row and its identity, and its host is a dash: that is
# what a flow asking whether any host still runs it reads.
ctl stop vm-2 > /dev/null
want_printed 'a stopped VM is listed with no host' \
    'VM    HOST             STATE    CHECKPOINT
vm-1  sproutfs-host-0  running  1
vm-2  -                stopped  2' \
    ctl list
want_printed 'stop says which host closed the VM and at which checkpoint' \
    'stopped vm-1 on sproutfs-host-0 at checkpoint 2 in 0.420s' \
    ctl stop vm-1
want_printed 'start says where the VM came back and at which checkpoint' \
    'vm-1 started on sproutfs-host-1 from checkpoint 2 in 1.250s' \
    ctl start vm-1 --to sproutfs-host-1
ctl start vm-2 --to sproutfs-host-0 > /dev/null

# hosts is the table a flow reads which hosts are ready out of the second
# column of.
want_printed 'hosts is the table of the host pods' \
    'HOST             READY  RUNNING  SERVING  RESIDENT  SHARED  SERVED  STATE
sproutfs-host-0  true   1        0        900       512     64      ok
sproutfs-host-1  true   1        0        900       512     64      ok' \
    ctl hosts

# fork prints one row per child, all of them carrying the one pause that one
# fork point of the parent cost.
want_printed 'fork prints every fork and the timings it cost' \
    'FORK  HOST             PAUSE  START  TOTAL
vm-3  sproutfs-host-0  0.012  0.300  0.316
vm-4  sproutfs-host-0  0.012  0.300  0.316' \
    ctl fork vm-2 --count 2

want_printed 'migrate prints the pause and the stream behind it' \
    'vm-3 moved from sproutfs-host-0 to sproutfs-host-1: pause 0.082s, stream 1.400s, 96 pages from the source, 12 unpublished' \
    ctl migrate vm-3 --to sproutfs-host-1

want_printed 'capture prints the checkpoint it published' \
    'vm-4 checkpoint 2 on sproutfs-host-0: pause 0.010s, publish 0.250s' \
    ctl capture vm-4

want_printed 'kill-host names the host it killed' 'killed sproutfs-host-1' \
    ctl kill-host sproutfs-host-1
want_printed 'recover says where the VM was reopened and from which checkpoint' \
    'vm-3 reopened on sproutfs-host-0 from checkpoint 1 in 2.500s' \
    ctl recover vm-3

want_printed 'delete names what it deleted' 'deleted vm-3' ctl delete vm-3

# store is one row per host and operation, which is what two readings
# subtracted make a phase's cost out of. The counters are one process's, so the
# pod that replaced the killed sproutfs-host-1 has rows of its own at zero and
# its predecessor's stay at what it had served when it went.
want_printed 'store is one row per host and operation' \
    'HOST             OP      CALLS  FAILED  BYTES
sproutfs-host-0  head    0      0       0
sproutfs-host-0  get     25     0       23068672
sproutfs-host-0  put     27     0       26214400
sproutfs-host-0  delete  0      0       0
sproutfs-host-0  list    0      0       0
sproutfs-host-1  head    0      0       0
sproutfs-host-1  get     11     0       12582912
sproutfs-host-1  put     0      0       0
sproutfs-host-1  delete  0      0       0
sproutfs-host-1  list    0      0       0
sproutfs-host-2  head    0      0       0
sproutfs-host-2  get     0      0       0
sproutfs-host-2  put     0      0       0
sproutfs-host-2  delete  0      0       0
sproutfs-host-2  list    0      0       0' \
    ctl store

want_printed 'a deployment that agrees with itself says so' \
    "the deployment's durable state agrees with itself" \
    ctl check

# And a request the deployment turns down is a non-zero exit with the reason on
# standard error, which is what a flow recording a refusal reads.
export SPROUTFS_FAKE_REFUSE=migrate:2
if ctl migrate vm-4 --to sproutfs-host-1 > "$work/out" 2> "$work/err"; then
    bad 'a refused placement exits non-zero' "it printed $(cat "$work/out")"
else
    ok
fi
want_file_has "$work/err" '503 Service Unavailable' 'a refusal says what the deployment said'
unset SPROUTFS_FAKE_REFUSE

# A cold start says so, and the checkpoint it names is the one that discarded
# the memory rather than the one the stop published: that is the checkpoint the VM
# is at now, and it is what a flow reading the number after "checkpoint" gets.
# The real format is cmd/sproutfsctl's
# TestColdStartPrintsThatTheVMCameBackWithoutItsMemory.
ctl stop vm-4 > /dev/null
if ctl start vm-4 --memory 1G > "$work/out" 2> "$work/err"; then
    bad 'a warm start that asks for a shape is refused' "it printed $(cat "$work/out")"
else
    ok
fi
want_file_has "$work/err" 'need --cold' 'a warm start says why a shape is refused'
want_printed 'a cold start says the VM came back without its memory' \
    'vm-4 cold started on sproutfs-host-0 from checkpoint 4 in 1.250s' \
    ctl start vm-4 --to sproutfs-host-0 --cold --memory 768M --disk 3G

report format-test.sh
