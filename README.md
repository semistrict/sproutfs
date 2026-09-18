# Sproutfs

Sproutfs is storage and managed memory for virtual machines that can move
between hosts and fork without copying their inherited disk and memory data.
It combines whole-VM checkpoints in object storage with a shared memory pager
and a Firecracker integration.

The project is under active development. It provides the host and orchestrator
commands the demo deployment runs, and the Rust library the VMM maps its guest
memory through; every Go package is internal to the module and none is offered
as a library. No production deployment is recorded yet.

There is no garbage collector, by decision, and its absence is not a gap to
close before the next release: it is deferred indefinitely. Until one exists the
object store grows without bound — every checkpoint a VM was ever forked at, the
checkpoints its index names, and the pinned lineage a deleted VM leaves behind are
kept forever. Deleting a VM reclaims only what nothing forked from. See
[open work](docs/open-work.md#correctness-and-unbounded-growth).

## What it does

- Serves VM volume reads and writes from memory, and makes them durable with a
  periodic whole-VM checkpoint: pause the vCPUs, save VMM state, seal the dirty
  pages by write protection, resume, then upload the sealed pages and an index.
- Forks a running VM as a handoff from the parent: one pause yields one fork
  point of it for any number of children, here or on another host, and nothing is
  published to take it.
- Shares resident memory pages of matching lineage within a pager.
- Moves a running VM between hosts post-copy: the destination resumes first and
  pulls the pages no checkpoint holds from the source's page server.

A VM's durable state is exactly one checkpoint, selected by the VM's control
record in the object store. A guest write never waits on object-store latency,
and losing a host rewinds its VMs to their last checkpoint — at most one
checkpoint interval, 60 seconds by default. See
the [loss model](docs/architecture.md#loss-model) before integrating.

## See it run

One disposable GCE VM runs a single-node cluster with two host pods, and the
five flows — boot, fork, live migration, recovery after a host is killed, and
running a command inside a guest across a fork and a move — run against it
non-interactively:

```sh
scripts/demo-gce.sh create
scripts/demo-gce.sh run
scripts/demo-gce.sh delete
```

See [the demo guide](docs/demo.md) for what each command does and what the
timings look like.

## Build and test

Install Go 1.26 or newer, then run from the repository root:

```sh
go build ./...
go test ./...
go test -race ./...
```

The core Go tests run with simulated disks, networks and object storage; they
do not require a cloud account. Linux VM integration tests have additional
requirements, including KVM and userfaultfd. Follow the
[managed-memory qualification guide](docs/vm-memory.md#qualification) for those
tests and the Firecracker build.

If you change protocol definitions, install Buf and run `just generate`
(or `buf lint` followed by `buf generate`). Generated files should not be
edited directly.

## Documentation

- [Architecture](docs/architecture.md): components, the loss model and the VM lifecycle.
- [Volumes and checkpoints](docs/volumes.md): the dirty overlay, publication and forks.
- [Host assembly](docs/hosting.md): assembly, draining, budgets and shutdown.
- [Managed VM memory](docs/vm-memory.md): pager and Firecracker integration.
- [Live migration](docs/migration.md): post-copy migration and fork handoff.
- [Testing](docs/testing.md): simulation, failure injection and integration tests.
- [The demo](docs/demo.md): boot, fork, migration and host loss on one GCE VM.

See the [documentation index](docs/index.md) for the full guide.

## License

Apache-2.0; see [LICENSE](LICENSE). The Firecracker fork under
`third_party/firecracker` keeps its own Apache-2.0 license.
