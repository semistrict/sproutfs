#!/usr/bin/env bash
# Disposable nested-KVM memory measurements. `all` always deletes its VM.
# SPROUTFS_GCE_FANOUT=1 runs only the fork fan-out that lists the pages each
# fork came to own. SPROUTFS_GCE_QUALIFY=1 runs only the Linux qualification:
# the memory crate's own tests, the pager against real UFFD, and every
# Firecracker suite, on x86-64. SPROUTFS_GCE_BOOTSURVEY=1 runs only the boot survey of what
# a 4 KiB boot makes private. SPROUTFS_GCE_WORKLOAD=1 runs only the realistic comparison:
# the workload image through every scenario of the guest workload benchmark,
# managed and on plain Firecracker; with SPROUTFS_GCE_SMOKE=1 as well, everything
# but the build, in minutes, which is what to run first on a host `create` made.
# `create`, `run` and `delete` expose the same steps for interrupted runs.
#
# SPROUTFS_GCE_BUCKET names a Cloud Storage bucket the benchmarks keep their
# objects in instead of the host's disk, which is the deployment's object store
# rather than a model of one. Each run keeps them under a prefix of its own and
# deletes it once the results are back. The host reaches the bucket as
# SPROUTFS_GCE_SERVICE_ACCOUNT: `create` gives a new host that account, and
# `account` gives it to an existing one, which stops and starts it.
set -euo pipefail
case ${SPROUTFS_GCE_BUILD_ONLY:-0} in 0|1) ;; *) echo "SPROUTFS_GCE_BUILD_ONLY must be 0 or 1" >&2; exit 2 ;; esac
case ${SPROUTFS_GCE_FANOUT:-0} in 0|1) ;; *) echo "SPROUTFS_GCE_FANOUT must be 0 or 1" >&2; exit 2 ;; esac
case ${SPROUTFS_GCE_BOOTSURVEY:-0} in 0|1) ;; *) echo "SPROUTFS_GCE_BOOTSURVEY must be 0 or 1" >&2; exit 2 ;; esac
case ${SPROUTFS_GCE_QUALIFY:-0} in 0|1) ;; *) echo "SPROUTFS_GCE_QUALIFY must be 0 or 1" >&2; exit 2 ;; esac
case ${SPROUTFS_GCE_WORKLOAD:-0} in 0|1) ;; *) echo "SPROUTFS_GCE_WORKLOAD must be 0 or 1" >&2; exit 2 ;; esac
case ${SPROUTFS_GCE_SMOKE:-0} in 0|1) ;; *) echo "SPROUTFS_GCE_SMOKE must be 0 or 1" >&2; exit 2 ;; esac
# These cross a remote shell, so they are held to what a scenario list, a count
# and a number of bytes are made of.
[[ ${SPROUTFS_BENCH_SCENARIOS:-} =~ ^[a-z,-]*$ ]] || { echo "SPROUTFS_BENCH_SCENARIOS is a comma-separated list of scenario names" >&2; exit 2; }
[[ ${SPROUTFS_BENCH_FORKS:-} =~ ^[0-9]*$ ]] || { echo "SPROUTFS_BENCH_FORKS is a number" >&2; exit 2; }
[[ ${SPROUTFS_GCE_BUCKET:-} =~ ^[a-z0-9._-]*$ ]] || { echo "SPROUTFS_GCE_BUCKET is a bucket name" >&2; exit 2; }
[[ ${SPROUTFS_GCE_SERVICE_ACCOUNT:-} =~ ^[a-z0-9@._-]*$ ]] || { echo "SPROUTFS_GCE_SERVICE_ACCOUNT is a service account email" >&2; exit 2; }
for shape in SPROUTFS_BENCH_RAM_BYTES SPROUTFS_BENCH_ROOT_BYTES \
    SPROUTFS_BENCH_RAM_RESIDENT_BYTES SPROUTFS_BENCH_PMEM_RESIDENT_BYTES; do
    [[ ${!shape:-} =~ ^[0-9]*$ ]] || { echo "$shape is a number of bytes" >&2; exit 2; }
done
# SPROUTFS_RAM_PAGE_BYTES is the RAM pager's page: 4096 (the default) on
# ordinary memory, or 2097152 on the HugeTLB pool, which is the pager RAM ran
# before it had a page of its own.
case ${SPROUTFS_RAM_PAGE_BYTES:-} in ''|4096|2097152) ;; *) echo "SPROUTFS_RAM_PAGE_BYTES must be 4096 or 2097152" >&2; exit 2 ;; esac
repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
project=${SPROUTFS_GCE_PROJECT:-$(gcloud config get-value project 2>/dev/null)}
zone=${SPROUTFS_GCE_ZONE:-us-east4-a}
action=${1:-all}
instance=${2:-sproutfs-memprobe-$(date -u +%Y%m%d-%H%M%S)}
results=${3:-$repo/docs/measurements/gce-memory-$(date -u +%Y%m%d-%H%M%S)}
[[ -n "$project" && "$project" != '(unset)' ]] || { echo 'Set SPROUTFS_GCE_PROJECT.' >&2; exit 2; }
[[ "$instance" == sproutfs-memprobe-* ]] || { echo 'Use a sproutfs-memprobe- instance name.' >&2; exit 2; }
cloud=(gcloud --quiet --project="$project")
# Every host has eight processors. The workload comparison runs 16 GiB guests
# over a 32 GiB root each clone of the plain side copies, so it takes the
# high-memory shape of the same eight; the memory probe needs none of that.
machine=n2-standard-8 disk=80GB limit=5h
if [[ ${SPROUTFS_GCE_WORKLOAD:-0} == 1 ]]; then machine=n2-highmem-8 disk=400GB limit=11h; fi

# The host's identity: none unless it is to reach a bucket, and then the one
# account that bucket grants, able to reach storage and nothing else.
identity=(--no-service-account --no-scopes)
if [[ -n ${SPROUTFS_GCE_SERVICE_ACCOUNT:-} ]]; then
    identity=(--service-account="$SPROUTFS_GCE_SERVICE_ACCOUNT" --scopes=storage-rw)
fi

create() {
    "${cloud[@]}" compute instances create "$instance" --zone="$zone" \
        --machine-type="${SPROUTFS_GCE_MACHINE_TYPE:-$machine}" \
        --min-cpu-platform='Intel Cascade Lake' --enable-nested-virtualization \
        --image=ubuntu-2604-resolute-amd64-v20260907 --image-project=ubuntu-os-cloud \
        --boot-disk-size="$disk" --boot-disk-type=pd-balanced --boot-disk-auto-delete \
        --network="${SPROUTFS_GCE_NETWORK:-default}" "${identity[@]}" \
        --metadata=block-project-ssh-keys=TRUE \
        --metadata-from-file="startup-script=$repo/scripts/lib/gce-bench-startup.sh" \
        --labels=purpose=memory-probe,lifecycle=temporary \
        --maintenance-policy=TERMINATE --no-restart-on-failure \
        --max-run-duration=24h --instance-termination-action=DELETE
}

check_owner() {
    local labels
    labels=$("${cloud[@]}" compute instances describe "$instance" --zone="$zone" \
        --format='value(labels.purpose,labels.lifecycle)')
    [[ "$labels" == $'memory-probe\ttemporary' ]] || {
        echo "Refusing to operate on an instance without the benchmark ownership labels." >&2
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

account() {
    check_owner
    [[ -n ${SPROUTFS_GCE_SERVICE_ACCOUNT:-} ]] || { echo 'Set SPROUTFS_GCE_SERVICE_ACCOUNT.' >&2; return 1; }
    "${cloud[@]}" compute instances stop "$instance" --zone="$zone"
    "${cloud[@]}" compute instances set-service-account "$instance" --zone="$zone" "${identity[@]}"
    "${cloud[@]}" compute instances start "$instance" --zone="$zone"
}

run() {
    check_owner
    mkdir -p "$results"
    results=$(cd "$results" && pwd)
    local ready=false
    for _ in {1..30}; do
        if "${cloud[@]}" compute ssh "$instance" --zone="$zone" \
            --ssh-flag='-o ConnectTimeout=10' \
            --command='test -e /var/lib/sproutfs-bench/ready' \
            >> "$results/startup.log" 2>&1; then
            ready=true
            break
        fi
        sleep 10
    done
    "$ready" || { echo 'Benchmark host startup timed out.' >&2; return 1; }
    local staging
    staging=$(mktemp -d /tmp/sproutfs-gce-source.XXXXXX)
    # Transfer only versioned and non-ignored source, including the Firecracker
    # submodule, with no credentials, Git internals or build caches.
    python3 - "$repo" "$staging/source.tar.gz" <<'PY'
import io, pathlib, subprocess, sys, tarfile
root = pathlib.Path(sys.argv[1])
def files(directory):
    return subprocess.check_output(['git', '-C', str(directory), 'ls-files', '--cached', '--others', '--exclude-standard', '-z']).split(b'\0')
names = {name.decode() for name in files(root) if name}
submodule = 'third_party/firecracker'
names.update(submodule + '/' + name.decode() for name in files(root / submodule) if name)
revision = subprocess.check_output(['git', '-C', str(root), 'rev-parse', 'HEAD']).decode().strip() + '-working-tree'
with tarfile.open(sys.argv[2], 'w:gz') as archive:
    for name in sorted(names):
        path = root / name
        if path.is_file() or path.is_symlink():
            archive.add(path, arcname=name, recursive=False)
    data = (revision + '\n').encode()
    entry = tarfile.TarInfo('source-revision.txt')
    entry.size = len(data)
    archive.addfile(entry, io.BytesIO(data))
PY
    (cd "$staging" && shasum -a 256 source.tar.gz) > "$results/source-archive.sha256"
    "${cloud[@]}" compute instances describe "$instance" --zone="$zone" \
        --format='json(name,zone,machineType,cpuPlatform,scheduling,advancedMachineFeatures,disks[].autoDelete)' \
        > "$results/instance.json"
    "${cloud[@]}" compute scp --zone="$zone" "$staging/source.tar.gz" "$instance:source.tar.gz"
    rm -rf -- "$staging"
    local status=0 prefix=""
    if [[ -n ${SPROUTFS_GCE_BUCKET:-} ]]; then
        prefix="sproutfs-bench/$(basename "$results")-$(date -u +%Y%m%dT%H%M%SZ)"
        echo "gs://$SPROUTFS_GCE_BUCKET/$prefix" > "$results/bucket.txt"
    fi
    # shellcheck disable=SC2016  # PWD is the remote shell's, deliberately.
    "${cloud[@]}" compute ssh "$instance" --zone="$zone" --command='set -eu
        test -e /var/lib/sproutfs-bench/ready
        sudo /usr/local/sbin/sproutfs-bench-expire --check-only
        sudo systemctl is-active sproutfs-bench-expire.timer
        mkdir -p source results
        tar -xzf source.tar.gz -C source
        sudo env SPROUTFS_GCE_BUILD_ONLY='"${SPROUTFS_GCE_BUILD_ONLY:-0}"' SPROUTFS_GCE_FANOUT='"${SPROUTFS_GCE_FANOUT:-0}"' SPROUTFS_GCE_BOOTSURVEY='"${SPROUTFS_GCE_BOOTSURVEY:-0}"' SPROUTFS_GCE_QUALIFY='"${SPROUTFS_GCE_QUALIFY:-0}"' SPROUTFS_GCE_WORKLOAD='"${SPROUTFS_GCE_WORKLOAD:-0}"' SPROUTFS_GCE_SMOKE='"${SPROUTFS_GCE_SMOKE:-0}"' SPROUTFS_BENCH_SCENARIOS='"${SPROUTFS_BENCH_SCENARIOS:-}"' SPROUTFS_BENCH_FORKS='"${SPROUTFS_BENCH_FORKS:-}"' SPROUTFS_BENCH_RAM_BYTES='"${SPROUTFS_BENCH_RAM_BYTES:-}"' SPROUTFS_BENCH_ROOT_BYTES='"${SPROUTFS_BENCH_ROOT_BYTES:-}"' SPROUTFS_BENCH_RAM_RESIDENT_BYTES='"${SPROUTFS_BENCH_RAM_RESIDENT_BYTES:-}"' SPROUTFS_BENCH_PMEM_RESIDENT_BYTES='"${SPROUTFS_BENCH_PMEM_RESIDENT_BYTES:-}"' SPROUTFS_RAM_PAGE_BYTES='"${SPROUTFS_RAM_PAGE_BYTES:-}"' SPROUTFS_GCS_BUCKET='"${SPROUTFS_GCE_BUCKET:-}"' SPROUTFS_GCS_PREFIX='"$prefix"' timeout --signal=TERM --kill-after=30s '"$limit"' bash source/scripts/lib/bench-memory-linux.sh "$PWD/source" "$PWD/results"' \
        > "$results/remote.log" 2>&1 || status=$?
    "${cloud[@]}" compute scp --recurse --zone="$zone" "$instance:results/." "$results/" || status=$?
    # The run's objects are the benchmark's scratch, and the bucket keeps none
    # of them; its lifecycle rule is only the backstop for a run cut short.
    if [[ -n $prefix ]] && "${cloud[@]}" storage ls "gs://$SPROUTFS_GCE_BUCKET/$prefix/" > /dev/null 2>&1; then
        "${cloud[@]}" storage rm --recursive "gs://$SPROUTFS_GCE_BUCKET/$prefix/" > "$results/bucket-cleanup.log" 2>&1 || status=$?
    fi
    return "$status"
}

case "$action" in
    create) create ;;
    account) account ;;
    run) run ;;
    delete) delete ;;
    all)
        existing=$("${cloud[@]}" compute instances list --filter="name=$instance" --format='value(name)')
        [[ -z "$existing" ]] || { echo "Instance already exists: $instance" >&2; exit 1; }
        # The cloud-side deadline also covers forced termination of this shell.
        # Set the trap before creation to cover ambiguous create responses.
        trap 'status=$?; trap - EXIT; if ! delete; then exit 1; fi; exit "$status"' EXIT
        create
        run
        ;;
    *) echo "Usage: $0 [all|create|account|run|delete] [instance] [results-directory]" >&2; exit 2 ;;
esac
