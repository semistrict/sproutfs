#!/usr/bin/env bash
# One publication of a guest's memory, several GiB of it, from one disposable
# GCE host to a Cloud Storage bucket, with the release before and with this
# release (docs/measurements/gce-publication-throughput-2026-10-04.md).
#
# cmd/sproutfs-publishbench holds the guest in memory and publishes it
# through the real checkpoint store and Cloud Storage adapter, timing the
# commit, the CPU it used, every PUT and the uploads in flight, with a CPU
# profile of every round. Each case runs once a round, in an order that
# alternates by round, for SPROUTFS_PUBLISH_ROUNDS rounds (three by default),
# each publishing SPROUTFS_PUBLISH_GIB GiB (four by default). The cases are
# each build with the store sized as a host on the machine sizes it, and
# each with SPROUTFS_PUBLISH_WIDE_ENCODERS encoders (four by default).
#
# The release before is main at 6a01a213, built with this release's bench,
# which uses only the store's public API.
#
# SPROUTFS_GCE_BUCKET names the bucket and SPROUTFS_GCE_SERVICE_ACCOUNT the
# account the host reaches it as. `all` always deletes the host and the run's
# objects. `create`, `run` and `delete` expose the same steps. The host also
# deletes itself after two hours, whatever this shell does.
# SPROUTFS_PUBLISH_MACHINE is the machine type (default n2-highmem-4).
#
#   scripts/bench-publish-gce.sh all [name-prefix] [results-directory]
set -euo pipefail
[[ ${SPROUTFS_PUBLISH_ROUNDS:-} =~ ^[0-9]*$ ]] || { echo "SPROUTFS_PUBLISH_ROUNDS is a number" >&2; exit 2; }
[[ ${SPROUTFS_PUBLISH_GIB:-} =~ ^[0-9]*$ ]] || { echo "SPROUTFS_PUBLISH_GIB is a number" >&2; exit 2; }
[[ ${SPROUTFS_PUBLISH_WIDE_ENCODERS:-} =~ ^[0-9]*$ ]] || { echo "SPROUTFS_PUBLISH_WIDE_ENCODERS is a number" >&2; exit 2; }
rounds=${SPROUTFS_PUBLISH_ROUNDS:-3}
gib=${SPROUTFS_PUBLISH_GIB:-4}
wide=${SPROUTFS_PUBLISH_WIDE_ENCODERS:-4}
machine=${SPROUTFS_PUBLISH_MACHINE:-n2-highmem-4}
[[ $machine =~ ^n2-(standard|highmem)-[0-9]+$ ]] || { echo "SPROUTFS_PUBLISH_MACHINE is an n2 machine type" >&2; exit 2; }
bucket=${SPROUTFS_GCE_BUCKET:-}
account=${SPROUTFS_GCE_SERVICE_ACCOUNT:-}
[[ $bucket =~ ^[a-z0-9._-]+$ && $account =~ ^[a-z0-9@._-]+$ ]] ||
    { echo "Set SPROUTFS_GCE_BUCKET and SPROUTFS_GCE_SERVICE_ACCOUNT." >&2; exit 2; }
before_revision=6a01a213

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
project=${SPROUTFS_GCE_PROJECT:-$(gcloud config get-value project 2>/dev/null)}
zone=${SPROUTFS_GCE_ZONE:-us-east4-a}
action=${1:-all}
prefix=${2:-sproutfs-publish-$(date -u +%Y%m%d-%H%M%S)}
results=${3:-$repo/docs/measurements/gce-publication-throughput-$(date -u +%Y-%m-%d)}
[[ -n "$project" && "$project" != '(unset)' ]] || { echo 'Set SPROUTFS_GCE_PROJECT.' >&2; exit 2; }
[[ "$prefix" =~ ^sproutfs-publish-[a-z0-9-]+$ ]] || { echo 'Use a sproutfs-publish- name prefix.' >&2; exit 2; }
cloud=(gcloud --quiet --project="$project")
host="$prefix-0"
objects="gs://$bucket/sproutfs-bench/$prefix"

create() {
    "${cloud[@]}" compute instances create "$host" --zone="$zone" \
        --machine-type="$machine" --min-cpu-platform='Intel Cascade Lake' \
        --image=ubuntu-2604-resolute-amd64-v20260907 --image-project=ubuntu-os-cloud \
        --boot-disk-size=20GB --boot-disk-type=pd-balanced --boot-disk-auto-delete \
        --network="${SPROUTFS_GCE_NETWORK:-default}" \
        --service-account="$account" --scopes=storage-rw \
        --metadata=block-project-ssh-keys=TRUE \
        --metadata-from-file="startup-script=$repo/scripts/lib/gce-bench-startup.sh" \
        --labels=purpose=publish-bench,lifecycle=temporary \
        --maintenance-policy=TERMINATE --no-restart-on-failure \
        --max-run-duration=2h --instance-termination-action=DELETE
}

existing() {
    "${cloud[@]}" compute instances list --filter="name ~ ^$prefix-[0-9]+\$" --format='value(name,zone.basename())'
}

locate() {
    local name found
    while read -r name found; do
        [[ -n $found ]] && zone=$found
    done < <(existing)
}

check_owner() {
    local labels
    labels=$("${cloud[@]}" compute instances describe "$1" --zone="$zone" \
        --format='value(labels.purpose,labels.lifecycle)')
    [[ "$labels" == $'publish-bench\ttemporary' ]] || {
        echo "Refusing to operate on $1, which lacks the benchmark ownership labels." >&2
        return 1
    }
}

delete() {
    local name where left where_of=""
    local -a names=()
    while read -r name where; do
        zone=$where check_owner "$name"
        names+=("$name")
        where_of=$where
    done < <(existing)
    if ((${#names[@]} > 0)); then
        "${cloud[@]}" compute instances delete "${names[@]}" --zone="$where_of" --delete-disks=all
    fi
    if "${cloud[@]}" storage ls "$objects/**" > /dev/null 2>&1; then
        "${cloud[@]}" storage rm --recursive "$objects"
    fi
    left=$(existing)
    [[ -z "$left" ]] || { echo "Hosts still exist: $left" >&2; return 1; }
    left=$("${cloud[@]}" compute disks list --filter="name ~ ^$prefix-[0-9]+\$" --format='value(name)')
    [[ -z "$left" ]] || { echo "Disks still exist: $left" >&2; return 1; }
    if "${cloud[@]}" storage ls "$objects/**" > /dev/null 2>&1; then
        echo "Objects still exist under $objects." >&2
        return 1
    fi
    echo "Verified deletion of every $prefix host, its disks and its objects." >&2
}

remote() {
    "${cloud[@]}" compute ssh "$host" --zone="$zone" --ssh-flag='-o ConnectTimeout=10' --command="$*" < /dev/null
}

# build makes this release's bench and the release before's, for
# linux/amd64, in staging.
build() {
    local staging=$1 before
    (cd "$repo" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$staging/publishbench-after" ./cmd/sproutfs-publishbench)
    before=$staging/before
    mkdir -p "$before"
    git -C "$repo" archive "$before_revision" | tar -x -C "$before"
    mkdir -p "$before/cmd/sproutfs-publishbench"
    cp "$repo"/cmd/sproutfs-publishbench/{main,guest}.go "$before/cmd/sproutfs-publishbench/"
    (cd "$before" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$staging/publishbench-before" ./cmd/sproutfs-publishbench)
}

run() {
    local ready=false status=0 staging round build encoders name
    check_owner "$host"
    mkdir -p "$results"
    results=$(cd "$results" && pwd)
    for _ in {1..30}; do
        if remote 'test -e /var/lib/sproutfs-bench/ready' >> "$results/startup.log" 2>&1; then
            ready=true
            break
        fi
        sleep 10
    done
    "$ready" || { echo "$host did not finish starting." >&2; return 1; }

    staging=$(mktemp -d "${TMPDIR:-/tmp}/sproutfs-publish.XXXXXX")
    build "$staging"
    {
        echo "revision $(git -C "$repo" rev-parse HEAD), before $before_revision"
        git -C "$repo" status --porcelain=v1 -- checkpoint internal/blob cmd/sproutfs-publishbench scripts | sed 's/^/changed /'
        (cd "$staging" && shasum -a 256 publishbench-after publishbench-before)
        echo "machine $machine, $gib GiB a publication, rounds $rounds, wide encoders $wide"
    } > "$results/source.txt"
    "${cloud[@]}" compute instances describe "$host" --zone="$zone" \
        --format='json(name,zone,machineType,cpuPlatform)' > "$results/instance.json"
    "${cloud[@]}" compute scp --zone="$zone" "$staging/publishbench-after" "$staging/publishbench-before" \
        "$host:" >> "$results/remote.log" 2>&1
    rm -rf -- "$staging"

    for ((round = 0; round < rounds; round++)); do
        local -a order=(before after)
        ((round % 2 == 1)) && order=(after before)
        for build in "${order[@]}"; do
            for encoders in 0 "$wide"; do
                name=$build
                ((encoders != 0)) && name=$build-e$encoders
                echo "Round $round, $name." >&2
                remote "./publishbench-$build -bucket $bucket -prefix sproutfs-bench/$prefix -gib $gib" \
                    "-build $name -encoders $encoders -first-round $round -out rounds.jsonl -cpuprofile profiles" \
                    >> "$results/remote.log" 2>&1 || status=$?
            done
        done
    done
    "${cloud[@]}" compute scp --recurse --zone="$zone" "$host:rounds.jsonl" "$host:profiles" "$results/" \
        >> "$results/remote.log" 2>&1 || status=$?
    return "$status"
}

case "$action" in
    create) create ;;
    run) locate; run ;;
    delete) delete ;;
    all)
        [[ -z "$(existing)" ]] || { echo "Hosts named $prefix-* already exist." >&2; exit 1; }
        trap 'status=$?; trap - EXIT; if ! delete; then exit 1; fi; exit "$status"' EXIT
        create
        run
        ;;
    *) echo "Usage: $0 [all|create|run|delete] [name-prefix] [results-directory]" >&2; exit 2 ;;
esac
