#!/usr/bin/env bash
# Disposable nested-KVM memory measurements. `all` always deletes its VM.
# SPROUTFS_GCE_FANOUT=1 runs only the fork fan-out that lists the pages each
# fork came to own.
# `create`, `run` and `delete` expose the same steps for interrupted runs.
set -euo pipefail
case ${SPROUTFS_GCE_BUILD_ONLY:-0} in 0|1) ;; *) echo "SPROUTFS_GCE_BUILD_ONLY must be 0 or 1" >&2; exit 2 ;; esac
case ${SPROUTFS_GCE_FANOUT:-0} in 0|1) ;; *) echo "SPROUTFS_GCE_FANOUT must be 0 or 1" >&2; exit 2 ;; esac
repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
project=${SPROUTFS_GCE_PROJECT:-$(gcloud config get-value project 2>/dev/null)}
zone=${SPROUTFS_GCE_ZONE:-us-east4-a}
action=${1:-all}
instance=${2:-sproutfs-memprobe-$(date -u +%Y%m%d-%H%M%S)}
results=${3:-$repo/docs/measurements/gce-memory-$(date -u +%Y%m%d-%H%M%S)}
[[ -n "$project" && "$project" != '(unset)' ]] || { echo 'Set SPROUTFS_GCE_PROJECT.' >&2; exit 2; }
[[ "$instance" == sproutfs-memprobe-* ]] || { echo 'Use a sproutfs-memprobe- instance name.' >&2; exit 2; }
cloud=(gcloud --quiet --project="$project")

create() {
    "${cloud[@]}" compute instances create "$instance" --zone="$zone" \
        --machine-type="${SPROUTFS_GCE_MACHINE_TYPE:-n2-standard-8}" \
        --min-cpu-platform='Intel Cascade Lake' --enable-nested-virtualization \
        --image=ubuntu-2604-resolute-amd64-v20260907 --image-project=ubuntu-os-cloud \
        --boot-disk-size=80GB --boot-disk-type=pd-balanced --boot-disk-auto-delete \
        --network="${SPROUTFS_GCE_NETWORK:-default}" --no-service-account --no-scopes \
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
    local status=0
    # shellcheck disable=SC2016  # PWD is the remote shell's, deliberately.
    "${cloud[@]}" compute ssh "$instance" --zone="$zone" --command='set -eu
        test -e /var/lib/sproutfs-bench/ready
        sudo /usr/local/sbin/sproutfs-bench-expire --check-only
        sudo systemctl is-active sproutfs-bench-expire.timer
        mkdir -p source results
        tar -xzf source.tar.gz -C source
        sudo env SPROUTFS_GCE_BUILD_ONLY='"${SPROUTFS_GCE_BUILD_ONLY:-0}"' SPROUTFS_GCE_FANOUT='"${SPROUTFS_GCE_FANOUT:-0}"' timeout --signal=TERM --kill-after=30s 2h bash source/scripts/lib/bench-memory-linux.sh "$PWD/source" "$PWD/results"' \
        > "$results/remote.log" 2>&1 || status=$?
    "${cloud[@]}" compute scp --recurse --zone="$zone" "$instance:results/." "$results/" || status=$?
    return "$status"
}

case "$action" in
    create) create ;;
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
    *) echo "Usage: $0 [all|create|run|delete] [instance] [results-directory]" >&2; exit 2 ;;
esac
