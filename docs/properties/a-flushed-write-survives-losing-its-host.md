---
title: A flushed write survives losing its host
summary: With durable flush on, a write a guest's flush returned for is there wherever the VM runs next.
---

**Given** durable flush on, and a guest that stores to a disk and flushes it,
**when** the flush returns success and the host is lost at once,
**then** the VM opens on another host with every block it stored before the
flush, or a later value, and runs only after those blocks are in its disks.
A flush that cannot be made durable fails with an I/O error instead.

**Status, 2026-10-07.** Partly. The parts hold in package tests.
`TestAJournalKeepsEveryAnsweredEntryThroughItsFaults` in `journal` reads back
every answered entry through injected write, sync and power faults.
`TestAnotherHostsOpenReplaysWhatTheVMFlushed` in `host` opens a VM on a second
host, which reads the first host's journal, fences it, and finds the flushed
byte. `MCCapture` and `MCTakeover` in `spec/journal` check `NoLostFlush` under
seals, failed batches, migrations, recoveries and fenced hosts that keep
running. The simulation kills a host right after a flush, between the sync
and the answer, during a seal's upload, during a post-copy and during a
scale-down, and the recovered VM holds every flushed block
(`internal/simtest/journals_test.go`); `TestJournalsSurviveTheirFaultsAndReachTheirProbes`
runs the same check through every fault site. On GCE a VM whose node was
powered off after a flush ran again on another node, 86.5 s later, with the
flushed block intact (docs/measurements/gce-fsync-journal-2026-10-07.md). Two
faults the shards model found are still open: a disk marked empty can be
deleted with an entry written after the mark, and a controller can add back a
disk another deleted.
