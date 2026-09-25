# Disk checkpoints, and RAM only on request

Decided 2026-09-24. Most software does not assume RAM survives the loss of the
node it runs on, and distributed storage is expected to keep the disk. The
target is an agent sandbox: what matters is that its disk is preserved, and the
agent recovers its in-memory state from it.

## What changes

1. **The interval checkpoints disks only.** A checkpoint of the interval pauses
   the vCPUs, seals the memory regions of the VM's disks (PMEM) and nothing else,
   resumes, and publishes those pages with no VMM state. RAM is not sealed and
   not uploaded. The published root keeps RAM's pages as the last checkpoint
   that had them left them, so nothing reclaims objects a resident page still
   names.
2. **RAM is uploaded only on request.** An explicit capture (the host API's
   checkpoint, and a stop that asks to suspend) seals every memory region and captures
   the VMM state, as every capture does today. A plain stop publishes disks
   only.
3. **A checkpoint without VMM state is a cold boot.** Opening one discards RAM
   and boots the kernel over the disks, which is a power cut at that checkpoint:
   the guest's filesystem recovers what its journal recovers. After a host
   loss a VM therefore cold boots from its latest disk checkpoint.
4. **RAM is outside durability.** RAM memory regions leave the loss window and never
   ask for a checkpoint under dirty pressure, since no disk checkpoint relieves
   them. A RAM pager's dirty budget has to hold every private RAM page, and a
   store past it stops the VM as a full budget does today.
5. **A guest flush blocks while the disks are stale.** A flush completes at
   once when the VM's last successful disk checkpoint landed within
   `SPROUTFS_FLUSH_BOUND` (default twice the checkpoint interval, 120 s); otherwise it waits until one lands, and
   the host asks for one out of the interval's turn. A flush never triggers a
   checkpoint on its own. So an fsync that returned is never more than the bound
   plus one interval from durable, and a guest whose disks cannot be published
   stops making fsync progress. A disk checkpoint is a point in time across all
   of a VM's disks, so ordering is a power cut's: nothing after a flush is
   durable without everything before it.
6. **Moving a VM between hosts is unchanged.** A migration and a fork hand RAM
   over pager to pager and upload nothing.

## Steps

1. `Machine.SealDisks` and `host.CaptureDisks`; the interval uses them; the
   simulation's oracle learns that a host loss cold boots from the last disk
   checkpoint.
2. RAM memory regions out of the loss window and pressure; the RAM dirty budget.
3. The flush request: the device, the memory client, the wire (a new frame
   with a reply, wire version 9) and the pager, ending in a callback that
   completes the flush when the host says the disks are fresh enough.
4. Docs: the loss model in architecture.md, Flush in context.md, hosting.md,
   volumes.md, vm-memory.md.
5. Qualification on GCE.
