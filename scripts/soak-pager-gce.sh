#!/usr/bin/env bash
# The pager's unscheduled soak (TestThePagersCampaignsSoakWithoutAScheduler in
# vmmemory/soak_test.go) under -race on a disposable many-core GCE VM. The
# vmmemory suite runs once per page, so each arena mode soaks every world for
# SPROUTFS_PAGER_SOAK at 4 KiB and again at 2 MiB, isolated first, then shared.
# `all` always deletes its VM.
#
#   create   the VM, deleted by GCE itself after six hours whatever happens
#   run      stage the source, build the race-enabled test, soak, copy the logs
#   delete   the VM and its disk, verified
#   all      every step in order, and delete whatever happens
#
# SPROUTFS_PAGER_SOAK (15m) is each soak's length, SPROUTFS_SOAK_MACHINE
# (c3-standard-22, within a project's default quota of 32 processors beside
# another benchmark host) the VM, and SPROUTFS_PAGER_SOAK_WORLDS and
# SPROUTFS_PAGER_SOAK_SEED are passed through to the test. A worktree whose
# Firecracker submodule is not checked out names a clone of the fork in
# SPROUTFS_FIRECRACKER_TREE (scripts/lib/stage-source.py).
#
#   scripts/soak-pager-gce.sh all [instance] [results-directory]
set -euo pipefail
soak=${SPROUTFS_PAGER_SOAK:-15m}
machine=${SPROUTFS_SOAK_MACHINE:-c3-standard-22}
worlds=${SPROUTFS_PAGER_SOAK_WORLDS:-}
seed=${SPROUTFS_PAGER_SOAK_SEED:-}
[[ $soak =~ ^[0-9]+[smh]$ ]] || { echo "SPROUTFS_PAGER_SOAK is a duration such as 15m" >&2; exit 2; }
[[ $machine =~ ^[a-z0-9]+-[a-z]+-[0-9]+$ ]] || { echo "SPROUTFS_SOAK_MACHINE is a machine type" >&2; exit 2; }
[[ $worlds =~ ^[a-z,]*$ ]] || { echo "SPROUTFS_PAGER_SOAK_WORLDS names worlds, separated by commas" >&2; exit 2; }
[[ $seed =~ ^[0-9]*$ ]] || { echo "SPROUTFS_PAGER_SOAK_SEED is a number" >&2; exit 2; }

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
project=${SPROUTFS_GCE_PROJECT:-$(gcloud config get-value project 2>/dev/null)}
zone=${SPROUTFS_GCE_ZONE:-us-east4-a}
action=${1:-all}
instance=${2:-sproutfs-soak-$(date -u +%Y%m%d-%H%M%S)}
results=${3:-$repo/docs/measurements/gce-pager-soak-$(date -u +%Y-%m-%d)}
[[ -n "$project" && "$project" != '(unset)' ]] || { echo 'Set SPROUTFS_GCE_PROJECT.' >&2; exit 2; }
[[ "$instance" =~ ^sproutfs-soak-[a-z0-9-]+$ ]] || { echo 'Use a sproutfs-soak- instance name.' >&2; exit 2; }
cloud=(gcloud --quiet --project="$project")

# create makes the VM in SPROUTFS_GCE_ZONE or, where that zone has no room for
# its machine, in the next zone of the same region that does, as
# scripts/bench-memory-gce.sh does.
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
        --labels=purpose=pager-soak,lifecycle=temporary \
        --maintenance-policy=TERMINATE --no-restart-on-failure \
        --max-run-duration=6h --instance-termination-action=DELETE
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
    [[ "$labels" == $'pager-soak\ttemporary' ]] || {
        echo "Refusing to operate on an instance without the soak's ownership labels." >&2
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
    local ready=false
    for _ in {1..30}; do
        if "${cloud[@]}" compute ssh "$instance" --zone="$zone" --ssh-flag='-o ConnectTimeout=10' \
            --command=true >> "$results/ssh.log" 2>&1; then
            ready=true
            break
        fi
        sleep 10
    done
    "$ready" || { echo 'The soak VM never answered.' >&2; return 1; }
    local staging status=0
    staging=$(mktemp -d /tmp/sproutfs-soak-source.XXXXXX)
    python3 "$repo/scripts/lib/stage-source.py" "$repo" "$staging/repo.tar.gz"
    git -C "$repo" rev-parse HEAD > "$results/revision.txt"
    "${cloud[@]}" compute scp --zone="$zone" "$staging/repo.tar.gz" "$instance:repo.tar.gz"
    rm -rf -- "$staging"
    # shellcheck disable=SC2016  # The variables are the remote shell's.
    "${cloud[@]}" compute ssh "$instance" --zone="$zone" --command='set -euo pipefail
        sudo DEBIAN_FRONTEND=noninteractive apt-get update -qq
        sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq build-essential curl ca-certificates
        curl -fsSL https://go.dev/dl/go1.26.6.linux-amd64.tar.gz -o go.tar.gz
        echo "708effb774be8237570d0add163225abbdfaf4fca28b2611df167beba4feef89  go.tar.gz" | sha256sum -c -
        rm -rf go source results && mkdir -p source results
        tar -xzf go.tar.gz
        tar -xzf repo.tar.gz -C source
        export PATH="$HOME/go/bin:$PATH"
        cd source/repo
        go test -race -c -o "$HOME/vmmemory.test" ./vmmemory
        cd vmmemory
        { nproc; lscpu | grep "Model name"; go version; } > "$HOME/results/environment.txt"
        status=0
        for arena in isolated shared; do
            SPROUTFS_ARENA=$arena SPROUTFS_PAGER_SOAK='"$soak"' SPROUTFS_PAGER_SOAK_WORLDS='"$worlds"' \
                SPROUTFS_PAGER_SOAK_SEED='"$seed"' "$HOME/vmmemory.test" -test.v -test.timeout=6h \
                -test.run "^TestThePagersCampaignsSoakWithoutAScheduler$" > "$HOME/results/soak-$arena.log" 2>&1 ||
                status=$?
        done
        exit "$status"' > "$results/remote.log" 2>&1 || status=$?
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
