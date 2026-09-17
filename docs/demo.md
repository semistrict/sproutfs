# The demo on one GCE VM

The demo runs the four flows sproutfs exists for — boot, fork, migration and
recovery after a host is lost — and a fifth that uses the VMs rather than
watching them, on one disposable GCE VM. A single-node k3s
cluster on that VM runs two host pods and one orchestrator; the guests are
Firecracker microVMs on the node's own `/dev/kvm`, and the bucket holds every
control record and checkpoint.

Three commands: `create`, `run`, `delete`. Five more when you want them:
`fixes`, `bigguest`, `workload`, `soak` and `redeploy`.

## Prerequisites

- `gcloud`, authenticated, with a default project that can create a
  `n2-standard-8` instance with nested virtualization and one GCS bucket.
  `scripts/demo-gce.sh` calls the instance `sproutfs-demo-1` and the bucket
  `sproutfs-demo-<project>`, labels both `purpose=demo,lifecycle=temporary`,
  and touches nothing without those labels.
- The Firecracker submodule, which the container image is built from:

  ```sh
  git submodule update --init third_party/firecracker
  ```

- `python3`, which is what packs the versioned source for the node. Nothing
  else locally: the container image, the guest images and the five flows are
  all built and run on the demo VM. `kubectl` is not needed on your machine.

The VM powers itself off after 24 hours with no command run against it, and GCE
deletes it after 24 hours of run time whatever it is doing, so a forgotten demo
costs a day at most. Every command the script runs on the VM renews that lease.

## Create

```sh
scripts/demo-gce.sh create
```

This creates the bucket and the VM, waits for the VM's startup script to set up
the 12 GiB HugeTLB pool and k3s, writes to the bucket with the VM's own
credentials, proves on a freshly created VM that `/dev/kvm` and the huge pages
are reachable from inside a pod, builds the container image and the two guest
images on the node, imports the container image into the node's containerd,
applies `deploy/`, and waits for the pods.

Twenty to thirty minutes the first time, most of it the Firecracker build; the
2026-09-14 run took twelve, of which the Rust build was about four. What you
should see at the end:

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

The five flows, non-interactively, with their timings. It exits non-zero on the
first thing that does not hold, and prints what the guest's console said when it
does. `scripts/lib/demo-run.sh` is the script it runs on the node; it drives the
deployment through `sproutfsctl`, which is in the deployment's own image, so
nothing is forwarded out of the cluster.

What it does, and what you should see:

1. **Create.** `sproutfsctl create --template alpine` boots a VM. The run waits
   for the guest's banner on the serial console and then runs `uname -a` in it,
   which is the guest answering for itself. It then sets a shell variable in the
   guest: only that guest's own memory carries it, so a VM that still answers
   with it later is the same guest and not a fresh boot. A create is a fork of
   the template's root checkpoint, and it does not return until the new VM has
   published a root checkpoint of its own: until it has, the VM runs on that
   host and nowhere else, so nothing could recover it and nothing could fork
   it. It is published between the fork and the boot, where it is free — the
   VM's bytes are still the template's, so it seals no pages and uploads none —
   and it is the `root` time the create prints.
2. **Fork.** `sproutfsctl fork <vm> --count 5` forks the running VM five times
   on its own host. All five come from one instant of the parent, so it pauses
   once: a fork is a migration handoff from a VM that keeps running, and nothing
   is published to take it. Every child is asked a question on its own console
   and has to answer it, and the host's shared frame count has to have risen:
   the children inherited the parent's memory rather than copying it.

   The run then does it again with `--to <the other host>`. The child starts
   there and pulls the pages no checkpoint of the parent holds out of the
   parent's page server, exactly as a migration's destination does; the parent
   keeps running where it was. The `SERVED` column of `sproutfsctl hosts`, which
   is the parent host's page server counting what it handed over, has to have
   risen. The forks are closed afterwards, so the flows below run on a host with
   room.
3. **Migrate.** `sproutfsctl migrate <vm> --to <the other host>` moves it. The
   guest has to still hold the variable that was set in it before the move,
   which is what makes this a migration and not a reboot. The pause and the
   post-copy stream time are printed.
4. **Lose a host.** `sproutfsctl kill-host <the host running it>` deletes that
   pod with no grace period, which skips the drain: this is a host loss, not a
   shutdown. `sproutfsctl recover <vm>` reopens the VM on the surviving host
   from its last checkpoint, and the guest has to still hold the variable. The
   run takes an explicit checkpoint (`sproutfsctl capture`) before the kill;
   a real loss rewinds the guest by at most the checkpoint interval, 60 s.
   Recovery reopens the VM only on evidence that its host is gone: the deleted
   pod stops being listed, and every pod that is still listed answered and said
   it does not run the VM. A host that merely did not answer is not evidence —
   its guest may be running perfectly well — so a recovery is refused while one
   is quiet, and `sproutfsctl recover <vm> --force` is how an operator who knows
   otherwise says so. No host running a VM is not the same as no host holding
   one: a source that has handed a VM over still serves the pages no checkpoint
   of it has, and an operation the orchestrator started is in flight for as long
   as its table row says so. Both are VMs between two hosts rather than lost, and
   a recovery is refused while either holds — force included, because force is
   evidence about a host's process and says nothing about a handover. An
   in-flight row whose operation really did die ages off, so it stops refusing on
   its own.
5. **Use it.** `sproutfsctl exec <vm> -- <command>` runs a command inside the
   guest and prints what it printed. The run does it three times: in the VM, in
   a fork of it — which has to hold a file the parent wrote before the fork —
   and again after moving the VM to the other host, through the same name and
   with a file written before the move still there. Nothing is addressed: the
   guest has no network. The command crosses the VM's virtio-vsock device,
   which is part of the VMM state a restore replays, so a fork and a migration
   carry the channel with them.

A run on an `n2-standard-8` looks like this — this one is 2026-09-14 on
`n2-standard-8` in `us-east4-a`, all five flows green:

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

The create behind that is `template 1.25s, fork 0.07s, boot 0.07s, root 0.15s`:
the template checkpoint already existed on that host, so the 1.25 s is reading
it, and the root is the new VM's own first checkpoint, which costs almost
nothing because nothing has run yet.

Five forks cost the parent one pause, because they are one instant of it and
none of them publishes anything: the pause is the VMM state capture and the
seal, and the total is that plus the children's boots. It is the whole fan-out's
pause, so it is the one number here that grows with the number of children —
0.464 s for five against 0.080 s for the single cross-host one. A fork placed on
the other host costs that pause and the post-copy of one interval's dirty set
behind it; this one moved 62 pages, 62 of them unpublished.

The migration moved the same 62 pages and the guest kept the shell variable set
in it beforehand; the recovery reopened it from checkpoint `8589934593` on the
surviving host, and the guest still held that variable. The cross-host fork
raised the parent host's `SERVED` count from 0 to 62, and the five same-host
forks raised its `SHARED` frame count from 0 to 310, which is what says the
children inherited the parent's memory rather than copying it.

## Workload

```sh
scripts/demo-gce.sh workload
```

A sixth thing to run, and the only one that is a measurement rather than a
demonstration: what the checkpoint model costs in object-store traffic under a
guest that is used. It boots a VM from the `workload` template — the same Alpine
plus git, ripgrep, Node, pnpm and three MIT-licensed TypeScript repositories
whose dependencies are in a pnpm store inside the image — checkpoints it, forks
it, and drives the forks through searching with `rg`, reading history with
`git`, an offline `pnpm install` and a build, ending each phase with an explicit
checkpoint of every VM and finishing with a phase in which nothing happens at
all, so the cost of an idle guest's interval checkpoint is in the numbers too.

`FORKS_BASE` is how many forks come off the base image's checkpoint and
`FORKS_PER_REPO` how many come off each of those once it has installed its
dependencies; they default to 2 and 1, which fit a host's 5 GiB arena. Raising
them is how a run asks what more sharing costs:

```sh
FORKS_BASE=3 FORKS_PER_REPO=2 scripts/demo-gce.sh workload
```

Every fork lands on its parent's host, so left alone a run piles onto the host
its base was created on while the other sits idle; the workers are spread over
the ready hosts with a migration after the one fork instant that starts them.
Even so this node tops out at three guests installing at once — the two host
pods' spill files are on the one boot disk — and four is where it stops
answering. [The larger setting](measurements-2026-09-14-workload.md#the-larger-setting)
has the detail.

Restart the hosts before a run whose numbers you mean to keep: their
object-store counters and frame counts are since the process started, and the
run reads their logs, so a host that has already carried a run reports that
one's work beside this one's.

```sh
scripts/demo-gce.sh kubectl rollout restart -n sproutfs deployment/sproutfs-host
```

A rollout takes one host pod down at a time — the two together request the
node's whole HugeTLB pool, so there is no room for a third — and the pod going
away drains its VMs to the one that stays up. Nothing is lost and nothing
rewinds: a guest keeps what it wrote since its last checkpoint, because it was
moved rather than restarted.

Every checkpoint of every VM is one line in the host's log — the VM, the
sequence, the 2 MiB pages the pause sealed, the bytes uploaded, the objects
written, and the pause and upload durations — and `sproutfsctl store` reports
each host's object-store counters, so two readings subtracted are what a phase
cost. The run joins the two and prints the tables; it copies everything it
recorded back to `.workload-runs/` here.
[What a checkpoint costs under a workload](measurements-2026-09-14-workload.md)
is one run written up.

## The soak

```sh
scripts/demo-gce.sh soak
```

The flows above prove each operation once. This takes many forks across the same
host and the other one, migrates and stops and starts them with work happening in
between, loses a host under them, and after every one of those asks each guest
whether its memory and its disk still hold exactly the bytes that guest wrote.
The simulated campaigns do that with model guests; this is the same discipline
over Firecracker, KVM, the pager, GCS and k3s.

What asks the guest is `sproutfs-guest-witness`, which both guest images carry
beside the agent. It holds a resident buffer and a file of the same size — 256
MiB each by default — filled with the pattern of a `(seed, step)`, mutates a
scattered fraction of both at each step, and checks every byte of memory and
every byte of the file, read again off the disk rather than compared against the
buffer. The pattern is a pure function of the seed, the step and the page, so
the expectation lives in the script and never in the guest: nothing about what a
VM should hold is carried across a fork, a migration or a stop, and a guest
cannot agree with itself about the wrong thing.

Each round, over every running VM in an order the seed draws: mutate and check;
fan one parent out on its own host and on the other one, checking every child
against its parent's `(seed, step)` before anything else and then giving it a
seed of its own; migrate the round's share, checking before and after; stop the
round's share, check that no host runs them, start them on the other host and
check them; and delete a seeded share so the population stays within what two
hosts admit. A placement the deployment refuses with 503 is recorded rather than
failed.

A seeded share of the restarts are cold: the VM comes back without its memory,
so what is asked of it is its disk alone — `witness check --disk-only`, which
reads the file with no witness resident, because the process that held the
buffer went with the memory — and the witness is then filled again under a seed
of its own before anything mutates that VM. A share of those cold starts also
resize it: a larger memory, a larger root volume, `witness grow /` in the guest
to take the pages the volume gained, and the disk checked again after it, because
a filesystem grown over the witness file is a filesystem that must still hold
it.

Once per soak, at a round the seed picks, every VM is captured and one host pod
is killed without grace, its VMs are recovered on the other host and each is
checked against what its last checkpoint published. That happens at the top of
the round, before anything in it has mutated a guest: every host checkpoints on
its own interval, so a kill taken after a mutation comes back at a state the
script cannot name, and a guest checked against a state it was never in is a
failure that says nothing.

At the end every VM is deleted and `sproutfsctl check` runs over the bucket,
when nothing but the templates and the lineages the deleted VMs pinned may
remain. The run prints what each round did and what it cost — the fork pauses,
the migration pauses and streams, the stops and the starts — with every check
and each host's object-store counters, and copies everything it recorded back to
`.workload-runs/soak-<timestamp>` here. It exits non-zero on the first check
that fails.

`SOAK_SEED` reproduces a run's shape: the order the VMs are walked in, which of
them each round's shares fall on, which round loses a host, and every witness
seed. The shares are `SOAK_ROUNDS`, `SOAK_FORKS_LOCAL`, `SOAK_FORKS_REMOTE`,
`SOAK_MIGRATIONS`, `SOAK_STOPS` and `SOAK_MAX_VMS`, and `SOAK_WITNESS_BYTES` is
how much of each guest is under the witness. `SOAK_COLD_STARTS` and
`SOAK_COLD_RESIZES` are one-in-this-many — zero draws none and one draws every
time — and `SOAK_COLD_MEMORY` and `SOAK_COLD_DISK` are what a resized VM comes
back as:

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

A stop is the deliberate end of a running VM that leaves the VM behind: the host
running it publishes everything its guest holds, closes the VMM process, gives
the frames back and releases the handle. The VM is then exactly its control
record and its objects — still listed, on no host — and a start opens it again at
exactly the bytes the stop published, on the host named or on the ready one whose
guests have promised the least of its arena.

That is the whole difference between a stop and losing the host, which takes the
writes since the last checkpoint with it. It is also why a start needs none of
the evidence a `recover` does: `recover` has to prove the VM's host is gone,
because opening takes the control record's epoch and would fence a guest that is
running perfectly well, and a stopped VM has no such host. Everything that says
the VM is between hosts rather than stopped still refuses a start — a host that
runs it, two hosts that claim it, a host still serving the pages no checkpoint
has, and an operation the orchestrator started that is still in flight.

A stop is refused for a VM a fork instant holds sealed, exactly as a delete is:
a child elsewhere is reading the pages no checkpoint holds out of the frames the
stop would release. Take the children's root checkpoints first.

## Starting a VM cold, and resizing it

```sh
scripts/demo-gce.sh kubectl exec -n sproutfs deploy/sproutfs-orchestrator -- \
    sproutfsctl start vm-01k... --cold
scripts/demo-gce.sh kubectl exec -n sproutfs deploy/sproutfs-orchestrator -- \
    sproutfsctl start vm-01k... --cold --memory 1G --disk 8G
```

A cold start brings a stopped VM back without its memory, for the reasons one
reboots any machine: a guest that is wedged, a kernel or init change on the
disk, or simply not to pay for memory the guest does not need to keep. The host
discards every page of the VM's RAM and the VMM state with it in one checkpoint
and boots the kernel from the root volume, which is exactly what the last
checkpoint published — so the guest's filesystem sees the boot as a power cut
after that checkpoint, and its journal recovers what a journal recovers. The
pages the memory held are reclaimed by the usual set difference.

It applies to a stopped VM. Stop a running one first, as you would to start it
warm.

A cold boot is also the one moment a VM's shape can change, because nothing in
memory describes it any more. `--memory` sets the RAM to any size the host
admits, up or down; `--disk` grows the root volume, whose new pages read as
zeroes, and the guest takes them with the guest witness's own grow:

```sh
scripts/demo-gce.sh kubectl exec -n sproutfs deploy/sproutfs-orchestrator -- \
    sproutfsctl exec vm-01k... -- sproutfs-guest-witness grow /
```

It is the witness's job and not resize2fs's. resize2fs's online path opens the
block device read-write, and a 6.18 kernel refuses a write open of a device
something has mounted: on a guest whose root filesystem is on that device it
fails with EBUSY before it begins. The grow issues `EXT4_IOC_RESIZE_FS` on a
descriptor of the mount point instead, which opens no device at all.

Shrinking the disk is refused — the end of a filesystem is not the volume's to
cut — and both flags are refused without `--cold`. From a resize on, the VM's
committed RAM is its own rather than its template's, and every later placement
of it, including a fork's children, is admitted against that.

## The failure paths

```sh
scripts/demo-gce.sh fixes
```

`run` shows the five flows working. This shows four things about what happens
when they do not, which only a live cluster can: a cross-host fork whose
destination pod is deleted leaves the parent running and checkpointing rather
than sealed forever; a recovery is refused while the VM's host is alive and
answering, and taken once that pod is gone; and a VMM killed underneath its host
leaves a diagnostic naming the VM, the cause and the console tail, after which
the host forgets the VM. `scripts/lib/demo-fixes.sh` is the script it runs on
the node.

## Looking around

```sh
scripts/demo-gce.sh kubectl get pods -n sproutfs
scripts/demo-gce.sh kubectl logs -n sproutfs -l app.kubernetes.io/name=sproutfs-host
scripts/demo-gce.sh ssh                     # a shell on the node
scripts/demo-gce.sh status                  # what exists, cloud side and node side
```

Where a VM goes is the host whose guests have promised the least of its arena,
not the one running the fewest VMs: a guest's cost to a host is its RAM, so one
2 GiB workload guest takes more of a host than three 512 MiB ones. What is
counted is the RAM the running guests were promised, not how much of the arena
is resident — the arena is a cache, so a page of a VM that has been migrated
away or deleted stays in it until a frame is needed, and a host measured by
residency looks full the moment it has done any work, which is a drain with
nowhere to go. A create or a fork whose children do not fit anywhere is refused
with 503 rather than started on the emptiest host and lost when its memory will
not attach — on the parent's own host as much as on another, because children
share the pages they have not diverged from and not the promise that they may —
and a named migration destination without room for the guest is refused before
the guest is stopped.

To drive the deployment by hand, `sproutfsctl` is in the image:

```sh
scripts/demo-gce.sh kubectl exec -n sproutfs deploy/sproutfs-orchestrator -- sproutfsctl hosts
scripts/demo-gce.sh kubectl exec -i -n sproutfs deploy/sproutfs-orchestrator -- \
    sproutfsctl console <vm> --for 8s
```

A console session ends with its input, so a scripted read needs `--for`: a line
piped in is exhausted long before the guest has answered it.

Or forward the orchestrator out of the cluster and run a local build against it:

```sh
scripts/demo-gce.sh ssh 'sudo KUBECONFIG=/etc/rancher/k3s/k3s.yaml \
    kubectl port-forward -n sproutfs service/sproutfs-orchestrator 8080:8080'
SPROUTFS_ORCHESTRATOR=http://localhost:8080 \
    SPROUTFS_API_TOKEN=$(scripts/demo-gce.sh kubectl get secret -n sproutfs \
        sproutfs-api-token -o jsonpath='{.data.token}' | base64 -d) \
    go run ./cmd/sproutfsctl hosts
```

Both APIs require the deployment's token, so a client outside the cluster
carries it; `sproutfsctl` run through `kubectl exec` into the orchestrator pod
inherits it from that container's environment and needs nothing.

To run a command in a guest rather than read its console:

```sh
scripts/demo-gce.sh kubectl exec -n sproutfs deploy/sproutfs-orchestrator -- \
    sproutfsctl exec <vm> -- 'uname -a; cat /proc/meminfo | head -3'
```

`exec` stops parsing at the `--`, so the guest's own quoting reaches it intact.
It goes to the host the orchestrator's table says runs the VM, and from there
into the guest over the VM's vsock, where a small agent the guest image runs
from init executes it. Each command runs in a process group of its own, so its
deadline ends everything it started rather than the shell alone and nothing it
backgrounded outlives the exec; what it prints is kept up to 1 MiB per stream
and the rest is dropped rather than held in the guest's own RAM; and the agent
admits four commands at once, refusing the rest with 429 rather than letting a
caller that retries fill the guest. `sproutfsctl list` shows the table: which
host each VM is on, and whether it is being created, running, migrating between
a named pair, stopped or being recovered. A deleted VM is simply gone from it: a
VM exists exactly while its control record does, and a delete removes that
record whatever its checkpoints are still pinned for.

A console read starts at the offset it is asked for and returns at most 256 KiB,
and what it reads is the VMM's own output as well as the guest's serial console,
held as the newest 1 MiB in the host's memory. A guest that has been running for
a long time is read from an offset rather than from zero; a read from before what
is still retained starts at the oldest byte the host has and says so.
`deploy/README.md` documents the whole host and orchestrator API.

## A guest larger than 3 GiB

x86_64 reserves 3 GiB to 4 GiB for MMIO, so a VM with more than 3 GiB of RAM is
two guest memory regions over the one RAM volume, which
[vm-memory](vm-memory.md) describes. Only an x86_64 node exercises that; the
Lima instance is aarch64. `scripts/demo-gce.sh bigguest` is the run that does,
on a demo cluster that is already up:

```sh
scripts/demo-gce.sh bigguest
```

It raises the deployment's VM RAM to 4 GiB, boots a guest, checks the guest's
own e820 map for a second usable range at 4 GiB, writes 3200 MiB of a non-zero
pattern — more than fits below the gap — and reads the tail of it back in the
parent, in a fork of it and again after a migration, against an md5 the node
computes over the same pipeline. It puts the VM RAM back to what it found on the
way out, whether it passed or not.

**This has not been run since the rename and the store rewrite.** The 2026-09-14
end-to-end validation ran out of time before it, so `scripts/lib/demo-bigguest.sh`
is written and reviewed but unexercised; the x86 gap mapping is covered only by
the unit tests until someone runs it.

By hand, it is this:

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

The fork and the moved VM read the tail of the file — the part that can only
live above the gap — rather than the whole of it: the guest's agent kills any
one command at ten minutes, which is also how long the orchestrator waits on a
host, so a large file is read back in windows.

## Changing something

To put new code on a demo cluster that already exists:

```sh
scripts/demo-gce.sh redeploy
```

This ships the current source, rebuilds the image and restarts the pods. The
Dockerfile copies the Rust sources before anything else, so a change to the Go
commands or to `deploy/` reuses the cached VMM and kernel stages, and the
workload guest image already on the node is kept rather than rebuilt: a redeploy
is a few minutes, against the half hour of the first build.

## Clean up

```sh
scripts/demo-gce.sh delete
```

The VM, its boot disk and the bucket, with the objects in it. It verifies all
three are gone and fails if any is not. Nothing else was created, so nothing
else is left.
