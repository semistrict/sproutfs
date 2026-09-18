# Host resource completion audit

Scope: the user's later clarification supersedes the original goal's list of
individual RAM allocations. Huge-page backing and retained object cache share
the RAM allowance. Ordinary heap, log/overlay objects, codec and I/O scratch,
and Go/VMM runtime overhead use deployment-provided container headroom.
The disk allowance covers owned file data and reserved working space, with
allocation-unit rounding. Actual filesystem pressure, including metadata and
unrelated host processes, is handled by free-space monitoring and cache-first
space-error handling. This is reservation management, not an OS-enforced quota.

| Requirement | Current implementation and inspected proof |
| --- | --- |
| One owner across host, pager and caches | `HostConfig.Resources`, log owner validation, `Host.AddMachine` and migration received-region checks reject foreign budgets. Host owner tests and migration suite pass. |
| Count shared pages once; admit before allocating | `takeFree` reserves a run before arena allocation; aliases reference slots. `TestSharedPageIsChargedOnceUntilLastAliasDetaches`, physical-cleanup failure tests and the 2 MiB cache-first fault test pass. |
| Cache yields before required refusal | Required acquire/grow/protect/workspace paths invoke registered evictors outside the budget lock. Pinned bytes remain charged; finishing readers yield to queued work. Image cache resource tests, console tests and replay-headroom test pass. |
| Required reads work when pages fill RAM | Cache misses use an unretained transient copy from I/O headroom when no retained reservation fits. The full-page read regression checks data and absence of retained cache. |
| Disk shared across journals, compaction and spill | Journals require reconciled `resource.Disk`; compaction uses the same owner and Progress class. Pager reserves destination disk before exposing writable RAM and retains it after failed punching. Compaction-at-capacity, replay and spill tests pass. |
| Progress reserves inside totals | Protected copy and state workspace use a shared maximum behind one reclaim permit. Ordinary admission cannot spend it. Copy reserve, cancellation/FIFO, workspace and capture-at-capacity tests pass. |
| VMM staging and console covered | State/config/restore use the owned disk. Capture reserves an enforced 64 MiB bound and rounded workspace before pausing, checks real space, stops ambiguous writers before cleanup. Console payload is rounded and truncation is synced before release; empty files yield inodes. Native capture/kernel-limit, console and reader replacement tests pass. |
| Reconcile after restart; preserve ambiguous ownership | Durable scans replace reservations atomically; over-limit files remain removable. Failed close and namespace outcomes retain ownership. Scratch inherits an exclusive lock into children, refuses live reconciliation and retries stopped-process cleanup. Resource and native scratch tests pass. |
| Filesystem allocation and unrelated use | File and copy reservations round per file, keeping logical reserved ends separate. The default 10% floor checks filesystem-wide available space on fills and every second. Same-filesystem cache is reclaimed on actual ENOSPC/EDQUOT, including safe namespace mutations and initial scratch setup. Native block/inode exhaustion and deterministic external-pressure tests pass. |
| Remove superseded quotas | Production Go search finds no ExpectedVMs, MaxJournalBytes, CacheBytes or cache CapacityBytes. Scratch/journal byte counters remain diagnostic. Concurrency, record, dirty-page and arena-geometry bounds remain intentional correctness bounds. |
| Preserve durability, fencing, capture, forks, migration and compression | Full Go race suite covers these integrations, including compressed objects/partial reads, compressed log replay, capture/fork and scheduled/chaotic migration. Linux amd64 compiles. Native Linux arm64 covers actual filesystem errors, startup, state writer limits and scratch/process ownership. |

## Verification and limits

`go test -race ./...` and `go vet ./...` completed. The full Linux amd64
compile-only run completed. Logs are `/tmp/sproutfs-resource-final-*.log` and
`/tmp/sproutfs-allocation-linux.log`. Linux arm64 native race runs exercised
resource, real-disk and VMM cases (`/tmp/sproutfs-allocation-native.log`). The
root-only mount run skipped one permission-denial test; it was rerun as an
unprivileged user and passed (`/tmp/sproutfs-allocation-unprivileged.log`).
The subsequent native scratch run covers initial ownership setup at actual
inode exhaustion (`/tmp/sproutfs-startup-space-native.log`).

The new accounting regressions initially demonstrated byte-length undercharging
and an undersized copy reserve. They now assert allocation rounding, block
boundary refusal, logical append increments, durable release and external-write
bounds. No test encodes the presence of a bug as its successful outcome.

Source review completed with no findings. The helper initially refused a dirty
submodule gitlink whose contents were absent from its bundle. The completed run
used an isolated flattened snapshot containing both root and Firecracker
changes, including untracked source files. Its three complete review passes
covered a 1,045,019-byte bundle. No finding required acceptance or rejection.
The working repositories remained unchanged by review setup.

Review command: `autoreview --mode local --prompt-file
plans/host-resource-audit.md --output /tmp/sproutfs-resource-review.md
--json-output /tmp/sproutfs-resource-review.json`. The helper exited zero;
review details are retained in those output files. Chunked source review
complements the inspected ownership invariants and behavioral tests above.

This does not assert a fresh full KVM workload qualification, a hard guarantee
of filesystem free space against concurrent external writers, arbitrary Go
scheduler determinism, or exact process RSS accounting. None of those is the
user's corrected resource-management requirement.


Latest closeout checks: focused resource/platform/VMM race tests and vet passed
following the initial-scratch admission fix. Native startup, capture, console
and scratch tests passed without skips in
`/tmp/sproutfs-resource-closeout-native.log`. The final tmpfs mount list matches
the pre-test list. The isolated Linux build cache and platform test binary were
removed and absence checked; HugeTLB remained zero. No cloud resources or global
kernel settings were created or changed.

Review snapshot measurements: 189 changed files across root and dependency;
4,984 added and 2,041 removed non-test source lines, including generated Go and
inherited Firecracker/Rust changes. This is the entire dirty-worktree footprint,
not just the allocation-rounding increment. The snapshot uses the exact root
and dependency HEADs as its baseline and copies every tracked change and
unignored new file into ordinary files so the dependency content is reviewable.


## Outcome

The corrected resource-management scope is implemented and verified. No required
implementation or validation item remains open in this audit. The documented
limits above remain deployment constraints, not unfinished per-buffer accounting.
No commit or push was made.
