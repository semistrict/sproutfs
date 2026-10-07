# The demo on one GCE VM

The demo runs on one disposable GCE VM. It runs five flows: boot, fork,
migration, recovery after a host is lost, and commands run inside the guests.
A single-node k3s cluster on the VM runs two host pods and one orchestrator.
The guests are Firecracker microVMs on the node's `/dev/kvm`. The bucket holds
every control record and checkpoint.

The main commands are `create`, `run` and `delete`. The others are optional.

## Prerequisites

- `gcloud`, authenticated, with a default project that can create an
  `n2-standard-8` instance with nested virtualization and one GCS bucket.
  `scripts/demo-gce.sh` names the instance `sproutfs-demo-1` and the bucket
  `sproutfs-demo-<project>`. It labels both `purpose=demo,lifecycle=temporary`
  and does not touch any resource without those labels.
- The Firecracker submodule, from which the container image is built:

  ```sh
  git submodule update --init third_party/firecracker
  ```

- `python3`, which packages the versioned source for the node. Everything else,
  including `kubectl`, runs on the demo VM.

The VM powers off after 24 hours without a command run against it, and GCE
deletes it after 24 hours of run time. Every command the script runs on the VM
renews that lease.

## Create

```sh
scripts/demo-gce.sh create
```

This command:

1. Creates the bucket and the VM.
2. Waits for the VM's startup script to set up the 12 GiB HugeTLB pool and k3s.
3. Writes to the bucket with the VM's credentials.
4. Checks that `/dev/kvm` and the huge pages are reachable from inside a pod.
5. Builds the container image and the two guest images on the node.
6. Imports the container image into the node's containerd.
7. Applies `deploy/` and waits for the pods.

The first run takes twenty to thirty minutes, mostly for the Firecracker build.
The 2026-09-14 run took twelve minutes, of which the Rust build took about
four. At the end you should see:

```
NAME                                    READY   STATUS    RESTARTS   AGE
sproutfs-host-<hash>-<a>                1/1     Running   0          4s
sproutfs-host-<hash>-<b>                1/1     Running   0          4s
sproutfs-orchestrator-<hash>-<c>        1/1     Running   0          14s
```

## Run

```sh
scripts/demo-gce.sh run
```

This runs the five flows and prints their timings. It exits non-zero on the
first check that fails and prints the guest's console output at that point. It
runs `scripts/lib/demo-run.sh` on the node, which drives the deployment through
the `sproutfsctl` in the deployment's image.

1. **Create.** `sproutfsctl create --template alpine` boots a VM. The run waits
   for the guest's banner on the serial console, runs `uname -a` in the guest,
   and sets a shell variable there. A later flow that still reads the variable
   is talking to the same guest, not a new boot.

   A create forks the template's root checkpoint and returns once the new VM
   has published its own root checkpoint. Until then the VM cannot be recovered
   or forked. The root is published between the fork and the boot, when the
   VM's bytes are still the template's, so it seals and uploads no pages. The
   create prints it as the `root` time.
2. **Fork.** `sproutfsctl fork <vm> --count 5` forks the running VM five times
   on its host, from one pause of the parent. The fork waits for no upload.
   Each child must answer a question on its console, and the host's shared page
   count must rise.

   The run then forks with `--to <the other host>`. The child pulls the pages
   that no checkpoint of the parent holds from the parent's peer server, and
   the parent keeps running. The `SERVED` column of `sproutfsctl hosts` counts
   those pages and must rise. The run then closes the forks to free room.
3. **Migrate.** `sproutfsctl migrate <vm> --to <the other host>` moves the VM.
   The guest must still hold the variable. The run prints the pause and the
   post-copy stream time.
4. **Lose a host.** `sproutfsctl kill-host <the host running it>` deletes that
   pod with no grace period, which skips the drain. `sproutfsctl recover <vm>`
   reopens the VM on the surviving host from its last checkpoint, and the guest
   must still hold the variable. The run takes a checkpoint
   (`sproutfsctl capture`) before the kill. A real loss rewinds the guest's disks
   by about the checkpoint interval, 60 s, and at most the loss window, five
   minutes, and the guest cold boots.

   Recovery reopens the VM only when:
   - the deleted pod is no longer listed, and
   - every pod still listed answered and said it does not run the VM.

   A host that did not answer may still be running the guest, so recovery is
   refused while any host is silent. `sproutfsctl recover <vm> --force`
   overrides this.

   Recovery is also refused, even with `--force`, while the VM is between two
   hosts:
   - a source that handed the VM over still serves the pages no checkpoint
     holds, or
   - the orchestrator's table row says an operation on it is in flight.

   A row whose operation died ages off and stops blocking recovery.
5. **Use it.** `sproutfsctl exec <vm> -- <command>` runs a command inside the
   guest and prints its output. The run does this:
   - in the VM;
   - in a fork, which must hold a file the parent wrote before the fork;
   - after moving the VM to the other host, where a file written before the
     move must still be present.

   The guest has no network. The command travels over the VM's virtio-vsock
   device, which is part of the VMM state, so forks and migrations keep it.

A run on 2026-09-14 on `n2-standard-8` in `us-east4-a`:

```
create to prompt      3.96s
fork pause (5)        0.464s
fork total (5)        1.512s
cross-host fork       pause 0.080s, total 0.798s
migration pause       0.275s
migration stream      0.450s
recover to running    1.01s
exec in a booted VM   0.13s
fork to exec          1.52s
migration pause (5)   0.327s
move to exec          0.14s

All five flows passed.
```

The create reported `template 1.25s, fork 0.07s, boot 0.07s, root 0.15s`. The
template checkpoint already existed on that host, so 1.25 s is the time to read
it.

The fork pause is the VMM state capture and the seal; the total adds the
children's boots. The pause covers the whole fan-out and is the only number
that grows with the number of children: 0.464 s for five, against 0.080 s for
the single cross-host fork. The cross-host fork also post-copied one interval's
dirty set: 62 pages, all unpublished.

The migration moved the same 62 pages. The recovery reopened the VM from
checkpoint `8589934593`. The cross-host fork raised the parent host's `SERVED`
count from 0 to 62, and the five same-host forks raised its `SHARED` page count
from 0 to 310.

## Workload

```sh
scripts/demo-gce.sh workload
```

This is a measurement, not a demonstration. It measures the object-store
traffic of checkpoints while a guest is in use.

It boots a VM from the `workload` template: the Alpine image plus git, ripgrep,
Node, pnpm and three MIT-licensed TypeScript repositories, with their
dependencies in a pnpm store inside the image. The run checkpoints the VM,
forks it, and drives the forks through these phases:

- searching with `rg`;
- reading history with `git`;
- an offline `pnpm install`;
- a build.

Each phase ends with a checkpoint of every VM. A final idle phase measures an
idle guest's interval checkpoint.

`FORKS_BASE` is the number of forks taken from the base image's checkpoint.
`FORKS_PER_REPO` is the number taken from each of those after it has installed
its dependencies. They default to 2 and 1, which fit a host's 5 GiB arena:

```sh
FORKS_BASE=3 FORKS_PER_REPO=2 scripts/demo-gce.sh workload
```

Every fork lands on its parent's host, so the run migrates workers to spread
them over the ready hosts. This node supports at most three guests installing
at once, because both host pods' spill files are on the one boot disk; at four,
the guests stop answering. See
[the larger setting](measurements-2026-09-14-workload.md#the-larger-setting).

Restart the hosts before a run whose numbers you want to keep. Their
object-store counters and page counts accumulate from process start, and the
run reads them.

```sh
scripts/demo-gce.sh kubectl rollout restart -n sproutfs deployment/sproutfs-host
```

A rollout takes down one host pod at a time, since the two pods together
request the node's whole HugeTLB pool. A pod going away drains its VMs to the
other pod, so no guest loses writes.

Every checkpoint of every VM is one line in the host's log, with the VM, the
sequence, the 2 MiB pages the pause sealed, the bytes uploaded, the objects
written, and the pause and upload durations. `sproutfsctl store` reports each
host's object-store counters; the difference between two readings is the cost
of a phase. The run prints the tables and copies everything it recorded to
`.workload-runs/` on your machine.
[What a checkpoint costs under a workload](measurements-2026-09-14-workload.md)
writes up one run.

## The soak

```sh
scripts/demo-gce.sh soak
```

The soak performs many operations with work between them:

- forks on the same host and on the other host;
- migrations;
- stops and starts;
- a host loss.

After each operation it checks that every guest's memory and disk hold the
bytes that guest wrote, as the simulated campaigns do, but over Firecracker,
KVM, the pager, GCS and k3s.

The checks use `sproutfs-guest-witness`, which both guest images include. The
witness holds a resident buffer and a file of the same size, 256 MiB each by
default, both filled with the pattern for a `(seed, step)`. At each step it
mutates a scattered fraction of both, then checks every byte of memory and
every byte of the file, reading the file again from disk. The pattern is a pure
function of the seed, the step and the page, so the script holds the expected
state and the guest never does.

Each round goes over every running VM in an order drawn from the seed:

1. Mutate and check.
2. Fork one parent on its own host and on the other host. Check every child
   against its parent's `(seed, step)`, then give the child its own seed.
3. Migrate the round's share of VMs, and check before and after.
4. Stop the round's share, and check that no host runs them. Start them on the
   other host, and check them.
5. Delete a seeded share, so the population stays within what two hosts admit.

A placement refused for want of room is recorded, not failed: the orchestrator
says "no host is available" (503), or a host says its capacity is exhausted. A
placement that fails for any other reason fails the run.

A seeded share of the restarts are cold. The VM comes back without its memory,
so only its disk is checked, with `witness check --disk-only`. The witness is
then filled again under a new seed. A share of those cold starts also resize
the VM:

- a larger memory;
- a larger root volume;
- `witness grow /` in the guest, to use the pages the volume gained.

The disk is checked again after the grow.

Once per soak, at a round the seed picks, the run captures every VM and kills
one host pod without grace. It recovers that host's VMs on the other host and
checks each against its last checkpoint. This happens at the start of the
round, before any mutation, because each host checkpoints on its own interval
and a kill after a mutation would come back at a state the script cannot
identify.

At the end the run deletes every VM and runs `sproutfsctl check` over the
bucket; only the templates and the checkpoints the deleted VMs pinned may
remain. It prints what each round did and cost (fork pauses, migration pauses
and streams, stops and starts), every check, and each host's object-store
counters. It copies everything to `.workload-runs/soak-<timestamp>` on your
machine, and exits non-zero on the first check that fails.

`SOAK_SEED` reproduces a run's shape: the order the VMs are walked, which VMs
each round's shares fall on, which round loses a host, and every witness seed.
The shares are set by `SOAK_ROUNDS`, `SOAK_FORKS_LOCAL`, `SOAK_FORKS_REMOTE`,
`SOAK_MIGRATIONS`, `SOAK_STOPS` and `SOAK_MAX_VMS`. `SOAK_WITNESS_BYTES` sets
how much of each guest the witness covers. `SOAK_COLD_STARTS` and
`SOAK_COLD_RESIZES` are one-in-N rates: zero never draws, and one draws every
time. `SOAK_COLD_MEMORY` and `SOAK_COLD_DISK` set the sizes a resized VM comes
back with:

```sh
SOAK_SEED=7 SOAK_ROUNDS=12 scripts/demo-gce.sh soak
```

## Stopping and starting a VM

```sh
scripts/demo-gce.sh kubectl exec -n sproutfs deploy/sproutfs-orchestrator -- \
    sproutfsctl stop vm-01k...
scripts/demo-gce.sh kubectl exec -n sproutfs deploy/sproutfs-orchestrator -- \
    sproutfsctl start vm-01k... --to sproutfs-host-7f9c4-qk2wd
```

A stop publishes the guest's disks, closes the VMM process, returns the pages
and releases the handle. The VM is then its control record and its objects:
still listed, on no host. A start boots it over the disks the stop published,
on the named host or else on the ready host whose guests have promised the
least of its arena. `stop --suspend` also publishes the guest's memory and VMM
state, and the next start resumes the guest.

Unlike a host loss, a stop loses no writes. A start needs none of the evidence
`recover` needs, because a stopped VM has no host that opening could fence. A
start is still refused while anything shows the VM is between hosts:

- a host that runs it;
- two hosts that each report holding it;
- a host still serving the pages that no checkpoint has;
- an operation the orchestrator started that is still in flight.

A stop is refused, as a delete is, for a VM whose pages a fork point holds
sealed: a child on another host is reading the unpublished pages from the
memory the stop would release. Take the children's root checkpoints first.

## Starting a VM cold, and resizing it

```sh
scripts/demo-gce.sh kubectl exec -n sproutfs deploy/sproutfs-orchestrator -- \
    sproutfsctl start vm-01k... --cold
scripts/demo-gce.sh kubectl exec -n sproutfs deploy/sproutfs-orchestrator -- \
    sproutfsctl start vm-01k... --cold --memory 1G --disk 8G
```

A cold start brings a stopped VM back without its memory, for example because
the guest is wedged, a kernel or init changed on the disk, or the memory is not
worth keeping.

The host discards every page of the VM's RAM and the VMM state in one
checkpoint, then boots the kernel from the root volume as the last checkpoint
published it. The guest's filesystem sees a power cut after that checkpoint,
and its journal recovers. The discarded pages are reclaimed by the usual set
difference.

A cold start applies only to a stopped VM. It is also the only time a VM's
shape can change. `--memory` sets the RAM to any size the host admits, larger
or smaller. `--disk` grows the root volume; the new pages read as zeroes, and
the guest takes them with the witness's grow command:

```sh
scripts/demo-gce.sh kubectl exec -n sproutfs deploy/sproutfs-orchestrator -- \
    sproutfsctl exec vm-01k... -- sproutfs-guest-witness grow /
```

resize2fs cannot do this: its online path opens the block device read-write,
which a 6.18 kernel refuses for a mounted device, so it fails with EBUSY. The
witness issues `EXT4_IOC_RESIZE_FS` on a descriptor of the mount point instead.

Shrinking the disk is refused. Both flags are refused without `--cold`. After a
resize, the VM's committed RAM is its own, not its template's, and every later
placement of the VM and its children is admitted against it.

## The failure paths

```sh
scripts/demo-gce.sh fixes
```

This runs `scripts/lib/demo-fixes.sh` on the node and checks that:

- A cross-host fork whose destination pod is deleted leaves the parent running
  and checkpointing, not sealed indefinitely.
- A recovery is refused while the VM's host is alive and answering.
- A recovery is taken once that pod is gone.
- A VMM killed underneath its host leaves a diagnostic that names the VM, the
  cause and the console tail, and the host forgets the VM.
- A rollout of the hosts migrates every VM rather than losing it.
- A same-host fan-out that fails part way takes back the children it started,
  and the parent checkpoints again.
- A host lost in the middle of a migration leaves every VM running or
  reopenable. A lost destination is retried on the pod that replaces it, and a
  lost source ends the receive.

## The merged features, and the arena mode

```sh
scripts/demo-gce.sh features
scripts/demo-gce.sh arena
SPROUTFS_DEMO_ARENA=isolated scripts/demo-gce.sh redeploy
```

`features` runs `scripts/lib/demo-features.sh`. It exercises kept checkpoints,
creates from a kept, a stopped and a captured VM, ephemeral disks, pulls, a
template imported at runtime, and a tenant's template, VM and stored bytes, and
checks each.

`arena` runs `scripts/lib/demo-arena.sh` against the mode the hosts run: a
fan-out, three 1 GiB checkpoints with the host's CPU, and restores. A redeploy
with `SPROUTFS_DEMO_ARENA` set changes the mode. The 2026-09-26 run compares
[both modes](measurements/arena-modes-2026-09-26.md).

`arena-worst` runs `scripts/lib/demo-arena-worst.sh`: the isolated arena's worst
cases beside the shared arena's, both modes and both RAM pages, in at most half
an hour. It sets the hosts to each mode and puts them back at the end. Its
[2026-09-26 run](measurements/arena-worst-case-2026-09-26.md) took 19 minutes.

## Looking around

```sh
scripts/demo-gce.sh kubectl get pods -n sproutfs
scripts/demo-gce.sh kubectl logs -n sproutfs -l app.kubernetes.io/name=sproutfs-host
scripts/demo-gce.sh ssh                     # a shell on the node
scripts/demo-gce.sh status                  # what exists, cloud side and node side
```

Placement chooses the host whose guests have promised the least of its arena,
counting the RAM promised to running guests, not the VM count or what is
resident. The arena is a cache: a page of a VM that was migrated away or
deleted stays until the memory is needed, so residency would make a host look
full after any work and leave a drain nowhere to go.

A create or fork whose children fit on no host is refused with 503, also on the
parent's own host: children share the pages they have not diverged from, but
each may still diverge. A named migration destination without room for the
guest is refused before the guest is stopped.

To drive the deployment by hand, use the `sproutfsctl` in the image:

```sh
scripts/demo-gce.sh kubectl exec -n sproutfs deploy/sproutfs-orchestrator -- sproutfsctl hosts
scripts/demo-gce.sh kubectl exec -i -n sproutfs deploy/sproutfs-orchestrator -- \
    sproutfsctl console <vm> --for 8s
```

A console session ends when its input ends, so a scripted read needs `--for`.

To run a local build against the orchestrator, forward it out of the cluster:

```sh
scripts/demo-gce.sh ssh 'sudo KUBECONFIG=/etc/rancher/k3s/k3s.yaml \
    kubectl port-forward -n sproutfs service/sproutfs-orchestrator 8080:8080'
SPROUTFS_ORCHESTRATOR=http://localhost:8080 \
    SPROUTFS_API_TOKEN=$(scripts/demo-gce.sh kubectl get secret -n sproutfs \
        sproutfs-api-token -o jsonpath='{.data.token}' | base64 -d) \
    go run ./cmd/sproutfsctl hosts
```

Both APIs require the deployment's token. `sproutfsctl` run through
`kubectl exec` in the orchestrator pod inherits it from the container's
environment.

To run a command in a guest:

```sh
scripts/demo-gce.sh kubectl exec -n sproutfs deploy/sproutfs-orchestrator -- \
    sproutfsctl exec <vm> -- 'uname -a; cat /proc/meminfo | head -3'
```

`exec` stops parsing at the `--`, so the guest receives its quoting intact. The
command goes to the host the orchestrator's table lists for the VM, and from
there over the VM's vsock to an agent the guest's init starts. The agent:

- runs each command in its own process group, so its deadline ends everything
  it started;
- keeps up to 1 MiB of output per stream and drops the rest;
- runs at most four commands at once and refuses the rest with 429.

`sproutfsctl list` shows the table: which host each VM is on, and whether it is
being created, running, migrating between a named pair, stopped, or being
recovered. A VM exists as long as its control record exists; a delete removes
the record even if the VM's checkpoints are still pinned.

A console read starts at the requested offset and returns at most 256 KiB of
the VMM's output and the guest's serial console. The host keeps the newest
1 MiB in memory. A read from before that starts at the oldest byte retained and
reports it. `deploy/README.md` documents the full host and orchestrator API.

## A guest larger than 3 GiB

x86_64 reserves 3 GiB to 4 GiB for MMIO, so a VM with more than 3 GiB of RAM
has two guest memory regions over the one RAM volume
([vm-memory](vm-memory.md)). Only an x86_64 node exercises this; the Lima
instance is aarch64. On a demo cluster that is already up:

```sh
scripts/demo-gce.sh bigguest
```

It:

1. Boots a guest and cold starts it with 3.5 GiB of RAM, which fits a host's
   3.75 GiB RAM arena.
2. Checks the guest's e820 map for a second usable range at 4 GiB.
3. Writes 3200 MiB of a non-zero pattern, more than fits below the gap.
4. Reads the tail back in the parent, in a fork on the other host (the
   parent's host has no room for a second guest this size), and after a
   migration, comparing each read with an md5 the node computes over the same
   pipeline.

It does not change the deployment. The 2026-09-26 run passed:
[the measurements](measurements/gce-2026-09-26.md).

By hand:

```sh
ctl() { scripts/demo-gce.sh kubectl exec -n sproutfs \
    deploy/sproutfs-orchestrator -- sproutfsctl "$@"; }
vm=$(ctl create --template alpine | cut -d' ' -f1)

# 3.5 GiB of guest RAM, from a cold boot. SPROUTFS_VM_MEMORY_BYTES is read
# only when an image is imported into a template, so changing it does not
# resize a VM created from an image already imported.
ctl stop "$vm"
ctl start "$vm" --cold --memory 3758096384

# Two usable e820 ranges, the second starting at 4 GiB.
ctl exec "$vm" -- 'head -1 /proc/meminfo; dmesg | grep -i usable'

# 3200 MiB of a non-zero pattern, and the md5 to compare every read against.
ctl exec "$vm" --timeout 300s -- 'mkdir -p /mnt/big
    mount -t tmpfs -o size=3400M tmpfs /mnt/big
    dd if=/dev/zero bs=1M count=3200 2>/dev/null | tr "\0" "Z" > /mnt/big/f
    md5sum /mnt/big/f'
scripts/demo-gce.sh ssh 'dd if=/dev/zero bs=1M count=3200 2>/dev/null |
    tr "\0" "Z" | md5sum'

ctl capture "$vm"
fork=$(ctl fork "$vm" --count 1 --to <the other host> | awk 'NR==2 {print $1}')
ctl exec "$fork" --timeout 300s -- \
    'dd if=/mnt/big/f bs=1M skip=2944 count=256 2>/dev/null | md5sum'
ctl migrate "$vm" --to <the other host>
ctl exec "$vm" --timeout 300s -- \
    'dd if=/mnt/big/f bs=1M skip=2944 count=256 2>/dev/null | md5sum'
```

The fork and the moved VM read only the tail, the part that must be above the
gap, because the guest's agent kills a command after ten minutes and the
orchestrator waits ten minutes on a host.

## Changing something

```sh
scripts/demo-gce.sh redeploy
```

This ships the current source, rebuilds the image and restarts the pods. The
Dockerfile copies the Rust sources first, so a change to the Go commands or to
`deploy/` reuses the cached VMM and kernel stages. The workload guest image on
the node is kept. A redeploy takes a few minutes, against about half an hour
for the first build.

## Clean up

```sh
scripts/demo-gce.sh delete
```

This deletes the VM, its boot disk, and the bucket with its objects, and fails
if any of them remains.
