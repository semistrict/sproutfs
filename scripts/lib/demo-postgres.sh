#!/usr/bin/env bash
# An embedder's database benchmark, on the demo node, with the host shaped as
# theirs: what they ran on 2026-10-08, where a guest stopped answering for two
# minutes while it started PostgreSQL after a test that wrote and read a 12 GiB
# file.
#
# scripts/demo-gce.sh copies this one file to the node and runs it there:
#
#     bash demo-postgres.sh
#
# The host deployment is reshaped first, and left so: one host, a 16 GiB arena
# three fifths RAM, 32 GiB of spill files on the node's disk, every dirty budget
# at the host's default, and a postgres template whose VMs get 8 GiB. The VM is
# that template on an 80 GiB root (ext4, dax=always) with 4 vCPUs, and runs, as
# the embedder's did:
#
#   file      12 GiB written with fio, then eight threads reading it 8 KiB at a
#             time at random for two minutes beside a 50 MB/s log writer, then
#             deleted
#   layers    the image's /usr copied three times and synced, which is what
#             their container runtime's image pull and extract wrote; this guest
#             has no network and no container runtime
#   load      initdb, PostgreSQL started with synchronous_commit=on,
#             shared_buffers=512MB and max_wal_size=4GB, and pgbench -i -s 300
#   run       pgbench -c 16 -j 8 -T 120
#
# Every phase runs detached in the guest and is polled. Throughout, the guest is
# probed every five seconds with an exec of `true`, the path the embedder's
# health check takes, and the host's metrics and the node's pressure and disk
# counters are sampled. A probe that has not answered in 30 s is a stall: the
# VMM's threads' kernel stacks, the host's threads' wait channels and the
# node's counters are kept every 15 s while it lasts. One unanswered for
# 150 s is a freeze: the host is sent SIGQUIT, its goroutines are kept from
# its log, and the run ends.
#
# SPROUTFS_POSTGRES_PLAIN=1 runs the same phases on the node itself instead,
# which is the plain machine the embedder compares against, held to what the
# guest has: the postgres image's own userland in a chroot on the node's disk,
# in a systemd slice of 8 GiB of memory (its page cache included, no swap) and
# four CPUs. Nothing is probed there, and the sproutfs host is left idle.
#
# What it records, under $SPROUTFS_POSTGRES_RUN_DIR (/tmp/sproutfs-postgres):
#   phases.tsv     each phase's name, start and end
#   probes.tsv     each probe's start, seconds and exit status
#   <phase>.out    what the phase printed in the guest
#   metrics/       the host's /metrics every 5 s, gzipped
#   node.tsv       the node's IO, memory and CPU pressure and disk counters
#   stall-<time>/  the evidence above, for each sample taken during a stall
#   host.log       the host's log over the run, goroutines and all
#   summary.txt    what this prints at the end
set -euo pipefail

plain=${SPROUTFS_POSTGRES_PLAIN:-0}
case $plain in 0|1) ;; *) echo "SPROUTFS_POSTGRES_PLAIN must be 0 or 1" >&2; exit 2 ;; esac
# SPROUTFS_POSTGRES_PULL=1 creates the VM marked to pull its memory, which is
# what keeps its published pages on the host's disk outside the cluster cache.
pull=${SPROUTFS_POSTGRES_PULL:-0}
case $pull in 0|1) ;; *) echo "SPROUTFS_POSTGRES_PULL must be 0 or 1" >&2; exit 2 ;; esac
# SPROUTFS_POSTGRES_HOST_ENV is more of the host's settings, NAME=VALUE
# separated by spaces, set over the embedder's shape: what an experiment varies.
read -r -a host_env <<< "${SPROUTFS_POSTGRES_HOST_ENV:-}"
export KUBECONFIG=${KUBECONFIG:-/etc/rancher/k3s/k3s.yaml}
namespace=${SPROUTFS_DEMO_NAMESPACE:-sproutfs}
run_dir=${SPROUTFS_POSTGRES_RUN_DIR:-/tmp/sproutfs-postgres}

# The embedder's host: a 16 GiB arena with 60% RAM and 32 GiB of spill. Each is
# 4 MiB short of theirs so that 60% of it is whole 4 KiB RAM pages and the rest
# whole 2 MiB PMEM pages, which the host requires.
arena_bytes=$((16380 << 20))
spill_bytes=$((32760 << 20))
ram_share=60
guest_memory_bytes=$((8 << 30))
guest_disk=80G
guest_vcpus=4
template_path=/usr/share/sproutfs/guest/postgres.ext4
# The same image on the node, which the plain run's chroot is copied from.
node_template=/opt/sproutfs-demo/guest/postgres.ext4

# The host pod's HugeTLB allotment. Both pagers' pages are 2 MiB by default,
# so both arenas are huge pages: 9828 MiB of RAM, 6552 MiB of PMEM and the
# ephemeral arena's 256 MiB, which is 8318 pages; 17 GiB is 8704 of them, and
# the node's pool is made at least that. SPROUTFS_POSTGRES_HUGEPAGES overrides
# the allotment.
hugepages=${SPROUTFS_POSTGRES_HUGEPAGES:-17Gi}
pool_pages=8704
pod_memory=16Gi
# SPROUTFS_POSTGRES_PMEM_PAGE=4096 runs the disks at 4 KiB pages. Their arenas
# are then ordinary memory charged to the pod, so its allotment of huge pages
# shrinks to the RAM arena's and its memory grows by the PMEM arena's. A
# template is named by its pages, so the host imports it again at 4 KiB.
pmem_page=${SPROUTFS_POSTGRES_PMEM_PAGE:-2097152}
case $pmem_page in
    2097152) ;;
    4096)
        hugepages=${SPROUTFS_POSTGRES_HUGEPAGES:-10Gi}
        pod_memory=24Gi
        ;;
    *) echo "SPROUTFS_POSTGRES_PMEM_PAGE must be 4096 or 2097152" >&2; exit 2 ;;
esac

# A probe unanswered for stall_seconds is a stall, and for freeze_seconds a
# freeze: the embedder's host gave up on its guest after about 110 s.
stall_seconds=30
freeze_seconds=150
# A phase's own bound, far over what the embedder's took.
phase_seconds=3600

rm -rf -- "$run_dir"
mkdir -p "$run_dir/metrics"
: > "$run_dir/phases.tsv"
: > "$run_dir/probes.tsv"
: > "$run_dir/node.tsv"

ctl() { kubectl exec -n "$namespace" -i deploy/sproutfs-orchestrator -- sproutfsctl "$@" < /dev/null; }
now() { date -u +%s.%N; }
since() { awk -v began="$1" -v now="$(now)" 'BEGIN { printf "%.3f", now - began }'; }
step() { printf '\n=== %s ===\n' "$*"; }
fail() { printf '\nFAIL: %s\n' "$*" >&2; exit 1; }
host_pod() { kubectl get pod -n "$namespace" -l app.kubernetes.io/name=sproutfs-host \
    -o jsonpath='{.items[0].metadata.name}'; }

# --- the plain machine ---------------------------------------------------------------
# prepare_plain copies the image's tree onto the node's disk, with the kernel's
# own filesystems bound into it, and caps the slice its phases run in.
plain_root=
pod=
token=
prepare_plain() {
    step 'a chroot of the postgres image on the node, held to 8 GiB and 4 CPUs'
    plain_root=/var/lib/sproutfs-plain/$(date -u +%Y%m%dT%H%M%SZ)
    local image=$plain_root.image
    sudo -n mkdir -p "$plain_root" "$image"
    sudo -n mount -o loop,ro "$node_template" "$image"
    sudo -n cp -a "$image/." "$plain_root/"
    sudo -n umount "$image"
    local point
    for point in proc sys dev; do
        sudo -n mount --bind "/$point" "$plain_root/$point"
    done
    sudo -n systemctl set-property --runtime sproutfs-plain.slice \
        MemoryMax="$guest_memory_bytes" MemorySwapMax=0 AllowedCPUs=0-$((guest_vcpus - 1))
    {
        df -h "$plain_root"
        cat "$plain_root/etc/sproutfs-postgres-version"
        nproc
    } | tee "$run_dir/guest.txt"
}

# target_run runs one short command in what is measured, synchronously.
target_run() {
    if ((plain)); then
        sudo -n chroot "$plain_root" /bin/sh -c "$1"
    else
        ctl exec "$vm" --timeout 600s -- "$1"
    fi
}

if ((plain)); then
    prepare_plain
    run_began=$(date -u +%FT%TZ)
else

# --- the host, shaped as the embedder's -----------------------------------------
# Both arenas are HugeTLB, so the node's pool and the pod's allotment grow to
# hold them; the pod's 16 GiB of ordinary memory holds the Go heap, the page
# cache, the VMM and the spill files' page cache. The disk limiter's ceiling
# makes room for the spill files. The kubelet reads the pool when it starts,
# so a pool that grew restarts k3s.
step 'shape the host as the embedder runs it'
if (($(awk '/^HugePages_Total:/ { print $2 }' /proc/meminfo) < pool_pages)); then
    printf 'vm.nr_hugepages = %s\n' "$pool_pages" |
        sudo -n tee /etc/sysctl.d/99-sproutfs-demo-hugepages.conf > /dev/null
    sudo -n sysctl -q -w vm.nr_hugepages="$pool_pages"
    (($(awk '/^HugePages_Total:/ { print $2 }' /proc/meminfo) == pool_pages)) ||
        fail "the node allocated $(awk '/^HugePages_Total:/ { print $2 }' /proc/meminfo) of $pool_pages huge pages"
    sudo -n systemctl restart k3s
    deadline=$(($(date +%s) + 300))
    # The kubelet writes a quantity in its largest whole unit: 17Gi, not 17408Mi.
    advertised() { kubectl get node -o jsonpath='{.items[0].status.capacity.hugepages-2Mi}' 2> /dev/null; }
    until [[ $(advertised) == "$((pool_pages * 2))Mi" || $(advertised) == "$((pool_pages * 2 / 1024))Gi" ]]; do
        (($(date +%s) < deadline)) || fail 'the kubelet does not advertise the larger pool'
        sleep 2
    done
fi
kubectl scale -n "$namespace" deployment/sproutfs-host --replicas=1 > /dev/null
kubectl set env -n "$namespace" deployment/sproutfs-host \
    SPROUTFS_ARENA_BYTES="$arena_bytes" SPROUTFS_RAM_SHARE_PERCENT="$ram_share" \
    SPROUTFS_SPILL_BYTES="$spill_bytes" SPROUTFS_DISK_USED_BYTES=$((150 << 30)) \
    SPROUTFS_MEMORY_BYTES- SPROUTFS_RAM_DIRTY_PAGES- SPROUTFS_PMEM_DIRTY_PAGES- \
    SPROUTFS_TEMPLATES="alpine=/usr/share/sproutfs/guest/guest.ext4,postgres=$template_path:$guest_memory_bytes" \
    SPROUTFS_PMEM_PAGE_BYTES="$pmem_page" GOMEMLIMIT=10GiB > /dev/null
((${#host_env[@]} == 0)) || kubectl set env -n "$namespace" deployment/sproutfs-host "${host_env[@]}" > /dev/null
# The host's processor quota is the guest's vCPUs and one more for the pager:
# plain Linux has the guest's vCPUs to itself, and a quota below them throttles
# the guest's vCPUs and the pager that serves their faults together.
host_cpus=$((guest_vcpus + 1))
kubectl patch -n "$namespace" deployment/sproutfs-host --type json -p '[
    {"op": "replace", "path": "/spec/template/spec/containers/0/resources",
     "value": {"requests": {"cpu": "'"$host_cpus"'", "memory": "'"$pod_memory"'", "hugepages-2Mi": "'"$hugepages"'"},
               "limits": {"cpu": "'"$host_cpus"'", "memory": "'"$pod_memory"'", "hugepages-2Mi": "'"$hugepages"'"}}}]' > /dev/null
kubectl rollout status -n "$namespace" deployment/sproutfs-host --timeout=900s > /dev/null
kubectl rollout restart -n "$namespace" deployment/sproutfs-orchestrator > /dev/null
kubectl rollout status -n "$namespace" deployment/sproutfs-orchestrator --timeout=300s > /dev/null
deadline=$(($(date +%s) + 600))
until (($(ctl hosts 2> /dev/null | awk 'NR > 1 && $2 == "true"' | wc -l) == 1)); do
    (($(date +%s) < deadline)) || fail 'the orchestrator does not see the host ready'
    sleep 2
done
run_began=$(date -u +%FT%TZ)
pod=$(host_pod)
kubectl get deployment -n "$namespace" sproutfs-host \
    -o jsonpath='{range .spec.template.spec.containers[0].env[*]}{.name}{"="}{.value}{"\n"}{end}' \
    > "$run_dir/host-env.txt"
token=$(kubectl get secret -n "$namespace" sproutfs-api-token -o jsonpath='{.data.token}' | base64 -d)

# --- the VM -----------------------------------------------------------------------
# A VM an earlier run left would hold arena pages this one is measured without.
for left in $(ctl list | awk 'NR > 1 { print $1 }'); do
    ctl delete "$left" > /dev/null
done
step "create a VM from the postgres template: $((guest_memory_bytes >> 30)) GiB, $guest_vcpus vCPUs, a $guest_disk root"
pull_flag=()
((pull == 0)) || pull_flag=(--pull)
created=$(ctl create --template postgres --memory "$((guest_memory_bytes >> 30))G" \
    --disk "$guest_disk" --vcpus "$guest_vcpus" "${pull_flag[@]}")
printf '%s\n' "$created"
vm=${created%% *}
[[ $vm == vm-* ]] || fail "create did not name a VM: $created"
deadline=$(($(date +%s) + 180))
until ctl exec "$vm" --timeout 30s -- true > /dev/null 2>&1; do
    (($(date +%s) < deadline)) || fail "the agent in $vm never answered"
done
target_run 'sproutfs-guest-witness grow / && df -h / && cat /etc/sproutfs-postgres-version && nproc' |
    tee "$run_dir/guest.txt"
fi

# --- watching -----------------------------------------------------------------------
# probe_loop asks the guest for `true` every five seconds. The start of the probe
# in flight is in probe.inflight, which is how the watcher knows a probe is late
# before it has answered.
probe_loop() {
    local began status
    while [[ ! -e $run_dir/.stop ]]; do
        began=$(now)
        printf '%s\n' "$began" > "$run_dir/probe.inflight"
        status=0
        ctl exec "$vm" --timeout 600s -- true > /dev/null 2>> "$run_dir/probe.err" || status=$?
        rm -f -- "$run_dir/probe.inflight"
        printf '%s\t%s\t%s\n' "$began" "$(since "$began")" "$status" >> "$run_dir/probes.tsv"
        sleep 5
    done
}

# sample_node appends one line of the node's pressure and its disk's counters,
# and the huge pages the host's cgroup holds, against its limit, with the
# number of times it has been refused one.
sample_node() {
    local disk cgroup=- pid
    disk=$(lsblk -ndo NAME "$(findmnt -no SOURCE / | sed 's/[0-9]*$//')" 2> /dev/null || echo sda)
    pid=$(pgrep -x sproutfs-host | head -1 || true)
    if [[ -n $pid ]]; then
        cgroup=/sys/fs/cgroup$(cut -d: -f3 "/proc/$pid/cgroup" 2> /dev/null)
        cgroup="$(cat "$cgroup/hugetlb.2MB.current" 2> /dev/null)/$(cat "$cgroup/hugetlb.2MB.max" 2> /dev/null)/$(awk '{ print $2 }' "$cgroup/hugetlb.2MB.events" 2> /dev/null)"
    fi
    printf '%s\tio:%s\tmemory:%s\tcpu:%s\thugetlb:%s\tdisk:%s\n' "$(now)" \
        "$(sed -n 2p /proc/pressure/io)" "$(sed -n 2p /proc/pressure/memory)" "$(sed -n 1p /proc/pressure/cpu)" \
        "$cgroup" "$(awk -v d="$disk" '$3 == d' /proc/diskstats)" >> "$run_dir/node.tsv"
}

# sample_metrics keeps the host's exposition. A host that cannot answer in ten
# seconds is itself evidence, and is noted rather than waited for.
sample_metrics() {
    local ip at
    [[ -n $pod ]] || return 0
    ip=$(kubectl get pod -n "$namespace" "$pod" -o jsonpath='{.status.podIP}' 2> /dev/null) || return 0
    at=$(date -u +%s)
    if ! curl --silent --fail --max-time 10 -H "Authorization: Bearer $token" \
        "http://$ip:8080/metrics" 2> /dev/null | gzip > "$run_dir/metrics/$at.prom.gz"; then
        rm -f -- "$run_dir/metrics/$at.prom.gz"
        printf '%s\tmetrics unanswered\n' "$at" >> "$run_dir/node.tsv"
    fi
}

# evidence keeps what says where a stalled guest is waiting: each VMM thread's
# kernel stack (a vCPU in a fault the pager has not answered sits in
# handle_userfault), the host's threads by wait channel, and the node's counters.
evidence() {
    local into pid task
    into=$run_dir/stall-$(date -u +%Y%m%dT%H%M%SZ)
    mkdir -p "$into"
    for pid in $(pgrep -f '(^|/)firecracker( |$)' || true); do
        for task in /proc/"$pid"/task/*; do
            printf '== %s %s wchan=%s\n' "$task" "$(cat "$task/comm" 2> /dev/null)" \
                "$(cat "$task/wchan" 2> /dev/null)"
            sudo -n cat "$task/stack" 2> /dev/null || true
        done
    done > "$into/vmm-stacks.txt"
    for pid in $(pgrep -x sproutfs-host || true); do
        for task in /proc/"$pid"/task/*; do
            printf '%s %s\n' "$(cat "$task/wchan" 2> /dev/null)" "$(awk '{ print $3 }' "$task/stat" 2> /dev/null)"
        done | sort | uniq -c | sort -rn
    done > "$into/host-waits.txt"
    cat /proc/pressure/io /proc/pressure/memory /proc/vmstat > "$into/node.txt" 2> /dev/null || true
    ctl hosts > "$into/hosts.txt" 2>&1 || true
}

# watch_loop samples every five seconds and watches the probe in flight.
watch_loop() {
    local began age last_evidence=0
    while [[ ! -e $run_dir/.stop ]]; do
        sample_node
        sample_metrics
        if began=$(cat "$run_dir/probe.inflight" 2> /dev/null) && [[ -n $began ]]; then
            age=$(since "$began" | cut -d. -f1)
            if ((age >= freeze_seconds)); then
                printf '%s\tfrozen: a probe unanswered for %s s\n' "$(now)" "$age" >> "$run_dir/stalls.tsv"
                evidence
                freeze
                return 0
            fi
            if ((age >= stall_seconds && $(date +%s) - last_evidence >= 15)); then
                printf '%s\tstalled: a probe unanswered for %s s\n' "$(now)" "$age" >> "$run_dir/stalls.tsv"
                evidence
                last_evidence=$(date +%s)
            fi
        fi
        sleep 5
    done
}

# freeze sends the host SIGQUIT, whose goroutine dump goes to its log, and ends
# the run: the dump is only whole once the process has exited, and the pod's
# restart loses the guest.
freeze() {
    touch "$run_dir/frozen"
    local pid
    for pid in $(pgrep -x sproutfs-host || true); do
        sudo -n kill -QUIT "$pid"
    done
    touch "$run_dir/.stop"
}

# summarize prints what the run measured: each phase, fio's reads, pgbench's
# load and run, and how long the guest took to answer its probes.
summarize() {
    python3 - "$run_dir" <<'SUMMARY'
import json, pathlib, re, sys

run = pathlib.Path(sys.argv[1])
print(f"sproutfs postgres benchmark: {run}")
for line in (run / "phases.tsv").read_text().splitlines():
    name, began, ended, status = line.split("\t")
    print(f"  {name:8} {float(ended) - float(began):8.1f} s  exit {status}")

def documents(text):
    decoder, at = json.JSONDecoder(), 0
    while (at := text.find("{", at)) >= 0:
        try:
            document, end = decoder.raw_decode(text, at)
        except ValueError:
            at += 1
            continue
        yield document
        at = end

file = run / "file.out"
if file.exists():
    for document in documents(file.read_text()):
        for job in document.get("jobs", []):
            for kind in ("read", "write"):
                side = job[kind]
                if not side.get("io_bytes"):
                    continue
                clat = side.get("clat_ns", {}).get("percentile", {})
                quantiles = " ".join(f"p{q.split('.')[0]} {clat[q] / 1e6:.1f} ms"
                                     for q in ("50.000000", "90.000000", "99.000000") if q in clat)
                print(f"  fio {job['jobname']} {kind}: {side['iops']:.0f}/s, "
                      f"{side['bw_bytes'] / 2**20:.0f} MiB/s, {quantiles}")

load = run / "load.out"
if load.exists() and (match := re.search(r"load_seconds (\d+)", load.read_text())):
    print(f"  pgbench -i -s 300: {match.group(1)} s")
bench = run / "run.out"
if bench.exists():
    text = bench.read_text()
    tps = re.search(r"tps = ([\d.]+) \(without initial", text)
    latency = re.search(r"latency average = ([\d.]+) ms", text)
    if tps and latency:
        print(f"  pgbench -c 16 -j 8 -T 120: {float(tps.group(1)):.0f} tps, {latency.group(1)} ms average")

probes = [line.split("\t") for line in (run / "probes.tsv").read_text().splitlines()]
seconds = sorted(float(p[1]) for p in probes)
if seconds:
    failed = sum(1 for p in probes if p[2] != "0")
    print(f"  probes: {len(seconds)}, {failed} failed, p50 {seconds[len(seconds) // 2]:.2f} s, "
          f"max {seconds[-1]:.1f} s, over 5 s {sum(s > 5 for s in seconds)}, over 30 s {sum(s > 30 for s in seconds)}")
stalls = run / "stalls.tsv"
if stalls.exists():
    print(stalls.read_text(), end="")
if (run / "frozen").exists():
    print("  FROZEN: the host was sent SIGQUIT; its goroutines are in its log")
SUMMARY
}

# finish stops the loops and keeps the host's log, the previous container's
# too where a freeze restarted it.
finish() {
    local status=$?
    touch "$run_dir/.stop"
    wait 2> /dev/null || true
    if ((plain)); then
        # Everything the phases left running, PostgreSQL among it, goes with
        # the slice; the bind mounts go after it.
        sudo -n systemctl stop sproutfs-plain.slice || true
        local point
        for point in proc sys dev; do
            [[ -z $plain_root ]] || sudo -n umount "$plain_root/$point" 2> /dev/null || true
        done
    else
        kubectl logs -n "$namespace" "$pod" --since-time="$run_began" > "$run_dir/host.log" 2>&1 || true
        kubectl logs -n "$namespace" "$pod" --previous > "$run_dir/host-previous.log" 2> /dev/null ||
            rm -f -- "$run_dir/host-previous.log"
        # The kubelet rotates a container's log; the rotated files hold what a
        # goroutine dump pushed out of the current one.
        sudo -n sh -c "cp /var/log/pods/${namespace}_${pod}_*/host/* '$run_dir/' 2> /dev/null" || true
    fi
    sudo -n chown -R "$(id -u):$(id -g)" "$run_dir"
    summarize > "$run_dir/summary.txt" 2>&1 || true
    cat "$run_dir/summary.txt"
    exit "$status"
}
trap finish EXIT

((plain)) || probe_loop &
watch_loop &

# --- the phases -------------------------------------------------------------------
# job runs one phase, the script on its standard input, detached in the guest,
# so that it outlives any one exec, and waits for it, polling. The script
# reaches the guest as base64, which no shell between here and there quotes.
# Its output is kept as <phase>.out; a phase that exits non-zero, or a run that
# froze, fails the run.
job() {
    local name=$1 script began deadline status
    script=$(base64 -w0)
    began=$(now)
    step "phase $name"
    # The agent kills an exec's process group as soon as its shell exits, so
    # the launch returns only once the job has said it started, which it does
    # from its own session.
    local body=": > /jobs/$name.started; sh -eu /jobs/$name.sh > /jobs/$name.out 2>&1; echo \$? > /jobs/$name.exit"
    target_run "mkdir -p /jobs && rm -f /jobs/$name.* && echo $script | base64 -d > /jobs/$name.sh"
    if ((plain)); then
        # A unit of the capped slice that stays active once its shell exits, so
        # that what the phase leaves running, PostgreSQL, stays in the slice.
        sudo -n systemd-run --quiet --expand-environment=no --unit="sproutfs-plain-$name-$(date +%s)" \
            --slice=sproutfs-plain.slice -p RemainAfterExit=yes \
            --setenv=PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin --setenv=HOME=/root \
            chroot "$plain_root" /bin/sh -c "$body"
        target_run "until [ -e /jobs/$name.started ]; do sleep 0.1; done"
    else
        target_run "setsid sh -c '$body' < /dev/null > /dev/null 2>&1 &
            until [ -e /jobs/$name.started ]; do sleep 0.1; done"
    fi
    deadline=$(($(date +%s) + phase_seconds))
    until status=$(target_run "cat /jobs/$name.exit" 2> /dev/null) && [[ -n $status ]]; do
        [[ ! -e $run_dir/frozen ]] || fail "the guest froze during $name"
        (($(date +%s) < deadline)) || fail "$name did not end in $phase_seconds s"
        sleep 10
    done
    target_run "cat /jobs/$name.out" > "$run_dir/$name.out" || true
    printf '%s\t%s\t%s\t%s\n' "$name" "$began" "$(now)" "$status" >> "$run_dir/phases.tsv"
    ((plain)) || ctl hosts > "$run_dir/$name.hosts.txt" 2>&1 || true
    ((status == 0)) || { tail -n 40 "$run_dir/$name.out" >&2; fail "$name exited $status"; }
    printf '%s took %s s\n' "$name" "$(since "$began")"
}

job file <<'PHASE'
mkdir -p /flat
fio --name=fill --filename=/flat/big --size=12G --rw=write --bs=1M --ioengine=psync --end_fsync=1 \
    --output-format=json --output=/jobs/fill.json
fio --output-format=json --output=/jobs/mixed.json \
    --name=readers --filename=/flat/big --size=12G --rw=randread --bs=8k --ioengine=psync \
        --numjobs=8 --group_reporting --time_based --runtime=120 \
    --name=log --filename=/flat/log --size=8G --rw=write --bs=64k --ioengine=psync \
        --rate=50m --time_based --runtime=120
cat /jobs/fill.json /jobs/mixed.json
rm -f /flat/big /flat/log
sync
PHASE
job layers <<'PHASE'
mkdir -p /layers
for i in 1 2 3; do cp -a /usr "/layers/usr$i"; done
sync
du -sh /layers
PHASE
job load <<'PHASE'
mkdir -p /flat/pgdata /flat/pglog
chown postgres:postgres /flat/pgdata /flat/pglog
chmod 700 /flat/pgdata
su postgres -s /bin/sh -c 'initdb -D /flat/pgdata -A trust -U postgres' > /dev/null
su postgres -s /bin/sh -c "pg_ctl -D /flat/pgdata -l /flat/pglog/postgres.log -w -t 600 start \
    -o '-c synchronous_commit=on -c shared_buffers=512MB -c max_wal_size=4GB -c listen_addresses= -c unix_socket_directories=/tmp'"
start=$(date +%s)
pgbench -h /tmp -U postgres -i -s 300 -q postgres
echo "load_seconds $(($(date +%s) - start))"
PHASE
job run <<'PHASE'
pgbench -h /tmp -U postgres -c 16 -j 8 -T 120 -P 30 postgres
PHASE
step 'done'
