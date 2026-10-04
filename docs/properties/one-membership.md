---
title: One membership
summary: Which hosts are in the cluster, with their weights, is one object in the object store, changed only by compare-and-set, and every request that routes by it names the generation it used.
---

**Given** a cluster whose hosts route requests by its membership, such as the
disk cache placing a window's stripes,
**when** a host joins, drains or leaves, or a weight or the code changes,
**then** the change is a compare-and-set of one object in the object store
that raises its generation. Correctness rests on that primitive alone: any
process may make the change, by reading the object, changing it and writing it
back conditional on the generation it read. Usually the orchestrator does, but
nothing depends on there being one writer.

Every request between hosts that depends on the membership names the
generation its sender holds. A host that finds the other side's generation
newer than its own reads the object before it acts. So no two hosts act on
different memberships without finding out.

A host keeps a copy of the membership, never an authority of its own.

**Status, 2026-10-03.** Holds in the simulation, and in its model. The
membership is the object `membership` (package `membership`); the list of
caches, `GET /caches` and the host's follower of it are gone (TASK-83).

- `Store.Update` writes only by a write conditional on the object read, and
  `membership.Step` refuses any next generation that skips one or moves a
  disk without releasing it. `TestConcurrentWritersNeverLoseAnUpdateOrGoBack`
  runs four writers with lost replies, failed writes and outages over sixteen
  seeds, and requires one line of generations, every acknowledged write in
  it, and no lost change.
- Every stripe read, keep, drop, presence check and fill right names the
  sender's generation. The peer server catches up when behind and answers
  stale otherwise (`TestAHolderBehindReadsTheMembershipBeforeItAnswers`,
  `TestAHolderOnAnotherGenerationAnswersStaleWithItsOwn`), and a sender told
  it is stale reads the object and asks again
  (`TestAReaderBehindItsHoldersReadsTheMembershipAndAsksAgain`,
  `TestAFillToHoldersAheadIsSentAgainUnderTheirGeneration`). A host that lost
  a disk never serves it again (`TestAMemberThatLostADiskNeverServesItAgain`).
- `spec/membership` checks that no stripe is served or placed under a
  membership the two sides do not both hold, that generations form one line,
  and that no two live hosts serve one disk; its mutants without the
  generation check, the assignment check, the conditional write and the
  release each fail.
- The orchestrator is the usual writer, one step a pass; two at once leave
  one line of generations (`TestTwoOrchestratorsMoveOneMembership`).

Not yet shown on a real cluster. Hosts do not write the membership
themselves: they report what they hold, and the controller writes it.

**Shards, 2026-10-04.** The membership moves shards, network disks, between
members, and the controller carries each step out through the cloud's attach
API (TASK-86, [hosting](../hosting.md#shards-on-network-disks)). A shard is let
go only once its host has closed it and the cloud has it on no machine, and a
host opens one only once the object, read again, still assigns it there; the
shard's lease refuses a member of an older assignment. `spec/shards` checks
`OneServer` with stale controllers and hosts.
