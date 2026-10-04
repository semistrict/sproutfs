# A new generation and the clock across a restore, on GCE, 2026-10-04

Every child of one fork point and every restore of one checkpoint starts from
the same guest memory, random pool included. Each restore gives the guest a
new VMGenID generation, and on x86_64 now moves the guest's clocks on by the
time its state was stopped
([a new generation and the right clock](../vm-memory.md#a-new-generation-and-the-right-clock)).
This run qualifies both on a real VMM and a real guest kernel (TASK-87).

## What ran

One disposable `n2-standard-8` host in us-east4-a (Intel Cascade Lake, nested
KVM, Ubuntu with Linux 7.0.0-1011-gcp), created, run and deleted by
`scripts/bench-memory-gce.sh` with `SPROUTFS_GCE_QUALIFY=1` and
`SPROUTFS_FIRECRACKER_RUN` naming the tests. The host's clocksource is `tsc`,
with `constant_tsc`, `nonstop_tsc` and `tsc_known_freq`. The guest kernel is
the pinned Firecracker CI build of Linux 6.18.44, with `CONFIG_VMGENID=y` and
`CONFIG_PTP_1588_CLOCK_VMCLOCK=y`. Its image holds the driver's ACPI ID
`VMGENCTR` once, which is what the host's check looks for. The guest's
clocksource is `tsc` too.

The guest init (`vmmachine/testdata/guest.c`) answers `entropy` with 16 bytes
of `getrandom()`, the count of `crng reseeded due to virtual machine fork` in
its kernel log, VMClock's generation counter and disruption marker from
`/dev/vmclock0`, and its wall clock. The host asks over the console, and the
times it asked and read the answer bound the guest's offset from the host's
wall clock. The console is polled every 20 ms, so a quick answer bounds the
offset to about 20 ms.

- `TestForkChildrenOfOnePointDrawDifferentRandomBytes` boots a parent, takes
  one fork point, receives two children onto a second pair of pagers through
  the parent's peer server, and asks each as soon as it runs, then until its
  kernel has reseeded.
- `TestARestoreAfterAStopReseedsAndKeepsTheClock` boots a guest, takes its last
  checkpoint with the guest paused, ends its VMM, waits 10 s, opens the VM on
  another host from that checkpoint and asks it the same.
- The fork's own tests of the clock pairing and the TSC
  (`test_vm_save_state_pairs_the_clock_with_the_wall_clock`,
  `test_vm_restore_state_moves_the_clock_on_by_the_time_stopped`,
  `test_vm_restore_state_without_a_wall_clock_resumes_the_clock_where_it_stopped`,
  `test_advance_tsc_moves_the_restored_tsc_on`) run on the same host's KVM.

## Results

Four runs of the two Firecracker tests, the last five times over: 9 fork points
with 18 children, and 9 restores after a stop. The fork's own tests passed in
every run.

**Random bytes.** No two draws shared their bytes: not a parent's, not a
child's first, not a child's after its reseed. Every child and every restored
guest reported exactly one more reseed than its state held, and both VMClock
counters one higher. The parents reported none.

**The reseed's window.** All 18 children had reseeded by their first answer,
236 to 262 ms after their release (the time includes the receive's fetch of the
parent's pages). Two of the 9 restored guests answered first before their
reseed, 360 ms after release, with VMClock already raised; their next answer,
20 ms later, came after it. Every reseed was seen within 381 ms of release.

**The clock.** At boot each guest's wall clock was 25 to 46 ms behind the
host's. After a restore it was where it had been: every child read the offset
its parent booted with, and every guest opened after its 10 s stop read the
offset it had before the stop, to within the console's 20 ms. Their monotonic
clocks had moved on by the stop, from 146 ms before it to 11.0 s after.

The first run had the fork's clock pairing but not the TSC change. Its guests'
clocksource is the TSC, which KVM's `KVM_CLOCK_REALTIME` does not move, so
the children read 0.51 and 0.87 s further behind the host than their parent:
the time between the fork point and each child's restore. That is what moving
every vCPU's TSC on fixed. The restore test of that run did not start, because
its name made a socket path too long; it was renamed.

The host's clock is the TSC here, so KVM paired the guest's clock with the wall
clock itself. The fork's own pairing, for a host on kvm-clock, ran only in its
unit test on this host.

## Teardown

`scripts/bench-memory-gce.sh delete sproutfs-memprobe-genid-87` deleted the
host and its boot disk and verified both gone, and
`gcloud compute instances list` and `gcloud compute disks list` showed no
`sproutfs-memprobe-` resource afterwards. Raw logs stayed on the machine that
ran the script.
