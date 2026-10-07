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
running. No simulation test yet kills a host right after a flush and recovers
its VM through the orchestrator, and no GCE run has measured it.
