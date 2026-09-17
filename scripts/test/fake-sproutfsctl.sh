#!/usr/bin/env bash
# A sproutfsctl that answers out of a model instead of a cluster, so that
# scripts/lib/demo-soak.sh can be run and its bookkeeping asserted without one.
#
# Every line it prints is the format cmd/sproutfsctl prints, and those formats
# are pinned by cmd/sproutfsctl/execute_test.go and stop_test.go: this file is
# only a second implementation of them over an in-memory model, and
# scripts/test/format-test.sh checks the two against each other.
#
# The model is the point. It holds, per VM, what a witness inside that guest
# would hold — the (seed, step) its memory and disk carry — and what the VM's
# last checkpoint published. A fork copies the parent's; a migration and a stop
# and start carry it; a host that is killed rewinds every VM it ran back to what
# its checkpoint held. So `exec ... sproutfs-guest-witness check --seed S --step
# K` answers out of the model exactly as a real guest answers out of its memory,
# and a soak whose expectation bookkeeping is wrong fails here for the same
# reason it would fail on a cluster.
#
# The state lives under SPROUTFS_FAKE_STATE, one file per VM plus a few
# counters, because each call is a separate process: the script reaches this
# through `kubectl exec`, which is faked beside it.
#
# What can be made to go wrong, so that a flow's handling of it can be asserted:
#   SPROUTFS_FAKE_REFUSE     "fork:2 migrate:1" — that numbered call of that
#                            command is refused with a 503, as a placement no
#                            host admits is.
#   SPROUTFS_FAKE_CORRUPT    the numbered witness check whose guest answers with
#                            a mismatch.
#   SPROUTFS_FAKE_TIMEOUT    the numbered witness check whose guest runs past
#                            the agent's timeout, which exits 124.
#   SPROUTFS_FAKE_VIOLATIONS a `check` that finds the deployment disagreeing.
#   SPROUTFS_FAKE_MULTILINE  a refusal whose reason carries a newline and a tab.
#
# It is written for bash 3.2 so that it runs on a developer's machine as it does
# in CI; the flow under test is the one that needs bash 4.
set -euo pipefail

state=${SPROUTFS_FAKE_STATE:?"SPROUTFS_FAKE_STATE names the directory the model is kept in"}
vms=$state/vms
mkdir -p "$vms"

# --- the model's own bookkeeping --------------------------------------------

# counter bumps a named counter and prints its new value. It is what numbers the
# calls SPROUTFS_FAKE_REFUSE and the rest name.
counter() {
    local name=$1 value=0
    [[ -f $state/count.$name ]] && read -r value < "$state/count.$name"
    value=$((value + 1))
    printf '%s\n' "$value" > "$state/count.$name"
    printf '%s\n' "$value"
}

note() { printf '%s\n' "$*" >> "$state/log"; }

# refused reports whether this numbered call of one command is one the model was
# told to refuse.
refused() {
    local command=$1 nth=$2 entry
    for entry in ${SPROUTFS_FAKE_REFUSE:-}; do
        [[ $entry == "$command:$nth" ]] && return 0
    done
    return 1
}

# refuse answers as the CLI answers a request the deployment turned down: the
# orchestrator's own message on stderr and a non-zero exit.
refuse() {
    if [[ -n ${SPROUTFS_FAKE_MULTILINE:-} ]]; then
        printf '%s: no host admits it\n\tsproutfs-host-0: 5 GiB committed\n\tsproutfs-host-1: 5 GiB committed\n' \
            "$1" >&2
    else
        printf '%s: no host admits it: 503 Service Unavailable\n' "$1" >&2
    fi
    exit 1
}

# --- the hosts ---------------------------------------------------------------
# Two pods of a Deployment. A Deployment's pod names carry a suffix of its own
# choosing, so a pod that is killed or rolled comes back under another name and
# nothing a flow does may assume otherwise — which is exactly what these names
# model: the replacement of sproutfs-host-0 is sproutfs-host-2, not
# sproutfs-host-0 again. down is how many more `hosts` calls a replacement takes
# to be ready, so that a flow waiting for one waits for something.

hosts_file=$state/hosts
if [[ ! -f $hosts_file ]]; then
    printf 'sproutfs-host-0 true 0\nsproutfs-host-1 true 0\n' > "$hosts_file"
    printf '1\n' > "$state/count.host-pod"
fi

# next_host is the name the next pod of the host Deployment comes up under,
# which is one nothing in the deployment has held before.
next_host() { printf 'sproutfs-host-%s\n' "$(counter host-pod)"; }

# tick_hosts brings a killed host back, one `hosts` call at a time.
tick_hosts() {
    local name ready down rest=''
    while read -r name ready down; do
        if [[ $ready == false ]]; then
            down=$((down - 1))
            ((down > 0)) || { ready=true; down=0; }
        fi
        rest+=$(printf '%s %s %s' "$name" "$ready" "$down")$'\n'
    done < "$hosts_file"
    printf '%s' "$rest" > "$hosts_file"
}

host_ready() {
    local name ready down
    while read -r name ready down; do
        [[ $name == "$1" && $ready == true ]] && return 0
    done < "$hosts_file"
    return 1
}

# a_ready_host is where the model puts a VM whose placement named no host.
a_ready_host() {
    local name ready down
    while read -r name ready down; do
        [[ $ready == true ]] && { printf '%s\n' "$name"; return 0; }
    done < "$hosts_file"
    return 1
}

# kill_host takes one pod away and starts its replacement, which is what a
# Deployment does with a pod that is deleted: another name, not ready yet.
kill_host() {
    local name ready down rest='' replacement
    replacement=$(next_host)
    while read -r name ready down; do
        [[ $name == "$1" ]] && continue
        rest+=$(printf '%s %s %s' "$name" "$ready" "$down")$'\n'
    done < "$hosts_file"
    rest+=$(printf '%s false 2' "$replacement")$'\n'
    printf '%s' "$rest" > "$hosts_file"
    store_rows "$replacement"
}

# replace_host is one step of a rollout: the pod drains — every VM it runs
# migrates to another one, which is what its preStop hook does — and is then
# replaced by a pod of its own name. The replacement starts after the old pod is
# gone, because the two pods request the whole HugeTLB pool between them and
# there is no room on the node for a third.
replace_host() {
    local gone=$1 name ready down rest='' replacement destination vm
    destination=''
    while read -r name ready down; do
        [[ $name != "$gone" && $ready == true && -z $destination ]] && destination=$name
    done < "$hosts_file"
    [[ -n $destination ]] || { printf 'the rollout has nowhere to drain %s to\n' "$gone" >&2; exit 1; }
    for vm in $(order); do
        load_vm "$vm"
        [[ $vm_host == "$gone" ]] || continue
        vm_host=$destination
        save_vm "$vm"
    done
    replacement=$(next_host)
    while read -r name ready down; do
        [[ $name == "$gone" ]] && continue
        rest+=$(printf '%s %s %s' "$name" "$ready" "$down")$'\n'
    done < "$hosts_file"
    rest+=$(printf '%s true 0' "$replacement")$'\n'
    printf '%s' "$rest" > "$hosts_file"
    store_rows "$replacement"
    printf '%s replaced by %s\n' "$gone" "$replacement"
}

# --- one VM ------------------------------------------------------------------
# host, state and checkpoint are what `list` reports; wseed and wstep are what a
# witness in that guest holds, and cseed and cstep what its last checkpoint
# published, which is what a host loss rewinds it to.
#
# resident is whether a witness process is in that guest. A cold start discards
# the guest's memory and boots its kernel, so the witness and the buffer it held
# are gone and only the file on the disk is left: a check of memory is answered
# the way a socket nothing is listening on is, and --disk-only is answered out
# of the model as before. memory and disk are the shape a cold start gave the
# VM, and grown whether the guest has taken the pages the volume gained.

#
# file and cfile are the one line of /tmp/witness, which demo-fixes.sh writes
# into a guest after its last checkpoint and reads back after a rollout: it is
# in the guest's memory and nowhere else, so a migration carries it and a host
# loss rewinds it to what the last checkpoint published, like everything else.

vm_host=''; vm_state=''; vm_checkpoint=''
vm_wseed=''; vm_wstep=''; vm_cseed=''; vm_cstep=''
vm_resident=1; vm_memory=''; vm_disk=''; vm_grown=1
vm_file=''; vm_cfile=''

load_vm() {
    [[ -f $vms/$1 ]] || { printf 'no VM named %s\n' "$1" >&2; exit 1; }
    # shellcheck disable=SC1090 # the model's own record, written by save_vm
    . "$vms/$1"
}

save_vm() {
    printf 'vm_host=%s\nvm_state=%s\nvm_checkpoint=%s\nvm_wseed=%s\nvm_wstep=%s\nvm_cseed=%s\nvm_cstep=%s\nvm_resident=%s\nvm_memory=%s\nvm_disk=%s\nvm_grown=%s\nvm_file=%s\nvm_cfile=%s\n' \
        "$vm_host" "$vm_state" "$vm_checkpoint" "$vm_wseed" "$vm_wstep" "$vm_cseed" "$vm_cstep" \
        "$vm_resident" "$vm_memory" "$vm_disk" "$vm_grown" "$vm_file" "$vm_cfile" \
        > "$vms/$1"
}

# order prints every VM in the order it was created, which is the order a
# control plane's own table would keep them in.
order() {
    [[ -f $state/order ]] || return 0
    cat "$state/order"
}

# --- the object store --------------------------------------------------------
# One counter per host and operation, so that two readings subtracted are what
# the work between them cost.

# store_rows gives one host a counter per operation. A pod that is replaced
# leaves its own behind: what it served is what the run cost up to the moment it
# went, and a table that dropped it would lose that.
store_rows() {
    local op
    for op in head get put delete list; do
        printf '%s %s 0 0 0\n' "$1" "$op" >> "$store_file"
    done
}

store_file=$state/store
if [[ ! -f $store_file ]]; then
    store_rows sproutfs-host-0
    store_rows sproutfs-host-1
fi

# charge adds calls and bytes to one host's counter for one operation.
charge() {
    local want_host=$1 want_op=$2 calls=$3 bytes=$4
    local host op c f b rest=''
    while read -r host op c f b; do
        if [[ $host == "$want_host" && $op == "$want_op" ]]; then
            c=$((c + calls)); b=$((b + bytes))
        fi
        rest+=$(printf '%s %s %s %s %s' "$host" "$op" "$c" "$f" "$b")$'\n'
    done < "$store_file"
    printf '%s' "$rest" > "$store_file"
}

# --- printing ----------------------------------------------------------------
# tabulate lays out rows whose fields are separated by unit separators the way
# text/tabwriter does with a padding of two, which is what the CLI prints
# through and what cmd/sproutfsctl's output tests pin.

tabulate() {
    awk -F'\037' '
        { rows = NR; if (NF > columns) columns = NF
          for (i = 1; i <= NF; i++) {
              cell[NR "," i] = $i
              if (length($i) > width[i]) width[i] = length($i) } }
        END {
            for (r = 1; r <= rows; r++) {
                line = ""
                for (i = 1; i <= columns; i++) {
                    if (i < columns)
                        line = line sprintf("%-" (width[i] + 2) "s", cell[r "," i])
                    else
                        line = line cell[r "," i]
                }
                print line
            }
        }'
}

row() {
    local out='' field
    for field in "$@"; do
        [[ -z $out ]] && out=$field || out=$out$'\037'$field
    done
    printf '%s\n' "$out"
}

# --- the witness in a guest ---------------------------------------------------
# What a guest answers is the model's own (seed, step) for that VM, so a script
# whose expectation has drifted is told the same thing a real guest would tell
# it: the first offset that is not what it asked for.

witness() {
    local vm=$1; shift
    local verb=$1; shift
    # A grow is the one verb whose argument is a mount point rather than a flag.
    # It gives the filesystem the pages a cold start's larger volume gained,
    # through an ioctl on that mount point: it opens no device, which is what
    # lets it grow a root filesystem that is mounted. A filesystem that already
    # fills its device has nothing to take and says so rather than failing.
    if [[ $verb == grow ]]; then
        local point=${1:-}
        [[ -n $point ]] ||
            { printf 'sproutfs-guest-witness: grow takes one argument, the mount point to grow\n' >&2; exit 1; }
        load_vm "$vm"
        if [[ -z $vm_disk ]]; then
            printf '%s already fills /dev/pmem0\n' "$point"
            return 0
        fi
        vm_grown=1
        save_vm "$vm"
        note "witness $vm grow $point $vm_disk"
        printf '%s grew to %s on /dev/pmem0\n' "$point" "$vm_disk"
        return 0
    fi
    local seed='' step='' disk_only=0 argument
    while (($# > 0)); do
        argument=$1; shift
        case $argument in
            --seed) seed=$1; shift ;;
            --step) step=$1; shift ;;
            --disk-only) disk_only=1 ;;
            --memory | --disk | --socket | --log) shift ;;
            *) printf 'sproutfs-guest-witness: no flag named %s\n' "$argument" >&2; exit 1 ;;
        esac
    done
    load_vm "$vm"
    # Everything but a disk-only check goes through the resident witness, and a
    # cold start left none: the memory it lived in was discarded and /run, which
    # is a tmpfs, was cleared by the boot.
    if ((vm_resident == 0)) && ! { [[ $verb == check ]] && ((disk_only)); } && [[ $verb != fill ]]; then
        printf 'sproutfs-guest-witness: reaching the witness at /run/sproutfs-witness.sock: connect: connection refused\n' >&2
        printf '%s exited 1 on %s\n' "$vm" "$vm_host" >&2
        exit 1
    fi
    case $verb in
        fill)
            vm_wseed=$seed; vm_wstep=0; vm_resident=1; save_vm "$vm"
            note "witness $vm fill $seed"
            printf 'filled 268435456 bytes with seed %s\n' "$seed"
            ;;
        mutate)
            if ((step != vm_wstep + 1)); then
                printf 'sproutfs-guest-witness: step %s does not follow %s\n' "$step" "$vm_wstep" >&2
                exit 1
            fi
            vm_wstep=$step; save_vm "$vm"
            note "witness $vm mutate $step"
            printf 'rewrote 8192 of 65536 pages at step %s\n' "$step"
            ;;
        check)
            local nth
            nth=$(counter witness-check)
            if ((disk_only)); then
                note "witness $vm check-disk $seed $step"
            else
                note "witness $vm check $seed $step"
            fi
            # A volume a cold start grew is a volume with pages the filesystem
            # has not taken yet, and the witness file is not readable past the
            # end of the filesystem that holds it.
            if ((vm_grown == 0)); then
                printf 'sproutfs-guest-witness: reading the witness file at 0: input/output error\n' >&2
                printf '%s exited 1 on %s\n' "$vm" "$vm_host" >&2
                exit 1
            fi
            if [[ -n ${SPROUTFS_FAKE_TIMEOUT:-} && $nth == "${SPROUTFS_FAKE_TIMEOUT}" ]]; then
                printf 'sproutfs-guest-agent: the command ran past its timeout\n' >&2
                printf '%s exited 124 on %s\n' "$vm" "$vm_host" >&2
                exit 1
            fi
            if [[ -n ${SPROUTFS_FAKE_CORRUPT:-} && $nth == "${SPROUTFS_FAKE_CORRUPT}" ]]; then
                printf 'sproutfs-guest-witness: memory differs at offset 8192: it holds 0x11 and (seed %s, step %s) is 0x22\n' \
                    "$seed" "$step" >&2
                printf '%s exited 1 on %s\n' "$vm" "$vm_host" >&2
                exit 1
            fi
            local where=memory
            ((disk_only)) && where=disk
            if [[ $seed != "$vm_wseed" || $step != "$vm_wstep" ]]; then
                printf 'sproutfs-guest-witness: %s differs at offset 0: it holds (seed %s, step %s) and (seed %s, step %s) was asked for\n' \
                    "$where" "$vm_wseed" "$vm_wstep" "$seed" "$step" >&2
                printf '%s exited 1 on %s\n' "$vm" "$vm_host" >&2
                exit 1
            fi
            if ((disk_only)); then
                printf '268435456 bytes of disk hold (seed %s, step %s)\n' "$seed" "$step"
            else
                printf '268435456 bytes of memory and disk hold (seed %s, step %s)\n' "$seed" "$step"
            fi
            ;;
        *) printf 'sproutfs-guest-witness: no command named %s\n' "$verb" >&2; exit 1 ;;
    esac
}

# --- the commands ------------------------------------------------------------

command=${1:?"a command"}; shift
target=
case $command in
    exec | fork | migrate | capture | kill-host | recover | stop | start | delete | console)
        target=${1:?"$command needs a target"}; shift ;;
esac
note "$command ${target:-} $*"

case $command in
create)
    template=alpine
    while (($# > 0)); do
        case $1 in
            --template) template=$2; shift 2 ;;
            *) shift ;;
        esac
    done
    nth=$(counter create)
    refused create "$nth" && refuse create
    host=$(a_ready_host)
    vm=vm-$nth
    vm_host=$host vm_state=running vm_checkpoint=1 vm_wseed=0 vm_wstep=0 vm_cseed=0 vm_cstep=0
    save_vm "$vm"
    printf '%s\n' "$vm" >> "$state/order"
    charge "$host" put 4 4194304
    note "create $vm $host $template"
    printf '%s on %s (template 4.00s, fork 0.50s, boot 1.25s, root 0.75s, total 6.50s)\n' "$vm" "$host"
    ;;
list)
    {
        row VM HOST STATE CHECKPOINT
        for vm in $(order); do
            load_vm "$vm"
            host=$vm_host
            [[ -n $host ]] || host=-
            row "$vm" "$host" "$vm_state" "$vm_checkpoint"
        done
    } | tabulate
    ;;
hosts)
    tick_hosts
    {
        row HOST READY RUNNING SERVING RESIDENT SHARED SERVED STATE
        while read -r name ready down; do
            running=0
            for vm in $(order); do
                load_vm "$vm"
                [[ $vm_host == "$name" ]] && running=$((running + 1))
            done
            row "$name" "$ready" "$running" 0 900 512 64 ok
        done < "$hosts_file"
    } | tabulate
    ;;
store)
    {
        row HOST OP CALLS FAILED BYTES
        while read -r host op calls failures bytes; do
            row "$host" "$op" "$calls" "$failures" "$bytes"
        done < "$store_file"
    } | tabulate
    ;;
fork)
    count=1 to=
    while (($# > 0)); do
        case $1 in
            --count) count=$2; shift 2 ;;
            --to) to=$2; shift 2 ;;
            *) shift ;;
        esac
    done
    nth=$(counter fork)
    refused fork "$nth" && refuse fork
    load_vm "$target"
    parent_seed=$vm_wseed parent_step=$vm_wstep
    [[ -n $to ]] || to=$vm_host
    host_ready "$to" || refuse fork
    # The children are made first and printed afterwards: what is written down
    # has to happen in this shell, and the table goes through a pipeline.
    children=''
    index=0
    while ((index < count)); do
        index=$((index + 1))
        child=vm-$(counter create)
        vm_host=$to vm_state=running vm_checkpoint=1
        vm_wseed=$parent_seed vm_wstep=$parent_step
        vm_cseed=$parent_seed vm_cstep=$parent_step
        save_vm "$child"
        printf '%s\n' "$child" >> "$state/order"
        children=$children$child$'\n'
    done
    {
        row FORK HOST PAUSE START TOTAL
        while read -r child; do
            [[ -n $child ]] || continue
            row "$child" "$to" 0.012 0.300 0.316
        done <<< "$children"
    } | tabulate
    charge "$to" get 8 2097152
    ;;
migrate)
    to=
    while (($# > 0)); do
        case $1 in
            --to) to=$2; shift 2 ;;
            *) shift ;;
        esac
    done
    nth=$(counter migrate)
    refused migrate "$nth" && refuse migrate
    load_vm "$target"
    from=$vm_host
    [[ -n $from ]] || { printf 'migrate: no host runs %s\n' "$target" >&2; exit 1; }
    host_ready "$to" || refuse migrate
    vm_host=$to; save_vm "$target"
    charge "$from" put 6 8388608
    charge "$to" get 6 8388608
    printf '%s moved from %s to %s: pause 0.082s, stream 1.400s, 96 pages from the source, 12 unpublished\n' \
        "$target" "$from" "$to"
    ;;
capture)
    nth=$(counter capture)
    refused capture "$nth" && refuse capture
    load_vm "$target"
    [[ -n $vm_host ]] || { printf 'capture: no host runs %s\n' "$target" >&2; exit 1; }
    vm_checkpoint=$((vm_checkpoint + 1))
    vm_cseed=$vm_wseed vm_cstep=$vm_wstep vm_cfile=$vm_file
    save_vm "$target"
    charge "$vm_host" put 3 1048576
    printf '%s checkpoint %s on %s: pause 0.010s, publish 0.250s\n' "$target" "$vm_checkpoint" "$vm_host"
    ;;
replace-hosts)
    # Not a sproutfsctl command: it is what a rollout of the host Deployment
    # does, and scripts/test/fake-kubectl.sh is the only thing that asks for it.
    # One pod at a time, each drained before it goes and each replaced by a pod
    # with a name of its own.
    rolling=$(awk '{ print $1 }' "$hosts_file")
    for host in $rolling; do
        replace_host "$host"
    done
    ;;
kill-host)
    # A pod the cluster no longer lists runs nothing, and the orchestrator's
    # survey clears the rows that named it rather than waiting to miss the pod:
    # a `list` straight after a kill shows its VMs with no host at all.
    kill_host "$target"
    for vm in $(order); do
        load_vm "$vm"
        [[ $vm_host == "$target" ]] || continue
        # Everything the guest did since its last checkpoint went with the host.
        vm_host='' vm_state=lost vm_wseed=$vm_cseed vm_wstep=$vm_cstep vm_file=$vm_cfile
        save_vm "$vm"
    done
    printf 'killed %s\n' "$target"
    ;;
recover)
    load_vm "$target"
    # A host's refusal, relayed as the refusal it was.
    [[ -z $vm_host ]] ||
        { printf 'recover: a live host still runs that VM: %s runs %s\n' "$vm_host" "$target" >&2; exit 1; }
    host=$(a_ready_host)
    vm_host=$host vm_state=running
    save_vm "$target"
    charge "$host" get 12 16777216
    printf '%s reopened on %s from checkpoint %s in 2.500s\n' "$target" "$host" "$vm_checkpoint"
    ;;
stop)
    nth=$(counter stop)
    refused stop "$nth" && refuse stop
    load_vm "$target"
    [[ -n $vm_host ]] || { printf 'stop: no host runs %s\n' "$target" >&2; exit 1; }
    was=$vm_host
    vm_checkpoint=$((vm_checkpoint + 1))
    vm_cseed=$vm_wseed vm_cstep=$vm_wstep vm_cfile=$vm_file
    vm_host='' vm_state=stopped
    save_vm "$target"
    charge "$was" put 5 4194304
    printf 'stopped %s on %s at checkpoint %s in 0.420s\n' "$target" "$was" "$vm_checkpoint"
    ;;
start)
    to='' cold=0 memory='' disk=''
    while (($# > 0)); do
        case $1 in
            --to) to=$2; shift 2 ;;
            --cold) cold=1; shift ;;
            --memory) memory=$2; shift 2 ;;
            --disk) disk=$2; shift 2 ;;
            *) shift ;;
        esac
    done
    # A VM that comes back where it was comes back at the shape its memory
    # describes, so there is nothing to resize: the CLI refuses this where it is
    # typed, and so does this.
    if ((cold == 0)) && [[ -n $memory || -n $disk ]]; then
        printf 'usage: --memory and --disk need --cold, which is the one moment a VM'"'"'s shape can change\n' >&2
        exit 2
    fi
    nth=$(counter start)
    refused start "$nth" && refuse start
    load_vm "$target"
    # A start is not a takeover: a VM a host runs is refused, and the host's own
    # 409 is what reaches here rather than a failure of the orchestrator's.
    [[ -z $vm_host ]] ||
        { printf 'start: a live host still runs that VM: %s runs %s\n' "$vm_host" "$target" >&2; exit 1; }
    [[ -n $to ]] || to=$(a_ready_host)
    host_ready "$to" || refuse start
    vm_host=$to vm_state=running
    if ((cold)); then
        # The memory and the witness that lived in it are discarded and the
        # kernel is booted; the disk is exactly what the last checkpoint
        # published, which is what the stop published.
        vm_checkpoint=$((vm_checkpoint + 1))
        vm_resident=0 vm_wseed=$vm_cseed vm_wstep=$vm_cstep vm_file=$vm_cfile
        [[ -n $memory ]] && vm_memory=$memory
        # A grown volume's new pages read as zeroes and the filesystem has not
        # taken them yet: the guest's own `witness grow` is what does that.
        [[ -n $disk ]] && { vm_disk=$disk; vm_grown=0; }
    fi
    save_vm "$target"
    charge "$to" get 5 4194304
    if ((cold)); then
        printf '%s cold started on %s from checkpoint %s in 1.250s\n' "$target" "$to" "$vm_checkpoint"
    else
        printf '%s started on %s from checkpoint %s in 1.250s\n' "$target" "$to" "$vm_checkpoint"
    fi
    ;;
delete)
    load_vm "$target"
    rm -f "$vms/$target"
    awk -v gone="$target" '$0 != gone' "$state/order" > "$state/order.next"
    mv "$state/order.next" "$state/order"
    printf 'deleted %s\n' "$target"
    ;;
check)
    if [[ -n ${SPROUTFS_FAKE_VIOLATIONS:-} ]]; then
        printf '[violation] demo/vm/vm-a/ckpt/7/index: the index names a part that is not there\n'
        printf 'the deployment disagrees with itself: 1 violations\n' >&2
        exit 1
    fi
    printf "the deployment's durable state agrees with itself\n"
    ;;
exec)
    while (($# > 0)); do
        case $1 in
            --timeout) shift 2 ;;
            --) shift; break ;;
            *) shift ;;
        esac
    done
    load_vm "$target"
    [[ -n $vm_host ]] || { printf 'exec: no host runs %s\n' "$target" >&2; exit 1; }
    case ${1:-} in
        true) ;;
        'echo '*' > /tmp/witness')
            # The one line demo-fixes.sh writes into a guest after its last
            # checkpoint, which only the guest's own memory carries.
            vm_file=${1#echo }
            vm_file=${vm_file% > /tmp/witness}
            save_vm "$target"
            note "witness-file $target write $vm_file"
            ;;
        'cat /tmp/witness')
            [[ -n $vm_file ]] || {
                printf 'cat: /tmp/witness: No such file or directory\n' >&2
                printf '%s exited 1 on %s\n' "$target" "$vm_host" >&2
                exit 1
            }
            note "witness-file $target read $vm_file"
            printf '%s\n' "$vm_file"
            ;;
        */sproutfs-guest-witness) shift; witness "$target" "$@" ;;
        *) printf 'the model runs no command named %s\n' "${1:-}" >&2; exit 1 ;;
    esac
    ;;
*)
    printf 'no command named %s\n' "$command" >&2
    exit 2
    ;;
esac
