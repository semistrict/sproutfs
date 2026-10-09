#!/usr/bin/env bash
# The vmmemory suite's Linux tests against real UFFD, with the crate's example
# client as the VMM, on a disposable GCE VM: what scripts/test-vm-memory-lima.sh
# runs, without Lima. `all` always deletes its VM.
#
#   create   the VM, deleted by GCE itself after three hours whatever happens
#   run      stage the source, build the client and the suite, run it, copy the log
#   delete   the VM and its disk, verified
#   all      every step in order, and delete whatever happens
#
# SPROUTFS_VM_MEMORY_RUN selects the tests by -test.run pattern; unset runs the
# whole suite. SPROUTFS_ARENA is the arena mode, shared when unset, and
# SPROUTFS_VM_MEMORY_MACHINE (c3-standard-8) the VM, and
# SPROUTFS_VM_MEMORY_HUGEPAGES (4096) the 2 MiB HugeTLB pool the run reserves on
# it, which the suite's 2 MiB pagers take their arenas from. A worktree whose
# Firecracker submodule is not checked out names a clone of the fork in
# SPROUTFS_FIRECRACKER_TREE (scripts/lib/stage-source.py).
#
#   scripts/test-vm-memory-gce.sh all [instance] [results-directory]
set -euo pipefail
run=${SPROUTFS_VM_MEMORY_RUN:-}
arena=${SPROUTFS_ARENA:-}
machine=${SPROUTFS_VM_MEMORY_MACHINE:-c3-standard-8}
hugepages=${SPROUTFS_VM_MEMORY_HUGEPAGES:-4096}
[[ $machine =~ ^[a-z0-9]+-[a-z]+-[0-9]+$ ]] || { echo "SPROUTFS_VM_MEMORY_MACHINE is a machine type" >&2; exit 2; }
[[ $hugepages =~ ^[0-9]+$ ]] || { echo "SPROUTFS_VM_MEMORY_HUGEPAGES is a number of 2 MiB pages" >&2; exit 2; }
[[ -z $arena || $arena == isolated || $arena == shared ]] || { echo "SPROUTFS_ARENA is isolated or shared" >&2; exit 2; }
[[ $run != *"'"* ]] || { echo "SPROUTFS_VM_MEMORY_RUN holds no single quote" >&2; exit 2; }

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
project=${SPROUTFS_GCE_PROJECT:-$(gcloud config get-value project 2>/dev/null)}
zone=${SPROUTFS_GCE_ZONE:-us-east4-a}
action=${1:-all}
instance=${2:-sproutfs-vmmem-$(date -u +%Y%m%d-%H%M%S)}
results=${3:-$(mktemp -d "${TMPDIR:-/tmp}/sproutfs-vm-memory-gce.XXXXXX")}
[[ -n "$project" && "$project" != '(unset)' ]] || { echo 'Set SPROUTFS_GCE_PROJECT.' >&2; exit 2; }
[[ "$instance" =~ ^sproutfs-vmmem-[a-z0-9-]+$ ]] || { echo 'Use a sproutfs-vmmem- instance name.' >&2; exit 2; }
cloud=(gcloud --quiet --project="$project")

# create makes the VM in SPROUTFS_GCE_ZONE or, where that zone has no room for
# its machine, in the next zone of the same region that does, as
# scripts/soak-pager-gce.sh does.
create() {
    local region candidates candidate log
    region=${zone%-*}
    candidates=("$zone")
    while read -r candidate; do
        [[ "$candidate" == "$zone" ]] || candidates+=("$candidate")
    done < <("${cloud[@]}" compute zones list --filter="region:$region" --format='value(name)' | sort)
    log=$(mktemp)
    for candidate in "${candidates[@]}"; do
        if create_in "$candidate" 2> "$log"; then
            zone=$candidate
            echo "Created $instance in $zone." >&2
            rm -f -- "$log"
            return 0
        fi
        if ! [[ $(< "$log") =~ ZONE_RESOURCE_POOL_EXHAUSTED|STOCKOUT|does\ not\ have\ enough\ resources ]]; then
            cat "$log" >&2
            rm -f -- "$log"
            return 1
        fi
        echo "$candidate has no room for this machine; trying the next zone." >&2
    done
    cat "$log" >&2
    rm -f -- "$log"
    return 1
}

create_in() {
    "${cloud[@]}" compute instances create "$instance" --zone="$1" \
        --machine-type="$machine" \
        --image=ubuntu-2604-resolute-amd64-v20260907 --image-project=ubuntu-os-cloud \
        --boot-disk-size=50GB --boot-disk-type=pd-balanced --boot-disk-auto-delete \
        --network="${SPROUTFS_GCE_NETWORK:-default}" --no-service-account --no-scopes \
        --metadata=block-project-ssh-keys=TRUE \
        --labels=purpose=vm-memory-test,lifecycle=temporary \
        --maintenance-policy=TERMINATE --no-restart-on-failure \
        --max-run-duration=3h --instance-termination-action=DELETE
}

# locate points zone at the zone the VM is in, which create may have chosen.
locate() {
    local found
    found=$("${cloud[@]}" compute instances list --filter="name=$instance" --format='value(zone.basename())')
    if [[ -n "$found" ]]; then zone=$found; fi
}

check_owner() {
    local labels
    labels=$("${cloud[@]}" compute instances describe "$instance" --zone="$zone" \
        --format='value(labels.purpose,labels.lifecycle)')
    [[ "$labels" == $'vm-memory-test\ttemporary' ]] || {
        echo "Refusing to operate on an instance without the test's ownership labels." >&2
        return 1
    }
}

delete() {
    local found
    found=$("${cloud[@]}" compute instances list --filter="name=$instance" --format='value(name)')
    if [[ -n "$found" ]]; then
        check_owner
        "${cloud[@]}" compute instances delete "$instance" --zone="$zone" --delete-disks=all
    fi
    found=$("${cloud[@]}" compute instances list --filter="name=$instance" --format='value(name)')
    [[ -z "$found" ]] || { echo "Instance still exists: $instance" >&2; return 1; }
    found=$("${cloud[@]}" compute disks list --filter="name=$instance" --format='value(name)')
    [[ -z "$found" ]] || { echo "Boot disk still exists: $instance" >&2; return 1; }
    echo "Verified deletion of $instance and its boot disk." >&2
}

run() {
    check_owner
    mkdir -p "$results"
    results=$(cd "$results" && pwd)
    echo "Results go to $results." >&2
    local ready=false
    for _ in {1..30}; do
        if "${cloud[@]}" compute ssh "$instance" --zone="$zone" --ssh-flag='-o ConnectTimeout=10' \
            --command=true >> "$results/ssh.log" 2>&1; then
            ready=true
            break
        fi
        sleep 10
    done
    "$ready" || { echo 'The VM never answered.' >&2; return 1; }
    local staging status=0 quoted_run quoted_arena
    # The selection and the mode reach the remote shell quoted for it.
    quoted_run=$(printf %q "$run")
    quoted_arena=$(printf %q "$arena")
    staging=$(mktemp -d "${TMPDIR:-/tmp}/sproutfs-vm-memory-source.XXXXXX")
    python3 "$repo/scripts/lib/stage-source.py" "$repo" "$staging/repo.tar.gz"
    git -C "$repo" rev-parse HEAD > "$results/revision.txt"
    "${cloud[@]}" compute scp --zone="$zone" "$staging/repo.tar.gz" "$instance:repo.tar.gz"
    rm -rf -- "$staging"
    # shellcheck disable=SC2016  # The variables are the remote shell's.
    "${cloud[@]}" compute ssh "$instance" --zone="$zone" --command='set -euo pipefail
        sudo DEBIAN_FRONTEND=noninteractive apt-get update -qq
        sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq build-essential curl ca-certificates
        echo '"$hugepages"' | sudo tee /proc/sys/vm/nr_hugepages > /dev/null
        test "$(cat /proc/sys/vm/nr_hugepages)" = '"$hugepages"'
        curl -fsSL https://go.dev/dl/go1.26.6.linux-amd64.tar.gz -o go.tar.gz
        echo "708effb774be8237570d0add163225abbdfaf4fca28b2611df167beba4feef89  go.tar.gz" | sha256sum -c -
        curl -fsSL https://sh.rustup.rs -o rustup.sh
        sh rustup.sh -y --profile minimal > /dev/null
        rm -rf sdk source results && mkdir -p sdk source results
        tar -xzf go.tar.gz -C sdk
        tar -xzf repo.tar.gz -C source
        export PATH="$HOME/sdk/go/bin:$HOME/.cargo/bin:$PATH"
        cd source/repo
        cargo build --locked --manifest-path rust/sproutfs-vm-memory/Cargo.toml --examples
        go test -c -o "$HOME/vmmemory.test" ./vmmemory
        client=$PWD/rust/sproutfs-vm-memory/target/debug/examples/client
        cd vmmemory
        { nproc; uname -r; go version; cargo --version; } > "$HOME/results/environment.txt"
        sudo env SPROUTFS_ARENA='"$quoted_arena"' SPROUTFS_VM_MEMORY_CLIENT="$client" \
            "$HOME/vmmemory.test" -test.v -test.timeout=30m -test.run='"$quoted_run"' > "$HOME/results/test.log" 2>&1' \
        > "$results/remote.log" 2>&1 || status=$?
    "${cloud[@]}" compute scp --recurse --zone="$zone" "$instance:results/." "$results/" || status=$?
    return "$status"
}

# finish deletes the VM as the script exits, keeping the run's exit status.
finish() {
    local code=$?
    delete || code=1
    exit "$code"
}

case "$action" in
    create) create ;;
    run) locate; run ;;
    delete) locate; delete ;;
    all)
        # The VM goes whatever happens, and the run's own failure is the
        # script's, not the deletion's success.
        trap finish EXIT
        create
        run
        ;;
    *) echo "unknown action: $action" >&2; exit 2 ;;
esac
