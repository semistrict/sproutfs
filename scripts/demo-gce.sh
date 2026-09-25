#!/usr/bin/env bash
# One disposable GCE VM running a single-node k3s cluster for the demo, as
# plans/demo-gce-2026-09-13.md describes it. Everything this script creates is
# labelled purpose=demo,lifecycle=temporary, and `delete` verifies that the VM,
# its disk and its bucket are gone.
#
#   create    VM, HugeTLB pool, k3s, the demo bucket, deploy/
#   run       the five demo flows, with their timings; non-zero on any failure
#   fixes     the failure paths only a live cluster shows
#   bigguest  a guest larger than the x86_64 MMIO gap
#   workload  what the checkpoint model costs under a guest that is used
#   soak      many forks, migrations, stops and starts, every guest checked
#   redeploy  rebuild the image from the current source and restart the pods
#   status    what exists, on the cloud side and on the node
#   ssh       a shell on the VM, or a command on it
#   kubectl   kubectl on the node's cluster, arguments passed through
#   delete    the VM, its disks and the bucket
#
# Overridable: SPROUTFS_DEMO_PROJECT, SPROUTFS_DEMO_ZONE, SPROUTFS_DEMO_INSTANCE,
# SPROUTFS_DEMO_BUCKET, SPROUTFS_DEMO_VM_IMAGE.
set -euo pipefail

repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
project=${SPROUTFS_DEMO_PROJECT:-$(gcloud config get-value project 2>/dev/null)}
zone=${SPROUTFS_DEMO_ZONE:-us-east4-a}
region=${zone%-*}
instance=${SPROUTFS_DEMO_INSTANCE:-sproutfs-demo-1}
[[ -n "$project" && "$project" != '(unset)' ]] || { echo 'Set SPROUTFS_DEMO_PROJECT or a default gcloud project.' >&2; exit 2; }
bucket=${SPROUTFS_DEMO_BUCKET:-sproutfs-demo-$project}
prefix=${SPROUTFS_DEMO_PREFIX:-demo}
# Ubuntu 26.04 LTS, the same family the memory benchmark uses. If this pinned
# image has been retired, take the family's current one:
#   gcloud compute images describe-from-family ubuntu-2604-lts-amd64 \
#       --project ubuntu-os-cloud --format='value(name)'
vm_image=${SPROUTFS_DEMO_VM_IMAGE:-ubuntu-2604-resolute-amd64-v20260907}
container_image=sproutfs:demo

# Both names carry the prefix that every destructive path checks for.
[[ "$instance" == sproutfs-demo-* ]] || { echo 'Use a sproutfs-demo- instance name.' >&2; exit 2; }
[[ "$bucket" == sproutfs-demo-* ]] || { echo 'Use a sproutfs-demo- bucket name.' >&2; exit 2; }

cloud=(gcloud --quiet --project="$project")
labels=(purpose=demo lifecycle=temporary)
kube='export KUBECONFIG=/etc/rancher/k3s/k3s.yaml'

instance_exists() {
    local found
    found=$("${cloud[@]}" compute instances list --filter="name=$instance" --format='value(name)')
    [[ -n "$found" ]]
}

# No destructive or provisioning step runs against an instance that is not the
# demo's own.
check_instance_owner() {
    local seen
    seen=$("${cloud[@]}" compute instances describe "$instance" --zone="$zone" \
        --format='value(labels.purpose,labels.lifecycle)')
    [[ "$seen" == $'demo\ttemporary' ]] || {
        echo "Refusing to operate on $instance: it lacks the demo ownership labels." >&2
        return 1
    }
}

bucket_exists() {
    "${cloud[@]}" storage buckets describe "gs://$bucket" --format='value(name)' > /dev/null 2>&1
}

check_bucket_owner() {
    local seen
    seen=$("${cloud[@]}" storage buckets describe "gs://$bucket" \
        --format='value(labels.purpose,labels.lifecycle)')
    [[ "$seen" == $'demo\ttemporary' ]] || {
        echo "Refusing to operate on gs://$bucket: it lacks the demo ownership labels." >&2
        return 1
    }
}

service_account() {
    local number
    number=$("${cloud[@]}" projects describe "$project" --format='value(projectNumber)')
    printf '%s-compute@developer.gserviceaccount.com' "$number"
}

# One bucket in the VM's own region, object admin granted to the VM's service
# account at the bucket, not the project, and no keys anywhere.
create_bucket() {
    if bucket_exists; then
        check_bucket_owner
        echo "Bucket gs://$bucket already exists." >&2
    else
        "${cloud[@]}" storage buckets create "gs://$bucket" \
            --location="$region" --uniform-bucket-level-access \
            --public-access-prevention
        "${cloud[@]}" storage buckets update "gs://$bucket" \
            --update-labels="$(IFS=,; echo "${labels[*]}")"
    fi
    local account
    account=$(service_account)
    "${cloud[@]}" storage buckets add-iam-policy-binding "gs://$bucket" \
        --member="serviceAccount:$account" --role=roles/storage.objectAdmin > /dev/null
    echo "Bucket gs://$bucket in $region, object admin for $account." >&2
}

# The token the VM can mint is storage and nothing else, which is why the guest
# agent logs one ACCESS_TOKEN_SCOPE_INSUFFICIENT for Cloud Logging on each boot.
# Startup-script output still reaches the journal and the serial port.
create_instance() {
    "${cloud[@]}" compute instances create "$instance" --zone="$zone" \
        --machine-type="${SPROUTFS_DEMO_MACHINE_TYPE:-n2-standard-8}" \
        --min-cpu-platform='Intel Cascade Lake' --enable-nested-virtualization \
        --image="$vm_image" --image-project=ubuntu-os-cloud \
        --boot-disk-size=100GB --boot-disk-type=pd-balanced --boot-disk-auto-delete \
        --network="${SPROUTFS_DEMO_NETWORK:-default}" \
        --service-account="$(service_account)" \
        --scopes=https://www.googleapis.com/auth/devstorage.read_write \
        --metadata=block-project-ssh-keys=TRUE \
        --metadata-from-file="startup-script=$repo/scripts/lib/gce-demo-startup.sh" \
        --labels="$(IFS=,; echo "${labels[*]}")" \
        --maintenance-policy=TERMINATE --no-restart-on-failure \
        --max-run-duration=24h --instance-termination-action=DELETE
}

# Run a command on the VM. Every call touches the lease, so the inactivity
# timer only fires on a VM nobody is using. Keepalives because the image build
# holds one of these open for the length of a Rust build.
remote() {
    "${cloud[@]}" compute ssh "$instance" --zone="$zone" \
        --ssh-flag='-o ConnectTimeout=15' \
        --ssh-flag='-o ServerAliveInterval=60' \
        --ssh-flag='-o ServerAliveCountMax=10' \
        --command="sudo touch /var/lib/sproutfs-demo/lease 2>/dev/null; $1"
}

wait_for_startup() {
    local ready=false
    for _ in {1..40}; do
        if "${cloud[@]}" compute ssh "$instance" --zone="$zone" \
            --ssh-flag='-o ConnectTimeout=15' \
            --command='test -e /var/lib/sproutfs-demo/ready' > /dev/null 2>&1; then
            ready=true
            break
        fi
        sleep 15
    done
    "$ready" || {
        echo 'Startup script did not finish. Serial console output:' >&2
        "${cloud[@]}" compute instances get-serial-port-output "$instance" --zone="$zone" >&2 || true
        return 1
    }
}

# Everything the pods depend on, checked from outside the pods so a failure
# names the layer that is missing.
verify_node() {
    remote "set -eu
        $kube
        sudo /usr/local/sbin/sproutfs-demo-expire --check-only
        systemctl is-active sproutfs-demo-expire.timer
        echo '--- /dev/kvm ---'
        test -c /dev/kvm && stat -c '%n %A %U:%G' /dev/kvm
        echo '--- hugepages ---'
        cat /proc/sys/vm/nr_hugepages
        grep -E '^(HugePages_Total|HugePages_Free|Hugepagesize):' /proc/meminfo
        cat /etc/sysctl.d/99-sproutfs-demo-hugepages.conf
        echo '--- k3s ---'
        kubectl get nodes -o wide
        kubectl get node -o jsonpath='{.items[0].status.capacity.hugepages-2Mi}{\"\n\"}'
        sudo k3s ctr version | head -4"
}

# A throwaway pod proves that what the node has is reachable from inside a
# container. It runs before deploy/ is applied, on a freshly created VM only:
# once the host pods hold the whole HugeTLB pool there is none left to prove
# anything with.
verify_pod() {
    local manifest=$repo/deploy/testdata/kvm-hugepages-probe.yaml
    "${cloud[@]}" compute scp --zone="$zone" "$manifest" "$instance:kvm-hugepages-probe.yaml"
    remote "set -eu
        $kube
        kubectl delete --ignore-not-found --wait pod/sproutfs-demo-probe
        kubectl apply -f kvm-hugepages-probe.yaml
        kubectl wait --for=condition=Ready pod/sproutfs-demo-probe --timeout=120s
        kubectl logs pod/sproutfs-demo-probe
        kubectl delete --wait pod/sproutfs-demo-probe"
}

# The VM's own credentials, not the caller's, write to the bucket.
verify_bucket_from_vm() {
    remote "set -eu
        echo \"sproutfs demo write check from \$(hostname) at \$(date -u +%FT%TZ)\" > /tmp/sproutfs-demo-check.txt
        gcloud storage cp /tmp/sproutfs-demo-check.txt gs://$bucket/$prefix/write-check.txt
        gcloud storage cat gs://$bucket/$prefix/write-check.txt
        gcloud storage rm gs://$bucket/$prefix/write-check.txt"
}

# The manifests in deploy/ describe the topology; only the bucket and prefix
# depend on where the demo runs, so they are generated here as a ConfigMap in
# the copy the node applies, next to the Secret holding the deployment's token.
apply_deploy() {
    local staging token
    staging=$(mktemp -d "${TMPDIR:-/tmp}/sproutfs-demo.XXXXXX")
    # od is what every POSIX system has; openssl is not.
    token=${SPROUTFS_DEMO_TOKEN:-$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')}
    cp "$repo"/deploy/*.yaml "$staging/"
    cat > "$staging/05-config.yaml" <<CONFIG
# Generated by scripts/demo-gce.sh: the only part of deploy/ that depends on
# which project and bucket the demo runs against.
apiVersion: v1
kind: ConfigMap
metadata:
  name: sproutfs-demo
  namespace: sproutfs
data:
  bucket: "$bucket"
  prefix: "$prefix"
---
# The deployment's shared bearer token: every host API, the orchestrator API
# and sproutfsctl require it, and nothing outside these pods has it. It is
# generated per deployment unless SPROUTFS_DEMO_TOKEN names one; roll_pods below
# is what puts the running pods onto it.
apiVersion: v1
kind: Secret
metadata:
  name: sproutfs-api-token
  namespace: sproutfs
type: Opaque
stringData:
  token: "$token"
CONFIG
    # scp --recurse nests the source inside an existing destination directory.
    remote 'rm -rf ~/deploy'
    "${cloud[@]}" compute scp --recurse --zone="$zone" "$staging" "$instance:deploy"
    rm -r -- "$staging"
    remote "set -eu
        $kube
        kubectl apply -f deploy/
        # The staged copy holds the Secret. Once the cluster has it, nothing on
        # the node needs it, and a token left in a home directory is a token
        # anyone with a shell on this VM can read.
        rm -rf ~/deploy
        kubectl get -n sproutfs deployments,services,pods"
    # A fresh apply starts pods on the token it carries, so nothing has to be
    # rolled: rolling here killed both hosts in the middle of importing their
    # templates, and a host replaced mid-import is what a redeploy tests, not
    # what a create does. Such a replacement recovers the half-finished template
    # and comes back, but it pays for that import again before it is ready,
    # which is a rollout nobody asked for.
    wait_for_pods
}

# roll_pods restarts the workloads onto what was just applied. A Secret and a
# ConfigMap are read into a container's environment when it starts, so applying
# a new token changes nothing about the pods already running on the old one:
# they go on presenting it to each other and refusing every request from
# anything that has been restarted since, which is a deployment that has half
# rotated and says only 401.
#
# The hosts roll first and the orchestrator only once they are back. A draining
# host asks the orchestrator to move each of its VMs and drives nothing itself,
# so restarting the two together is a drain with nothing to answer it — every VM
# on the pod being replaced rewound to its last checkpoint.
roll_pods() {
    remote "set -eu
        $kube
        kubectl rollout restart -n sproutfs deployment/sproutfs-host"
    wait_for_pods
    remote "set -eu
        $kube
        kubectl rollout restart -n sproutfs deployment/sproutfs-orchestrator"
    wait_for_pods
}

# Only versioned and non-ignored source, including the Firecracker submodule,
# reaches the node: no credentials, Git internals or build caches.
stage_source() {
    python3 - "$repo" "$1" <<'PY'
import pathlib, subprocess, sys, tarfile
root = pathlib.Path(sys.argv[1])
def files(directory):
    return subprocess.check_output(['git', '-C', str(directory), 'ls-files',
                                    '--cached', '--others', '--exclude-standard', '-z']).split(b'\0')
names = {name.decode() for name in files(root) if name}
submodule = root / 'third_party/firecracker'
if (submodule / '.git').exists():
    names.update('third_party/firecracker/' + name.decode() for name in files(submodule) if name)
with tarfile.open(sys.argv[2], 'w:gz') as archive:
    for name in sorted(names):
        path = root / name
        if path.is_file() or path.is_symlink():
            archive.add(path, arcname='repo/' + name, recursive=False)
PY
}

# The container image and the guest image are scripts/lib/demo-image.sh's work.
# It runs here, as root on the node, over a copy of this repository, and leaves
# $container_image in k3s's containerd (namespace k8s.io) and the guest image at
# /opt/sproutfs-demo/guest/guest.ext4.
build_image() {
    local hook=$repo/scripts/lib/demo-image.sh
    if [[ ! -f "$hook" ]]; then
        cat >&2 <<HOOK
scripts/lib/demo-image.sh is not present, so no container image was built.
The pods will stay in ErrImageNeverPull until it exists: they run
$container_image with imagePullPolicy: Never. Re-run:
    $0 create
once the image build script has landed.
HOOK
        return 0
    fi
    # The build needs the Firecracker submodule; an empty one fails inside the
    # container image, far from the cause.
    [[ -f "$repo/third_party/firecracker/Cargo.toml" ]] || {
        echo 'third_party/firecracker is empty. Run:' >&2
        echo '    git submodule update --init third_party/firecracker' >&2
        return 1
    }
    local staging
    staging=$(mktemp -d "${TMPDIR:-/tmp}/sproutfs-demo-source.XXXXXX")
    stage_source "$staging/repo.tar.gz"
    "${cloud[@]}" compute scp --zone="$zone" \
        "$staging/repo.tar.gz" "$hook" "$instance:"
    rm -r -- "$staging"
    # The Rust build runs for tens of minutes. pipefail so that a failure inside
    # it is not hidden by tee's success; the log stays on the node for a
    # post-mortem that does not need this shell.
    remote "set -euo pipefail
        sudo install -d -m 0755 /opt/sproutfs-demo
        sudo rm -rf /opt/sproutfs-demo/repo
        sudo tar -xzf repo.tar.gz -C /opt/sproutfs-demo
        sudo bash demo-image.sh /opt/sproutfs-demo $container_image 2>&1 \
            | tee /tmp/demo-image.log"
}

# Wait for the deployments the demo drives. A pod that never becomes ready is
# reported with its own logs, which is where the reason is.
wait_for_pods() {
    remote "set -eu
        $kube
        if ! kubectl rollout status -n sproutfs deployment/sproutfs-host --timeout=300s ||
           ! kubectl rollout status -n sproutfs deployment/sproutfs-orchestrator --timeout=300s; then
            kubectl get -n sproutfs pods -o wide
            kubectl describe -n sproutfs pods | tail -60
            kubectl logs -n sproutfs -l app.kubernetes.io/part-of=sproutfs --tail=80 --prefix || true
            exit 1
        fi
        kubectl get -n sproutfs pods -o wide"
}

create() {
    local fresh=false
    if instance_exists; then
        check_instance_owner
        echo "Instance $instance already exists; reprovisioning it." >&2
    else
        create_bucket
        create_instance
        fresh=true
    fi
    check_instance_owner
    bucket_exists || create_bucket
    wait_for_startup
    verify_node
    verify_bucket_from_vm
    if "$fresh"; then verify_pod; fi
    build_image
    apply_deploy
    wait_for_pods
    cat >&2 <<DONE

Demo cluster up on $instance ($zone), bucket gs://$bucket.
    $0 run
    $0 kubectl get pods -n sproutfs
    $0 ssh
    $0 delete
DONE
}

# The five flows, run on the node by scripts/lib/demo-run.sh. sproutfsctl is in
# the deployment's own image, so the flows run through kubectl exec rather than
# through a port-forward out of the cluster.
run() {
    check_instance_owner
    wait_for_pods
    "${cloud[@]}" compute scp --zone="$zone" "$repo/scripts/lib/demo-run.sh" "$instance:"
    remote "set -euo pipefail
        $kube
        bash demo-run.sh 2>&1 | tee /tmp/demo-run.log"
}

# The blocker fixes that only a live cluster can show — a fork whose destination
# pod is deleted, a recovery refused while the host is alive, a VMM killed
# underneath its host — run on the node by scripts/lib/demo-fixes.sh.
fixes() {
    check_instance_owner
    wait_for_pods
    "${cloud[@]}" compute scp --zone="$zone" "$repo/scripts/lib/demo-fixes.sh" "$instance:"
    remote "set -euo pipefail
        $kube
        bash demo-fixes.sh 2>&1 | tee /tmp/demo-fixes.log"
}

# A guest larger than the x86_64 MMIO gap, run on the node by
# scripts/lib/demo-bigguest.sh. Only an x86_64 node exercises the mapping of two
# guest memory regions at all, which is why this is here and not in the Lima tests.
bigguest() {
    check_instance_owner
    wait_for_pods
    "${cloud[@]}" compute scp --zone="$zone" "$repo/scripts/lib/demo-bigguest.sh" "$instance:"
    remote "set -euo pipefail
        $kube
        bash demo-bigguest.sh 2>&1 | tee /tmp/demo-bigguest.log"
}

# What the checkpoint model costs under a guest that is used, run on the node by
# scripts/lib/demo-workload.sh. FORKS_BASE and FORKS_PER_REPO are passed through
# so one invocation can ask what a different amount of forking costs; everything
# the run records is left on the node under /tmp/sproutfs-workload and copied
# back here.
workload() {
    check_instance_owner
    wait_for_pods
    "${cloud[@]}" compute scp --zone="$zone" "$repo/scripts/lib/demo-workload.sh" "$instance:"
    remote "set -euo pipefail
        $kube
        FORKS_BASE=${FORKS_BASE:-2} FORKS_PER_REPO=${FORKS_PER_REPO:-1} \
            bash demo-workload.sh 2>&1 | tee /tmp/demo-workload.log"
    local into=${SPROUTFS_DEMO_WORKLOAD_OUT:-$repo/.workload-runs}
    local run=$into/base${FORKS_BASE:-2}-repo${FORKS_PER_REPO:-1}
    # scp --recurse nests the source inside an existing destination directory,
    # so a second run of one setting would land under the first one rather than
    # replace it.
    rm -rf -- "$run"
    mkdir -p "$into"
    "${cloud[@]}" compute scp --recurse --zone="$zone" \
        "$instance:/tmp/sproutfs-workload" "$run"
    echo "Recorded under $run." >&2
}

# Many forks, migrations, stops and starts under a workload, with every guest's
# memory and disk checked after every one of them, run on the node by
# scripts/lib/demo-soak.sh. SOAK_SEED and the round's shares are passed through
# so one invocation can ask for a longer or a wider run; everything the run
# records is left on the node under /tmp/sproutfs-soak and copied back here.
soak() {
    check_instance_owner
    wait_for_pods
    "${cloud[@]}" compute scp --zone="$zone" "$repo/scripts/lib/demo-soak.sh" "$instance:"
    local settings=(
        "SOAK_SEED=${SOAK_SEED:-1}"
        "SOAK_ROUNDS=${SOAK_ROUNDS:-6}"
        "SOAK_FORKS_LOCAL=${SOAK_FORKS_LOCAL:-2}"
        "SOAK_FORKS_REMOTE=${SOAK_FORKS_REMOTE:-2}"
        "SOAK_MIGRATIONS=${SOAK_MIGRATIONS:-2}"
        "SOAK_STOPS=${SOAK_STOPS:-2}"
        "SOAK_WITNESS_BYTES=${SOAK_WITNESS_BYTES:-256M}"
        "SOAK_MAX_VMS=${SOAK_MAX_VMS:-6}"
    )
    remote "set -euo pipefail
        $kube
        ${settings[*]} bash demo-soak.sh 2>&1 | tee /tmp/demo-soak.log"
    local into=${SPROUTFS_DEMO_SOAK_OUT:-$repo/.workload-runs} run
    run=$into/soak-$(date -u +%Y%m%dT%H%M%SZ)
    # scp --recurse nests the source inside an existing destination directory,
    # so each run gets a directory of its own that does not yet exist.
    mkdir -p "$into"
    "${cloud[@]}" compute scp --recurse --zone="$zone" \
        "$instance:/tmp/sproutfs-soak" "$run"
    echo "Recorded under $run." >&2
}

# Rebuild the image from the current source and restart the pods on it. The
# Dockerfile copies the Rust sources before anything else, so a change to the Go
# commands or the manifests reuses the cached VMM and kernel stages and the
# rebuild is the Go stage alone.
redeploy() {
    check_instance_owner
    build_image
    apply_deploy
    # The image tag does not change, so an apply alone restarts nothing: the
    # pods are rolled onto the rebuilt image, hosts first and the orchestrator
    # after them, which is the order a drain needs.
    roll_pods
}

status() {
    if instance_exists; then
        "${cloud[@]}" compute instances describe "$instance" --zone="$zone" \
            --format='table(name,status,machineType.basename(),labels.purpose,labels.lifecycle,scheduling.maxRunDuration.seconds,advancedMachineFeatures.enableNestedVirtualization)'
    else
        echo "No instance $instance in $zone." >&2
    fi
    if bucket_exists; then
        "${cloud[@]}" storage buckets describe "gs://$bucket" \
            --format='table(name,location,labels.purpose,labels.lifecycle)'
    else
        echo "No bucket gs://$bucket." >&2
    fi
    if instance_exists; then
        remote "set -eu
            $kube
            sudo /usr/local/sbin/sproutfs-demo-expire --check-only
            cat /proc/sys/vm/nr_hugepages
            kubectl get nodes
            kubectl get -n sproutfs deployments,services,pods 2>&1 || true" || true
    fi
}

delete() {
    if instance_exists; then
        check_instance_owner
        "${cloud[@]}" compute instances delete "$instance" --zone="$zone" --delete-disks=all
    fi
    if bucket_exists; then
        check_bucket_owner
        # Objects first: a bucket with objects in it will not delete.
        "${cloud[@]}" storage rm --recursive "gs://$bucket/**" 2> /dev/null || true
        "${cloud[@]}" storage buckets delete "gs://$bucket"
    fi
    local found
    found=$("${cloud[@]}" compute instances list --filter="name=$instance" --format='value(name)')
    [[ -z "$found" ]] || { echo "Instance still exists: $instance" >&2; return 1; }
    found=$("${cloud[@]}" compute disks list --filter="name=$instance" --format='value(name)')
    [[ -z "$found" ]] || { echo "Boot disk still exists: $instance" >&2; return 1; }
    ! bucket_exists || { echo "Bucket still exists: gs://$bucket" >&2; return 1; }
    echo "Verified deletion of $instance, its disks and gs://$bucket." >&2
}

action=${1:-}
[[ $# -eq 0 ]] || shift
case "$action" in
    create) create ;;
    run) run ;;
    fixes) fixes ;;
    bigguest) bigguest ;;
    workload) workload ;;
    soak) soak ;;
    redeploy) redeploy ;;
    status) status ;;
    # ssh joins its arguments and hands them to a shell on the node, the way
    # ssh(1) does, so both `ssh ls -l /opt` and `ssh 'a | b'` mean what they
    # look like. kubectl quotes each argument instead, so that a jsonpath or a
    # selector survives that shell intact.
    ssh)
        if [[ $# -eq 0 ]]; then
            "${cloud[@]}" compute ssh "$instance" --zone="$zone"
        else
            remote "$*"
        fi
        ;;
    kubectl) remote "$kube; kubectl $(printf '%q ' "$@")" ;;
    delete) delete ;;
    *) echo "Usage: $0 [create|run|fixes|bigguest|workload|soak|redeploy|status|ssh|kubectl|delete] [arguments]" >&2; exit 2 ;;
esac
