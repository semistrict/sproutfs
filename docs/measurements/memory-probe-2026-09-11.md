# Guest memory probe follow-up, 2026-09-11

The slow rewrite is reproducible, but the proposed explanation of RAM being
sealed every 32 MiB is contradicted by the measurements. Both managed probes
recorded zero seals, range protections, cut pages, cut writes, evictions and
spills. The pager served roughly 4,160 faults in 1.5 seconds of aggregate handler
time during probes taking 76–101 seconds. These totals are not a wall-time
decomposition: handlers may overlap and exclude time outside the pager.

A phase-aligned profile identifies Apple's nested-memory mapping machinery as
the dominant cost in an isolated plain-guest rewrite. The managed pager is not
required to reproduce the slowdown. Host transparent huge pages remove the
multi-second rewrite in the same plain-guest configuration. The Google Cloud
comparison below checks whether the symptom follows the workload to nested KVM.

## Google Cloud cross-check

The multi-second rewrite does **not** follow the workload to Google Cloud
nested KVM. Three rounds of every configuration returned the expected checksum.
All numbers below are medians; each guest used 2 GiB RAM and the probe touched
1 GiB. Managed guests used 64 KiB pager pages and a 3 GiB resident arena.

| Environment | vCPUs | First fill, s | Rewrite, ms | Sequential read, ms | Random reads, ms |
| --- | ---: | ---: | ---: | ---: | ---: |
| Managed guest | 1 | 6.767 | 94.27 | 88.51 | 35.71 |
| Plain guest, same harness | 1 | 5.363 | 92.44 | 86.63 | 34.02 |
| Managed guest | 4 | 6.793 | 90.58 | 85.22 | 34.11 |
| Plain guest, same harness | 4 | 5.374 | 92.21 | 88.12 | 34.93 |
| Isolated plain, `None` | 1 | 5.359 | 92.00 | 88.03 | 35.11 |
| Isolated plain, `Transparent` | 1 | 0.510 | 89.84 | 85.12 | 24.67 |
| Directly on the cloud VM | — | 0.450 | 88.00 | 84.83 | 24.50 |

[All 21 probe results](memory-probe-2026-09-11/gce/summary.json),
[host environment and artifact hashes](memory-probe-2026-09-11/gce/environment.txt),
[instance configuration](memory-probe-2026-09-11/gce/instance.json).
The surrounding directory contains each original managed JSON and plain JSONL
record. Managed rewrites ranged from 90.4–97.4 ms across both vCPU settings;
plain harness rewrites ranged from 89.7–94.3 ms.

The six managed probe windows recorded 4,141–4,161 faults, about 1.21–1.27 s of
aggregate fault-handler time, and zero seals, protections, cut pages, evictions
or spills. Their rewrite and sequential-read times are close to the cloud-native
probe. The managed first fill remains about 1.4 s slower than plain; aggregate
handler time is consistent with much of that gap but is not a wall-time
attribution. Plain first fill remains about 5.36 s with small host pages and
falls to 0.51 s with host transparent huge pages. Thus nested KVM also has
substantial first-touch costs, while the severe warm rewrite pathology observed
on Lima/VZ is absent here. We did not profile the cloud first-touch path.

This comparison changes architecture and hardware as well as the outer
hypervisor. The cloud host was an Intel Cascade Lake `n2-standard-8` with 32 GiB
RAM and Linux `7.0.0-1011-gcp`. The x86-64 guest kernel was the official
Firecracker CI `6.18.44` artifact. Its minimal static BusyBox root included the
same init and probe sources as the local fixture, compiled for x86-64; this was
not the identical ARM executable or root image. Disassembly confirmed that both
fills remained in the executable. The cloud data supports the local profile's
conclusion, rather than independently isolating an Apple implementation defect.

## Isolated local cause

The isolated runner starts Firecracker directly, with no Go pager or object
store. Three paired one-vCPU runs changed only the VMM's `huge_pages` setting;
the guest still had `transparent_hugepage=never`.

| Plain guest host backing | First fill median, s | Rewrite median, s | Rewrite range, s |
| --- | ---: | ---: | ---: |
| `None` | 8.809 | 2.838 | 2.538–2.901 |
| `Transparent` | 0.443 | 0.01338 | 0.01326–0.01344 |

[Paired raw records](memory-probe-2026-09-11/paired-hugepages.jsonl).
A separate instrumented run measured a 4.164-second rewrite with **zero guest
minor or major faults**. Subsequent rewrites in that process took about 14 ms.
The [phase records](memory-probe-2026-09-11/phase-4k.jsonl) and
[diagnostic source](memory-probe-2026-09-11/memprobe-diagnostic.c) preserve that distinction.

During the slow rewrite, a three-second macOS `sample` of the VZ process
recorded 2,532 samples for its active `cpu-1` thread. Of those, 2,077 (82%) were
inside `Hv::Vm::map_nested_space`, and 1,946 (77%) reached
`find_range_bounds_containing(std::list<HvCore::NestedGuestMemoryMap::MappedRange>&, ...)`
through `GuestHypervisorSpaceManager::map`. These percentages describe this
thread's sampled stacks, not total machine CPU usage or the entire benchmark.
See the [full profile](memory-probe-2026-09-11/vz-rewrite-sample.txt).

This directly locates the sampled stall in the outer hypervisor's nested mapping
path. The huge-page experiment supports mapping granularity as a major factor;
it does not prove a particular algorithmic complexity or explain every earlier
managed-guest second. This is an environment limitation for these measurements,
not evidence that the pager should seal less frequently. Managed RAM currently
does not support the VMM huge-page setting used by the isolated experiment.

## Measurements

Each probe allocates 1 GiB, fills it twice, reads one byte per cache line, and
then performs 1,048,576 random page reads. Both fills are present in the compiled
binary. All runs returned the expected checksum, 35,651,584.

| Environment | vCPUs | First fill, s | Rewrite, s | Sequential read, s | Random reads, ms |
| --- | ---: | ---: | ---: | ---: | ---: |
| Managed guest | 4 | 36.767 | 61.004 | 0.017 | 8.17 |
| Plain guest | 4 | 27.448 | 19.672 | 3.241 | 10.26 |
| Managed guest | 1 | 30.597 | 42.426 | 0.019 | 7.99 |
| Plain guest | 1 | 17.436 | 15.439 | 23.737 | 7.15 |
| Directly in Lima, median of three | — | 0.140 | 0.01357 | 0.01854 | 8.22 |

Raw records: [four vCPUs](memory-probe-2026-09-11-4vcpu.json),
[one vCPU](memory-probe-2026-09-11-1vcpu.json),
[direct execution](memory-probe-2026-09-11-native.log).
Direct execution used the identical cached static executable, after all nested
guests stopped. It removes the nested guest, its kernel and its VMM together;
it does not isolate which layer causes the difference.

The managed probes each flushed only 2,424,832 bytes and uploaded 3,885,323
object bytes. The one-vCPU probe advanced the volume checkpoint sequence from
34 to 35 while recording **zero** RAM sealing or protection activity. Ordinary
volume checkpoints publish bytes already ingested into volumes; they do not
periodically capture guest RAM. See [capture and restore](../vm-memory.md#capture-and-restore).

`CopyOnWrites` includes the initial allocation of private writable pages from
sparse zeroes. It is not a count of post-checkpoint rewrites. Both probes also
mapped about 12,300 additional fresh pages through write-ahead. Ending resident
usage was about 1.24 GiB, below the configured 3 GiB arena budget.

## Sampling and limitations

A three-second KVM exit histogram during the four-vCPU managed probe recorded
3,294 exits and no data-abort exception class. A one-second histogram during the
one-vCPU plain probe recorded 1,098 IRQ exits, all at guest PC `0x400838`, the
probe's sequential `ldrb` instruction. These bounded samples do not cover entire
phases and cannot observe traps handled below Linux KVM.
See [managed exits](memory-probe-2026-09-11-managed-exits.log) and
[plain guest PC samples](memory-probe-2026-09-11-baseline-pc.log).

These timings include the short tracing windows. A separate compile and direct
probe overlapped part of the first managed probe; the direct results in the table
were taken later without a nested guest running. Treat the values as evidence
of the severe slowdown, not precise overhead ratios. The three earlier runs
also showed slow rewrites but lacked probe-time pager counters.

The local machine was an M5 Pro Mac with 48 GiB RAM and macOS 26.4.1.
The host VM was Lima VZ, aarch64, eight CPUs and 16 GiB RAM, with Linux
`7.0.0-29-generic`; guests had 2 GiB RAM, 8 GiB disks, and 64 KiB pager pages.
Guest transparent huge pages were disabled. Lima had no swap configured and
about 15 GiB available before the probes; macOS reported about 4.49 GiB of swap
in use. Swap occupancy alone does not establish active paging of this VM.

The working tree was dirty at commit
`370431c443a637b49e5ce585717db5d8d342138b`. The raw configuration records lack
a revision string. Cached artifact SHA-256 identities:

| Artifact | SHA-256 |
| --- | --- |
| Probe executable | `b6cf22bffe8e6a3150238820b99b508d5293a6155fc6d506ec1b76ad2253678c` |
| Probe source | `e8f4ee716cbb425a56f5ff46d3cf2562e87eeb5fea4d56eab286894e91ebe3d5` |
| Firecracker | `27ef1ca4e9562b72c001d0effcee01a719ab34d2f0e26d689fc77440254288b0` |
| Guest kernel | `013ba9494dfab7d41d406734e2ddd3ad287fb59ef41e94e48048414e7c899f08` |

## Harness correction

The first rerun exposed an unrelated race in the benchmark's disk object store:
`Get` remembered a generation's pathname, waited for simulated latency, then
opened a file that concurrent replacement or deletion had already unlinked.
The store now opens the selected generation under its metadata lock and keeps
that descriptor until the read finishes. A deterministic regression test covers
both overwrite and deletion; both failed before the fix and passed afterwards.
The Linux package's unconfigured tests and Linux-targeted vet also passed;
environment-gated integration tests were not enabled in that package run.

Probe records now retain pager deltas, protection counts and before/after
checkpoint sequences. The diagnostic commands used the existing cached probe
image with `SPROUTFS_BENCH_SCENARIOS=boot,baseline`,
`SPROUTFS_BENCH_RUN='/usr/local/bin/memprobe 1024'`,
`SPROUTFS_PAGE_BYTES=65536`, and `SPROUTFS_BENCH_VCPUS=4` or `1`.
The regular workload image does not currently build the probe executable.


## Cloud adapter correction

The initial GNU/Linux build failed every real memory-client attachment. A syscall
trace showed `mremap(..., MREMAP_MAYMOVE|MREMAP_DONTUNMAP, 0xffffffff) = -1 EINVAL`.
`TrapSource::prepare` had omitted the variadic fifth argument. glibc consumes the
destination hint for `DONTUNMAP` as well as `FIXED`; the
[glibc contract](https://sourceware.org/glibc/manual/latest/html_node/Memory_002dmapped-I_002fO.html)
specifies null to let the kernel choose an address. Adding an explicit null
pointer corrected the call. A standalone C comparison failed with the omitted
argument and succeeded with null, both with and without mapping advice.

The existing real `vmtest` suite went from attachment failures to passing after
this three-line correction, without changing its assertions. A separate
qualification failure involved an obsolete expectation of writes succeeding
during an object-store outage; the concurrent authority work corrected that
test and added explicit refusal checks.

The cloud runner builds Firecracker for musl to match its shipped seccomp policy.
An initial glibc VMM build was rejected at startup for syscall 232 (`epoll_wait`)
when paired with the musl policy. That failed startup produced no guest timings.
The musl build also exposed three glibc-specific type assumptions in `ioctl`
and ancillary-message lengths. Explicit ABI-appropriate casts let both targets
build. The Rust test adapter remains a glibc build, so it continues to exercise
the variadic-argument regression.

## Reproducing the cloud comparison

Run `SPROUTFS_GCE_PROJECT=your-project bash scripts/bench-memory-gce.sh all`.
The default runner creates a disposable `n2-standard-8` in `us-east4-a` with
nested virtualization enabled, builds the current working tree including the
Firecracker submodule, runs real memory-protocol qualification, measures three
rounds per configuration, downloads the records, and deletes the VM and its
boot disk. It verifies both are absent. `create`, `run`, and `delete` expose the
same steps for recovery after an interrupted run; `run` alone leaves the VM
available until explicit deletion or expiry. An existing benchmark VM requires
an explicit instance name for those commands.

The guest startup script installs a systemd timer checking
`/var/lib/sproutfs-bench/lease` every minute. It powers off the host when the file
is at least 24 hours old or missing. The benchmark touches that file every
minute while active. To renew the inactivity lease manually, run
`sudo touch /var/lib/sproutfs-bench/lease` on the VM. A separate GCE
`maxRunDuration=86400s`, `instanceTerminationAction=DELETE` cap deletes the VM
at 24 hours even if the lease keeps being touched. The boot disk is auto-delete.
The timer's check-only mode was tested with fresh and 25-hour-old temporary
lease files: exit statuses were 0 and 10 respectively, and the timer was active.

The independent plain-guest probe is `scripts/bench-plain-memory.py`. It accepts
explicit Firecracker, kernel and root filesystem paths plus `--huge-pages None`
or `--huge-pages Transparent`. It copies the root fixture, starts one VMM, sends
the requested command, records JSON lines, and terminates the VMM and removes
its temporary directory on exit. This path has no Go pager or object store.

The final cloud qualification passed all real `vmtest` and `vmmemory` tests,
including the updated KVM authority case, plus the benchmark-store replacement
regression. Clippy with warnings denied passed for both GNU and musl targets.
`cargo test` also ran for both targets but currently contains zero Rust tests;
the real cross-process Go suites provide the memory-adapter behavioral checks.
See [qualification output](memory-probe-2026-09-11/gce/final-checks.log),
[real client tests](memory-probe-2026-09-11/gce/vmtest.log), and
[managed memory tests](memory-probe-2026-09-11/gce/vmmemory.log).

No pager policy or checkpoint threshold was changed for this diagnosis.
The working tree includes concurrent work and remains uncommitted; the initial
source archive hash and subsequent source-file hashes are retained beside the
records. These are diagnostic measurements from debug VMM builds, not a general
performance qualification.

The temporary GCE instance and its boot disk were deleted after the records
were downloaded, and both absence checks passed. No extra cloud disks, service
accounts, firewall rules or networks were created.
[Cleanup record](memory-probe-2026-09-11/gce/cleanup.log).
