# Sproutfs

Sproutfs is storage and managed memory for virtual machines. The VMs can move
between hosts and fork without copying their inherited disk and memory data.
Sproutfs combines whole-VM checkpoints in object storage with a shared memory
pager and a Firecracker integration.

The project is under active development. It provides the host and orchestrator
commands that the demo deployment runs. It also provides the Rust library
through which the VMM maps its guest memory. The runtime packages at the
module root (`host`, `volume`, `vmmemory`, `vmmigrate`, `vmmachine`, `api/host`
and what they build on) can be imported by a program that embeds a host.
Helpers and test harnesses stay under `internal`. No production deployment is
recorded yet.

There is no garbage collector. This is a deliberate decision, and the absence
is not a gap to close before the next release: the collector is deferred
indefinitely. Until one exists, the object store grows without bound. It keeps
these objects permanently:

- every checkpoint at which a VM was ever forked;
- the checkpoints that checkpoint's index names;
- the pinned checkpoints that a deleted VM leaves behind.

Deleting a VM reclaims only checkpoints that no fork was taken from. See
[open work](docs/open-work.md#correctness-and-unbounded-growth).

## What it does

- Serves VM volume reads and writes from memory. It makes them durable with a
  periodic whole-VM checkpoint: pause the vCPUs, save VMM state, seal the dirty
  pages by write protection, resume, then upload the sealed pages and an index.
- Forks a running VM as a handoff from the parent. One pause gives one fork
  point, which serves any number of children on this host or on another host.
  Taking the fork point publishes nothing.
- Shares resident memory pages of the same identity within a pager.
- Moves a running VM between hosts post-copy. The destination resumes first and
  pulls the pages that no checkpoint holds from the source's page server.

A VM's durable state is one checkpoint, which the VM's control record in the
object store selects. A guest write never waits on object-store latency.
Losing a host rewinds its VMs to their last checkpoint, which is at most one
checkpoint interval old (60 seconds by default). Read
the [loss model](docs/architecture.md#loss-model) before integrating.

## See it run

One disposable GCE VM runs a single-node cluster with two host pods. Five flows
run against it non-interactively:

- boot;
- fork;
- live migration;
- recovery after a host is killed;
- running a command inside a guest across a fork and a move.

Run them with:

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
