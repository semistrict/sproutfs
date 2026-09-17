# Sproutfs

Sproutfs runs virtual machines whose disk and memory follow them between hosts
and can be forked without copying. Start with the architecture.

- [Architecture](architecture.md): the decisions, the components, the loss model and the VM lifecycle.
- [Terminology](context.md): the shared domain language.
- [Volumes and checkpoints](volumes.md): a VM's volumes, its checkpoints, the index objects and parts they publish, and forks.
- [Metadata authority](metadata.md): the control record, conditional writes and the latency boundary.
- [Hosting](hosting.md): host assembly, budgets and shutdown.
- [Managed VM memory](vm-memory.md): the pager, the mapping protocol, Firecracker and qualification.
- [Live migration](migration.md): the handoff, the pause, the page server and the drain.
- [Testing](testing.md): deterministic simulation, byte models and injected failures.
- [The demo](demo.md): the five flows on one disposable GCE VM, in three commands.
- [What a checkpoint costs under a workload](measurements-2026-09-14-workload.md): object-store traffic per checkpoint under a guest that searches, installs and builds.
- [Measurement reports](measurements/README.md): the dated reports behind the numbers quoted elsewhere.
- [Open work](open-work.md): the open engineering items.
