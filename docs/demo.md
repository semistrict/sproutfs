# The demo on one GCE VM

The demo runs on one disposable GCE VM. It runs the four flows sproutfs is
built for: boot, fork, migration, and recovery after a host is lost. A fifth
flow uses the VMs instead of only observing them. A single-node k3s cluster on
the VM runs two host pods and one orchestrator. The guests are Firecracker
microVMs on the node's `/dev/kvm`. The bucket holds every control record and
checkpoint.

The main commands are `create`, `run` and `delete`. Five more are optional:
`fixes`, `bigguest`, `workload`, `soak` and `redeploy`.

## Prerequisites

- `gcloud`, authenticated, with a default project that can create an
  `n2-standard-8` instance with nested virtualization and one GCS bucket.
  `scripts/demo-gce.sh` names the instance `sproutfs-demo-1` and the bucket
  `sproutfs-demo-<project>`. It labels both `purpose=demo,lifecycle=temporary`
  and does not touch any resource without those labels.
- The Firecracker submodule. The container image is built from it:

  ```sh
  git submodule update --init third_party/firecracker
  ```

- `python3`, which packages the versioned source for the node. You need
  nothing else locally. The container image, the guest images and the five
  flows are all built and run on the demo VM. You do not need `kubectl` on your
  machine.

The VM powers off after 24 hours without a command run against it. GCE deletes
it after 24 hours of run time, regardless of what it is doing. A forgotten demo
therefore costs at most one day. Every command the script runs on the VM renews
that lease.

## Create

```sh
scripts/demo-gce.sh create
```

This command does the following:

1. Creates the bucket and the VM.
2. Waits for the VM's startup script to set up the 12 GiB HugeTLB pool and k3s.
3. Writes to the bucket with the VM's credentials.
4. Checks on the newly created VM that `/dev/kvm` and the huge pages are
   reachable from inside a pod.
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

This runs the five flows non-interactively and prints their timings. It exits
non-zero on the first check that fails, and prints the guest's console output
at that point. `scripts/lib/demo-run.sh` is the script it runs on the node. That
script drives the deployment through `sproutfsctl`, which is included in the
deployment's image, so nothing is forwarded out of the cluster.

The steps and their expected results:

1. **Create.** `sproutfsctl create --template alpine` boots a VM. The run waits
   for the guest's banner on the serial console. It then runs `uname -a` in the
   guest, so the answer comes from the guest. Next it sets a shell variable in
   the guest. Only that guest's memory holds the variable. If the VM still
   returns the variable later, it is the same guest and not a new boot.

   A create is a fork of the template's root checkpoint. It does not return
   until the new VM has published its own root checkpoint. Before that, the VM
   runs only on that host, so it cannot be recovered or forked. The root
   checkpoint is published between the fork and the boot, where it costs
   nothing. At that point the VM's bytes are still the template's, so the
   checkpoint seals no pages and uploads none. The create prints this as the
   `root` time.
2. **Fork.** `sproutfsctl fork <vm> --count 5` forks the running VM five times
   on its host. All five children come from one pause of the parent, so the
   parent pauses once. A fork is a migration handoff from a VM that keeps
   running, and it publishes nothing. Each child must answer a question on its
   console. The host's shared page count must rise. This shows that the
   children inherited the parent's memory instead of copying it.

   The run then repeats the fork with `--to <the other host>`. The child starts
   on the other host. It pulls the pages that no checkpoint of the parent holds
   from the parent's page server, as a migration destination does. The parent
   keeps running on its host. The `SERVED` column of `sproutfsctl hosts` counts
   the pages the parent host's page server has handed over, and it must rise.
   The run then closes the forks, so the later flows run on a host with free
   room.
3. **Migrate.** `sproutfsctl migrate <vm> --to <the other host>` moves the VM.
   The guest must still hold the variable set before the move. This shows that
   the VM was migrated and not rebooted. The run prints the pause and the
   post-copy stream time.
4. **Lose a host.** `sproutfsctl kill-host <the host running it>` deletes that
   pod with no grace period. This skips the drain, so it is a host loss and not
   a shutdown. `sproutfsctl recover <vm>` reopens the VM on the surviving host
   from its last checkpoint. The guest must still hold the variable. The run
   takes an explicit checkpoint (`sproutfsctl capture`) before the kill. A real
   loss rewinds the guest by at most the checkpoint interval, 60 s.

   Recovery reopens the VM only when there is evidence that its host is gone.
   The evidence is:
   - the deleted pod is no longer listed, and
   - every pod that is still listed answered and said it does not run the VM.

   A host that did not answer is not evidence, because its guest may still be
   running normally. A recovery is therefore refused while any host is silent.
   An operator who knows the host is gone can override this with
   `sproutfsctl recover <vm> --force`.

   A VM can still be held by a host that does not run it, in two cases:
   - A source that has handed a VM over still serves the pages that no
     checkpoint of the VM holds.
   - An operation the orchestrator started is in flight for as long as its
     table row says so.

   In both cases the VM is between two hosts, not lost. A recovery is refused
   while either case holds, even with `--force`. The reason is that `--force` is
   evidence about a host's process and says nothing about a handover. If the
   operation behind an in-flight row actually died, the row ages off and stops
   blocking recovery.
5. **Use it.** `sproutfsctl exec <vm> -- <command>` runs a command inside the
   guest and prints its output. The run does this three times:
   - in the VM;
   - in a fork of the VM, which must hold a file the parent wrote before the
     fork;
   - after moving the VM to the other host, through the same name, with a file
     written before the move still present.

   No network address is used, because the guest has no network. The command
   travels over the VM's virtio-vsock device. That device is part of the VMM
   state that a restore replays, so forks and migrations keep the channel.

A run on an `n2-standard-8` looks like this. This run is from 2026-09-14 on
`n2-standard-8` in `us-east4-a`, with all five flows passing:

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

The create in that run reported
`template 1.25s, fork 0.07s, boot 0.07s, root 0.15s`. The template checkpoint
already existed on that host, so the 1.25 s is the time to read it. The root is
the new VM's first checkpoint. It costs almost nothing because nothing has run
yet.

Five forks cost the parent one pause, because they share one fork point and
none of them publishes anything. The pause is the VMM state capture and the
seal. The total is the pause plus the children's boots. The pause covers the
whole fan-out, so it is the only number here that grows with the number of
children: 0.464 s for five, against 0.080 s for the single cross-host fork. A
fork placed on the other host costs that pause plus the post-copy of one
interval's dirty set. This one moved 62 pages, and all 62 were unpublished.

The migration moved the same 62 pages, and the guest kept the shell variable
set before the move. The recovery reopened the VM from checkpoint `8589934593`
on the surviving host, and the guest still held that variable. The cross-host
fork raised the parent host's `SERVED` count from 0 to 62. The five same-host
forks raised its `SHARED` page count from 0 to 310. This shows that the
children inherited the parent's memory instead of copying it.

## Workload

```sh
scripts/demo-gce.sh workload
```

This is a sixth run. It is the only one that is a measurement and not a
demonstration. It measures the object-store traffic that the checkpoint model
costs while a guest is in use.

It boots a VM from the `workload` template. That template is the same Alpine
image plus git, ripgrep, Node, pnpm and three MIT-licensed TypeScript
repositories. The repositories' dependencies are in a pnpm store inside the
image. The run checkpoints the VM, forks it, and drives the forks through these
phases:

- searching with `rg`;
- reading history with `git`;
- an offline `pnpm install`;
- a build.

Each phase ends with an explicit checkpoint of every VM. A final phase does
nothing, so the numbers also include the cost of an idle guest's interval
checkpoint.

`FORKS_BASE` is the number of forks taken from the base image's checkpoint.
`FORKS_PER_REPO` is the number of forks taken from each of those after it has
installed its dependencies. They default to 2 and 1, which fit a host's 5 GiB
arena. Raise them to measure the cost of more sharing:

```sh
FORKS_BASE=3 FORKS_PER_REPO=2 scripts/demo-gce.sh workload
```

Every fork lands on its parent's host. Without intervention, a run would pile
onto the host its base was created on, while the other host sat idle. The run
therefore spreads the workers over the ready hosts, with a migration after the
single fork point that starts them. Even so, this node supports at most three
guests installing at once, because the two host pods' spill files are on the
one boot disk. At four, the guests stop answering.
[The larger setting](measurements-2026-09-14-workload.md#the-larger-setting)
has the details.

Restart the hosts before a run whose numbers you want to keep. Their
object-store counters and page counts accumulate from process start, and the
run reads their logs. A host that has already carried a run reports that run's
work together with the current run's work.

```sh
scripts/demo-gce.sh kubectl rollout restart -n sproutfs deployment/sproutfs-host
```

A rollout takes down one host pod at a time. The two pods together request the
node's whole HugeTLB pool, so there is no room for a third. When a pod goes
away, its VMs drain to the pod that stays up. Nothing is lost and nothing
rewinds. A guest keeps what it wrote since its last checkpoint, because it was
moved and not restarted.

Every checkpoint of every VM is one line in the host's log. The line records:

- the VM;
- the sequence;
- the 2 MiB pages the pause sealed;
- the bytes uploaded;
- the objects written;
- the pause and upload durations.

`sproutfsctl store` reports each host's object-store counters. The difference
between two readings is the cost of a phase. The run joins the log lines and
the counters, prints the tables, and copies everything it recorded to
`.workload-runs/` on your machine.
[What a checkpoint costs under a workload](measurements-2026-09-14-workload.md)
writes up one run.

## The soak

```sh
scripts/demo-gce.sh soak
```

The flows above prove each operation once. The soak performs many operations
with work between them:

- forks on the same host and on the other host;
- migrations;
- stops and starts;
- a host loss.

After each operation it asks every guest whether its memory and its disk still
hold the exact bytes that guest wrote. The simulated campaigns do the same with
model guests. The soak applies the same checks over Firecracker, KVM, the
pager, GCS and k3s.

The checks use `sproutfs-guest-witness`, which both guest images include next
to the agent. The witness holds a resident buffer and a file of the same size,
256 MiB each by default. Both are filled with the pattern for a `(seed, step)`.
At each step the witness mutates a scattered fraction of both. It then checks
every byte of memory and every byte of the file. It reads the file again from
the disk instead of comparing it with the buffer. The pattern is a pure
function of the seed, the step and the page. The script therefore holds the
expected state, and the guest never does. Nothing about what a VM should hold
is carried across a fork, a migration or a stop, so a guest cannot confirm its
own wrong state.

Each round goes over every running VM in an order drawn from the seed, and does
the following:

1. Mutate and check.
2. Fork one parent on its own host and on the other host. Check every child
   against its parent's `(seed, step)` before anything else, then give the
   child its own seed.
3. Migrate the round's share of VMs, and check before and after.
4. Stop the round's share, and check that no host runs them. Start them on the
   other host, and check them.
5. Delete a seeded share, so the population stays within what two hosts admit.

If the deployment refuses a placement with 503, the run records the refusal and
does not fail.

A seeded share of the restarts are cold. The VM comes back without its memory,
so only its disk is checked, with `witness check --disk-only`. That command
reads the file without a resident witness, because the process that held the
buffer was lost with the memory. The witness is then filled again under a new
seed before anything mutates that VM. A share of those cold starts also resize
the VM:

- a larger memory;
- a larger root volume;
- `witness grow /` in the guest, to use the pages the volume gained.

The disk is checked again after the grow, because a filesystem grown over the
witness file must still hold it.

Once per soak, at a round the seed picks, the run captures every VM and kills
one host pod without grace. It recovers that host's VMs on the other host and
checks each one against what its last checkpoint published. This happens at
the start of the round, before anything in the round has mutated a guest. The
reason is that every host checkpoints on its own interval. A kill after a
mutation would come back at a state the script cannot identify. A check against
a state the guest was never in would fail without giving any information.

At the end the run deletes every VM and runs `sproutfsctl check` over the
bucket. At that point only the templates and the checkpoints the deleted VMs
pinned may remain. The run prints what each round did and what it cost: the
fork pauses, the migration pauses and streams, the stops and the starts. It
also prints every check and each host's object-store counters. It copies
everything it recorded to `.workload-runs/soak-<timestamp>` on your machine. It
exits non-zero on the first check that fails.

`SOAK_SEED` reproduces a run's shape:

- the order in which the VMs are walked;
- which VMs each round's shares fall on;
- which round loses a host;
- every witness seed.

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

A stop ends a running VM on purpose and keeps the VM. The host running it
publishes the guest's disks, closes the VMM process, returns the pages and
releases the handle. The VM then consists only of its control record and its
objects. It is still listed, but on no host. A start opens it again at the
disks the stop published, and boots it over them. It opens on the named host,
or otherwise on the ready host whose guests have promised the least of its
arena. `stop --suspend` also publishes the guest's memory and VMM state, and
the start after it resumes the guest where it stopped.

That is the only difference between a stop and a host loss. A host loss also
loses the writes since the last checkpoint. It is also why a start needs none
of the evidence that `recover` needs. `recover` must prove the VM's host is
gone, because opening takes the control record's epoch and would fence a guest
that is still running normally. A stopped VM has no such host. A start is still
refused by anything that shows the VM is between hosts and not stopped:

- a host that runs it;
- two hosts that each report holding it;
- a host still serving the pages that no checkpoint has;
- an operation the orchestrator started that is still in flight.

A stop is refused for a VM whose pages a fork point holds sealed, in the same
way a delete is. A child on another host is reading the pages that no
checkpoint holds from the memory the stop would release. Take the children's
root checkpoints first.

## Starting a VM cold, and resizing it

```sh
scripts/demo-gce.sh kubectl exec -n sproutfs deploy/sproutfs-orchestrator -- \
    sproutfsctl start vm-01k... --cold
scripts/demo-gce.sh kubectl exec -n sproutfs deploy/sproutfs-orchestrator -- \
    sproutfsctl start vm-01k... --cold --memory 1G --disk 8G
```

A cold start brings a stopped VM back without its memory. The reasons are the
same as for rebooting any machine:

- the guest is wedged;
- a kernel or init changed on the disk;
- the guest does not need to keep its memory, so you avoid paying for it.

The host discards every page of the VM's RAM, and the VMM state, in one
checkpoint. It then boots the kernel from the root volume, which holds what the
last checkpoint published. The guest's filesystem therefore sees the boot as a
power cut after that checkpoint, and its journal recovers as a journal normally
does. The pages the memory held are reclaimed by the usual set difference.

A cold start applies only to a stopped VM. Stop a running VM first, as you
would before a warm start.

A cold boot is also the only time a VM's shape can change, because no memory
state describes the shape any more. `--memory` sets the RAM to any size the
host admits, larger or smaller. `--disk` grows the root volume. The new pages
read as zeroes, and the guest takes them with the guest witness's grow command:

```sh
scripts/demo-gce.sh kubectl exec -n sproutfs deploy/sproutfs-orchestrator -- \
    sproutfsctl exec vm-01k... -- sproutfs-guest-witness grow /
```

The witness does this instead of resize2fs. The online path of resize2fs opens
the block device read-write. A 6.18 kernel refuses a write open of a mounted
device. On a guest whose root filesystem is on that device, resize2fs therefore
fails with EBUSY before it starts. The witness's grow issues
`EXT4_IOC_RESIZE_FS` on a descriptor of the mount point instead, so it does not
open the device.

Shrinking the disk is refused, because the volume cannot remove the end of a
filesystem. Both flags are refused without `--cold`. After a resize, the VM's
committed RAM is its own and no longer its template's. Every later placement of
the VM, including its forks' children, is admitted against that value.

## The failure paths

```sh
scripts/demo-gce.sh fixes
```

`run` shows the five flows working. This command shows four behaviours when
they fail. Only a live cluster can show them:

- A cross-host fork whose destination pod is deleted leaves the parent running
  and checkpointing, not sealed indefinitely.
- A recovery is refused while the VM's host is alive and answering.
- A recovery is taken once that pod is gone.
- A VMM killed underneath its host leaves a diagnostic that names the VM, the
  cause and the console tail. The host then forgets the VM.

`scripts/lib/demo-fixes.sh` is the script it runs on the node.

## Looking around

```sh
scripts/demo-gce.sh kubectl get pods -n sproutfs
scripts/demo-gce.sh kubectl logs -n sproutfs -l app.kubernetes.io/name=sproutfs-host
scripts/demo-gce.sh ssh                     # a shell on the node
scripts/demo-gce.sh status                  # what exists, cloud side and node side
```

Placement chooses the host whose guests have promised the least of its arena.
It does not choose the host running the fewest VMs. A guest's cost to a host is
its RAM, so one 2 GiB workload guest uses more of a host than three 512 MiB
guests. Placement counts the RAM promised to the running guests, not how much
of the arena is resident. The arena is a cache. A page of a VM that was
migrated away or deleted stays in it until the memory is needed. If placement
measured residency, a host would look full as soon as it had done any work, and
a drain would have nowhere to go.

A create or fork whose children do not fit on any host is refused with 503. It
is not started on the emptiest host, where it would be lost when its memory
failed to attach. This also applies on the parent's own host. Children share
the pages they have not diverged from, but they do not share the promise that
they may diverge. A named migration destination without room for the guest is
refused before the guest is stopped.

To drive the deployment by hand, use `sproutfsctl`, which is in the image:

```sh
scripts/demo-gce.sh kubectl exec -n sproutfs deploy/sproutfs-orchestrator -- sproutfsctl hosts
scripts/demo-gce.sh kubectl exec -i -n sproutfs deploy/sproutfs-orchestrator -- \
    sproutfsctl console <vm> --for 8s
```

A console session ends when its input ends, so a scripted read needs `--for`.
A piped line is consumed long before the guest answers it.

You can also forward the orchestrator out of the cluster and run a local build
against it:

```sh
scripts/demo-gce.sh ssh 'sudo KUBECONFIG=/etc/rancher/k3s/k3s.yaml \
    kubectl port-forward -n sproutfs service/sproutfs-orchestrator 8080:8080'
SPROUTFS_ORCHESTRATOR=http://localhost:8080 \
    SPROUTFS_API_TOKEN=$(scripts/demo-gce.sh kubectl get secret -n sproutfs \
        sproutfs-api-token -o jsonpath='{.data.token}' | base64 -d) \
    go run ./cmd/sproutfsctl hosts
```

Both APIs require the deployment's token, so a client outside the cluster must
send it. `sproutfsctl` run through `kubectl exec` in the orchestrator pod
inherits the token from that container's environment and needs no setup.

To run a command in a guest instead of reading its console:

```sh
scripts/demo-gce.sh kubectl exec -n sproutfs deploy/sproutfs-orchestrator -- \
    sproutfsctl exec <vm> -- 'uname -a; cat /proc/meminfo | head -3'
```

`exec` stops parsing at the `--`, so the guest receives its quoting intact. The
command goes to the host that the orchestrator's table lists for the VM. From
there it enters the guest over the VM's vsock. In the guest, a small agent that
the guest image starts from init runs the command. The agent applies these
limits:

- Each command runs in its own process group. Its deadline therefore ends
  everything it started, not only the shell, and nothing it put in the
  background outlives the exec.
- Output is kept up to 1 MiB per stream. The rest is dropped instead of being
  held in the guest's RAM.
- The agent runs at most four commands at once and refuses the rest with 429.
  This stops a caller that retries from filling the guest.

`sproutfsctl list` shows the table. It lists which host each VM is on, and
whether the VM is being created, running, migrating between a named pair,
stopped, or being recovered. A deleted VM no longer appears in it. A VM exists
exactly as long as its control record exists. A delete removes that record even
if the VM's checkpoints are still pinned.

A console read starts at the requested offset and returns at most 256 KiB. It
reads the VMM's output as well as the guest's serial console. The host keeps
the newest 1 MiB of that output in memory. Read a guest that has run for a long
time from an offset, not from zero. A read from before the retained data starts
at the oldest byte the host has, and reports that it did.
`deploy/README.md` documents the full host and orchestrator API.

## A guest larger than 3 GiB

x86_64 reserves the range from 3 GiB to 4 GiB for MMIO. A VM with more than
3 GiB of RAM therefore has two guest memory regions over the one RAM volume, as
[vm-memory](vm-memory.md) describes. Only an x86_64 node exercises this case.
The Lima instance is aarch64. `scripts/demo-gce.sh bigguest` runs the case on a
demo cluster that is already up:

```sh
scripts/demo-gce.sh bigguest
```

It does the following:

1. Raises the deployment's VM RAM to 4 GiB and boots a guest.
2. Checks the guest's e820 map for a second usable range at 4 GiB.
3. Writes 3200 MiB of a non-zero pattern, which is more than fits below the
   gap.
4. Reads the tail of the data back in the parent, in a fork of it, and again
   after a migration. Each read is compared with an md5 that the node computes
   over the same pipeline.

On exit it restores the VM RAM setting it found, whether the run passed or
failed.

**This has not been run since the rename and the store rewrite.** The
2026-09-14 end-to-end validation ran out of time before it.
`scripts/lib/demo-bigguest.sh` is written and reviewed but has not been run.
Until someone runs it, only the unit tests cover the x86 gap mapping.

To do it by hand:

```sh
ctl() { scripts/demo-gce.sh kubectl exec -n sproutfs \
    deploy/sproutfs-orchestrator -- sproutfsctl "$@"; }
vm=$(ctl create --template alpine | cut -d' ' -f1)

# 4 GiB of guest RAM, which is more than the 3 GiB below the gap. It still fits
# the host's 5 GiB arena, so the pager stays resident and nothing spills. It
# comes from a cold boot, which is the one moment a VM's shape can change: the
# deployment's own SPROUTFS_VM_MEMORY_BYTES is the size a guest image is
# imported into a template at, and a template is named by the image's bytes, so
# changing it gives no new memory to a VM created from an image already there.
ctl stop "$vm"
ctl start "$vm" --cold --memory 4294967296

# Two usable e820 ranges, the second starting at 4 GiB.
ctl exec "$vm" -- 'head -1 /proc/meminfo; dmesg | grep -i usable'

# 3200 MiB of a non-zero pattern, which cannot fit below the gap, read back.
# The same pipeline on the node prints the md5 to compare every read against.
ctl exec "$vm" --timeout 300s -- 'mkdir -p /mnt/big
    mount -t tmpfs -o size=3400M tmpfs /mnt/big
    dd if=/dev/zero bs=1M count=3200 2>/dev/null | tr "\0" "Z" > /mnt/big/f
    md5sum /mnt/big/f'
scripts/demo-gce.sh ssh 'dd if=/dev/zero bs=1M count=3200 2>/dev/null |
    tr "\0" "Z" | md5sum'

ctl capture "$vm"
fork=$(ctl fork "$vm" --count 1 | awk 'NR==2 {print $1}')
ctl exec "$fork" --timeout 300s -- \
    'dd if=/mnt/big/f bs=1M skip=2944 count=256 2>/dev/null | md5sum'
ctl migrate "$vm" --to <the other host>
ctl exec "$vm" --timeout 300s -- \
    'dd if=/mnt/big/f bs=1M skip=2944 count=256 2>/dev/null | md5sum'
```

The fork and the moved VM read only the tail of the file. The tail is the part
that can only be above the gap. They do not read the whole file, because the
guest's agent kills any single command after ten minutes. The orchestrator also
waits ten minutes on a host. A large file is therefore read back in windows.

## Changing something

To put new code on a demo cluster that already exists:

```sh
scripts/demo-gce.sh redeploy
```

This ships the current source, rebuilds the image and restarts the pods. The
Dockerfile copies the Rust sources first. A change to the Go commands or to
`deploy/` therefore reuses the cached VMM and kernel stages. The workload guest
image already on the node is kept, not rebuilt. A redeploy takes a few minutes,
compared with about half an hour for the first build.

## Clean up

```sh
scripts/demo-gce.sh delete
```

This deletes the VM, its boot disk, and the bucket with its objects. It checks
that all three are gone and fails if any of them remains. The demo creates
nothing else, so nothing else is left.
