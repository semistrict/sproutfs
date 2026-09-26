---
id: TASK-38
title: Fork from an older checkpoint that was marked kept
status: Done
assignee:
  - '@claude'
created_date: '2026-09-26 01:40'
updated_date: '2026-09-26 13:59'
labels:
  - embedder
dependencies: []
documentation:
  - docs/hosting.md
  - docs/metadata.md
priority: high
ordinal: 44000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
A fork can start only from a VM's newest checkpoint or from one a pin already keeps. The next checkpoint's reclamation deletes every older one, so a user cannot go back to an earlier point in a VM's life and start a new VM there. The owner decided on 2026-09-25 that older checkpoints stay forkable only when someone asks for it: a checkpoint request can mark its checkpoint kept, and only kept checkpoints cost storage beyond the newest. The owner also decided that a fork from a checkpoint that holds memory and VMM state resumes the guest where it was, as a fork of a running VM does, and that a fork from a checkpoint without state boots cold over its disks. Today a create from a published checkpoint (TASK-8, CreateRequest.From) always boots cold. A pin is permanent until a collector exists (TASK-24), so a kept checkpoint that has been forked cannot be released safely without one.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 A checkpoint request (suspend, capture and the host's checkpoint call, through the host API and the CLI) can mark the checkpoint it publishes kept; a kept checkpoint survives every later reclamation
- [x] #2 A VM's kept checkpoints can be listed, with each one's sequence, time and whether it holds VMM state
- [x] #3 A create from a kept checkpoint of any VM of the same tenant succeeds whether or not that VM runs, and whatever checkpoints it has published since
- [ ] #4 A fork from a kept checkpoint that holds VMM state resumes the guest from that state with its memory; one without state boots cold over its disks, and the result says which
- [x] #5 A kept checkpoint that no fork was taken from can be released, and reclamation then deletes what only it held; releasing one that was forked is refused
- [x] #6 Simulation invariants: a kept checkpoint's pages are never reclaimed while it is kept; a child started from it reads exactly that checkpoint's bytes, never a later one's
- [x] #7 docs/hosting.md and docs/metadata.md describe kept checkpoints
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. control: record format 5 adds kept checkpoints (sequence, time, whether it holds VMM state) beside the pins. Pins keep meaning forked. Handle.SelectKept selects and keeps in one write. Client.Pin also admits a kept checkpoint. Client.Release drops a kept checkpoint unless a pin holds it (ErrForked). Record.Protected() = pins and kept. control.Config.Clock dates keeps.
2. checkpoint/volume: reclamation and compaction spare Protected(); a VM delete still spares only pins. volume.Terms{Keep, Retry} on Snapshot/SnapshotDisks; the publication selects kept. Manager.Release releases and sweeps what only the released checkpoint held (Reclaim with it as the replaced root). ForkPoint.HasState.
3. host: Capture/CaptureDisks take Terms; Host.Stop takes a StopRequest with Keep; Host.CreateRoot publishes a create's root and resumes (state, no shape) or boots cold; Host.Kept lists. Supervisor create uses CreateRoot; CreateResult.Resumed.
4. API/CLI: CaptureRequest.Keep, StopRequest.Keep, GET /vms/{id}/kept, POST /vms/{id}/kept/{checkpoint}/release on host and orchestrator; sproutfsctl capture/stop --keep, kept VM, release VM@CHECKPOINT.
5. Simulation: driver keeps checkpoints, stops with keep, creates topology VMs from kept checkpoints (warm or cold), releases them; VerifyKept after every step; a child reads exactly that checkpoint's bytes; CheckDeployment reaches kept checkpoints. Guard volume-reclaim-kept.
6. Docs: metadata.md, hosting.md, volumes.md, context.md, architecture.md, testing.md, deploy/README.md.
7. Lima: vmmachine TestAVMCreatedFromAKeptCheckpointResumesItsGuest.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Progress: control record format 5 (Kept entries), SelectKept, Client.Release, Pin admits kept (commit 1). volume.Terms{Keep,Retry}, reclamation spares Protected(), Manager.Release sweep, Host.CreateRoot warm/cold, host API keep/kept/release, deployment check reaches kept, fixtures bumped (commit 2). Environment: the Xcode license is not accepted on this Mac, so the /usr/bin version-control shim fails; used the Xcode binary directly and GOFLAGS=-buildvcs=false.

Progress: orchestrator and sproutfsctl (capture/stop --keep, kept, release VM@CHECKPOINT); simulation ops keep / create-from-kept / release with VerifyKept after every step; guard volume-reclaim-kept; docs. 100-seed TestSeededTopologySoak passed with 16 creates from kept checkpoints (9 resumed, 6 cold), 84 releases, 10 refused as forked. Lima: vmmachine TestAVMCreatedFromAKeptCheckpointResumesItsGuest written; the run failed before the guest booted because the Lima instance has no HugeTLB pool (HugePages_Total 0, pager fault: no space left on device).

Determinism: the fingerprint test caught a background sweep racing the next step's store fault (the kept operations changed seed 1's schedule so it hit it). World.checkpoint and checkpointDisks now wait for Swept. The host's control client is also given the host clock, so kept times are simulated time. After that: go test ./internal/simtest passes; 100 seeds each of TestSeededTopologySoak and TestBuggifiedTopologySoak pass with 32 creates from kept checkpoints (18 resumed, 12 cold), 166 releases and 20 releases refused as forked. AC4 is proven in host tests and the simulation; the real-VMM Lima run is blocked until the instance has a 2 MiB HugeTLB pool.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
A checkpoint request can keep its checkpoint: capture and stop (plain or suspending) take keep on the host API, the orchestrator and sproutfsctl (--keep), and the host's Capture/CaptureDisks take volume.Terms{Keep}. Control record format 5 lists kept checkpoints (sequence, time, state) beside the pins, which still mean forked; the selecting write keeps. Reclamation and compaction spare pinned and kept checkpoints; a VM delete takes unforked kept ones. A pin without the writer may name a kept checkpoint, so a create can start from it whether or not the VM runs. Host.CreateRoot resumes from VMM state with its memory, or boots cold when there is no state or a shape is named; CreateResult.Resumed says which. Client.Release / Manager.Release give up an unforked kept checkpoint and sweep what only it held; a forked one is refused (ErrForked, 409). GET /vms/{id}/kept and POST /vms/{id}/kept/{checkpoint}/release on host and orchestrator; sproutfsctl kept and release VM@CHECKPOINT. Verified by control, volume, host, orchestrator, host-server and CLI tests; the simulation (keep, create from kept, release; VerifyKept every step; guard volume-reclaim-kept killed); 100 seeds each of the plain and buggified topology soaks; Go, proto and shell parts of just check. Open: AC4 on a real VMM. vmmachine TestAVMCreatedFromAKeptCheckpointResumesItsGuest did not boot because the Lima instance has no 2 MiB HugeTLB pool. The check-rust step cannot link on this Mac until the Xcode license is accepted.
<!-- SECTION:FINAL_SUMMARY:END -->
