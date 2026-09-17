#!/usr/bin/env bash
# A guest larger than the x86_64 MMIO gap, on the demo node.
#
# scripts/demo-gce.sh copies this one file to the node and runs it there:
#
#     bash demo-bigguest.sh
#
# x86_64 reserves 3 GiB to 4 GiB for MMIO, so a VM with more than 3 GiB of RAM
# is two guest memory regions over the one RAM volume, which docs/vm-memory.md
# describes: volume bytes [0, 3 GiB) are guest addresses [0, 3 GiB), and volume
# offset 3 GiB + x is guest address 4 GiB + x. Only an x86_64 node exercises
# that mapping; the Lima instance this is otherwise developed against is
# aarch64, so this run is the only thing that does.
#
# It boots a guest and cold starts it at 4 GiB, checks the guest's own e820 map
# for the second usable range, writes 3200 MiB of a non-zero pattern — more than
# fits below the gap — and reads the tail of it back in the parent, in a fork of
# it, and again after a migration. The tail is the part that can only live above
# the gap, which is the whole point: a wrong mapping loses or duplicates exactly
# those bytes.
#
# The RAM comes from a cold start rather than from the deployment's own
# SPROUTFS_VM_MEMORY_BYTES, which is where a VM's memory comes from only when
# its guest image is first imported: a template is named by the image's bytes,
# so the image this creates from was imported once and holds the size it was
# imported at. A cold boot is the one moment a VM's shape can change, and it is
# what gives one VM memory its template never had, which is all this run needs.
#
# The guest's agent kills any one command at ten minutes, which is also how long
# the orchestrator waits on a host, so the file is read back in a window rather
# than whole.
#
# Nothing in the deployment is changed, so every other flow still sees its
# 512 MiB default; the VM this run leaves behind is deleted at the end.
set -euo pipefail

export KUBECONFIG=${KUBECONFIG:-/etc/rancher/k3s/k3s.yaml}
namespace=${SPROUTFS_DEMO_NAMESPACE:-sproutfs}
# 4 GiB of guest RAM, which is more than the 3 GiB below the gap. It still fits
# the host's 5 GiB arena, so the pager stays resident and nothing spills.
big=${SPROUTFS_DEMO_BIG_BYTES:-4294967296}
# 3200 MiB of pattern cannot fit below the gap; the last 256 MiB of it is read
# back, which is 2944 MiB into the file and unambiguously above it.
pattern_mib=${SPROUTFS_DEMO_PATTERN_MIB:-3200}
tail_skip_mib=2944
tail_count_mib=256
exec_timeout=300s
agent_timeout=180

ctl() { kubectl exec -n "$namespace" -i deploy/sproutfs-orchestrator -- sproutfsctl "$@" < /dev/null; }
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

ctl hosts

step 'boot a guest and give it 4 GiB of RAM'
created=$(ctl create --template alpine)
printf '%s\n' "$created"
vm=${created%% *}
[[ $vm == vm-* ]] || fail "create did not name a VM: $created"
agent_ready "$vm" || fail "the agent in $vm never answered"
# A cold boot is the one moment a VM's shape can change: its memory and the VMM
# state go in one checkpoint and the guest boots its kernel from the root
# volume, at whatever size the start asks for. It is how a VM is given RAM its
# template never had, and what it costs here is the boot this guest was about to
# do anyway.
ctl stop "$vm" > /dev/null || fail "stopping $vm failed"
started=$(ctl start "$vm" --cold --memory "$big") || fail "cold starting $vm at $big bytes failed"
printf '%s\n' "$started"
agent_ready "$vm" || fail "the agent in $vm never answered after the cold start"

# The guest's own account of its memory: a total above 3 GiB, and two usable
# e820 ranges with the second one starting at 4 GiB.
step 'what the guest sees'
seen=$(run_in "$vm" 'head -1 /proc/meminfo; dmesg | grep -i usable')
printf '%s\n' "$seen"
total=$(printf '%s\n' "$seen" | awk '/MemTotal/ { print $2 }')
((total > 3 * 1024 * 1024)) || fail "the guest reports only $total kB of RAM, which is not above the gap"
usable=$(printf '%s\n' "$seen" | grep -ci usable || true)
((usable >= 2)) || fail "the guest sees $usable usable e820 ranges, not the two a VM above the gap has"
printf '%s\n' "$seen" | grep -qi '0x0000000100000000' ||
    fail 'no usable e820 range begins at 4 GiB, so nothing is mapped above the gap'
printf 'the guest has %s kB over %s usable ranges, the second at 4 GiB\n' "$total" "$usable"

# The pipeline on the node is the control: the same bytes, md5'd outside the VM.
step "write and read back ${pattern_mib} MiB of a non-zero pattern"
want=$(dd if=/dev/zero bs=1M count="$pattern_mib" 2>/dev/null | tr '\0' 'Z' | md5sum | cut -d' ' -f1)
printf 'the node says the whole pattern is %s\n' "$want"
whole=$(run_in "$vm" "mkdir -p /mnt/big
    mount -t tmpfs -o size=$((pattern_mib + 200))M tmpfs /mnt/big
    dd if=/dev/zero bs=1M count=$pattern_mib 2>/dev/null | tr '\\0' 'Z' > /mnt/big/f
    md5sum /mnt/big/f" | awk '{ print $1 }')
[[ $whole == "$want" ]] || fail "the guest wrote $whole, not $want"
printf 'the guest wrote the same %s MiB\n' "$pattern_mib"

# Only the tail is read back from here on: it is the part that can only live
# above the gap, and a whole-file read would outlast the agent's own ceiling.
tail_want=$(dd if=/dev/zero bs=1M count="$tail_count_mib" 2>/dev/null | tr '\0' 'Z' | md5sum | cut -d' ' -f1)
printf 'the node says its last %s MiB is %s\n' "$tail_count_mib" "$tail_want"
read_tail() {
    run_in "$1" "dd if=/mnt/big/f bs=1M skip=$tail_skip_mib count=$tail_count_mib 2>/dev/null | md5sum" |
        awk '{ print $1 }'
}
got=$(read_tail "$vm")
[[ $got == "$tail_want" ]] || fail "the parent read $got above the gap, not $tail_want"
printf 'the parent reads its own tail correctly\n'

step 'a fork reads the same bytes above the gap'
ctl capture "$vm" > /dev/null
child=$(ctl fork "$vm" --count 1 | awk 'NR == 2 { print $1 }')
[[ $child == vm-* ]] || fail "forking $vm named no child"
agent_ready "$child" || fail "the agent in the fork $child never answered"
got=$(read_tail "$child")
[[ $got == "$tail_want" ]] || fail "the fork $child read $got above the gap, not $tail_want"
printf 'the fork %s reads the tail correctly\n' "$child"
ctl delete "$child" > /dev/null

step 'and so does the VM after a migration'
here=$(host_of "$vm")
there=$(other_host "$here")
[[ -n $there ]] || fail "no other host to migrate $vm to"
moved=$(ctl migrate "$vm" --to "$there")
printf '%s\n' "$moved"
[[ $(host_of "$vm") == "$there" ]] || fail "$vm is not on $there after the migration"
agent_ready "$vm" || fail "the agent in $vm did not answer after the move"
got=$(read_tail "$vm")
[[ $got == "$tail_want" ]] || fail "the moved $vm read $got above the gap, not $tail_want"
printf 'the moved %s reads the tail correctly on %s\n' "$vm" "$there"

ctl delete "$vm" > /dev/null 2>&1 || true
printf '\nA guest larger than the 3 GiB gap holds its bytes across a fork and a migration.\n'
