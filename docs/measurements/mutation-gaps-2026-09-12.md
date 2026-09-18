# Mutation gap follow-up — 2026-09-12

This follow-up is **in progress**. It closes the three named priority gaps,
adds regular determinism assertions, and fixes two bugs exposed while examining
uncovered paths. The original campaign's counts remain
unchanged; these are later replays of its retained mutated programs.

## Additional behavioral coverage

Nine previously surviving canonical Go programs now fail tests:

| Boundary | Newly detected programs | Intended behavior |
| --- | --- | --- |
| Wire checksums | `49676a0e67d8`, `ea9556ed64c0` | Every undefined checksum enum is rejected, including empty payloads. |
| Wire payload descriptors | `803b1a658551`, `f337a0fb773b` | Unchecksummed nonempty bytes and checksummed empty payloads retain their descriptors and integrity checks. |
| Empty checksum readers | `0bf9c55e3b63`, `06d7d7968aa9` | Empty checksums need no reader; nonempty checksums require one. |
| Membership snapshot cache | `1249ade030e1` | An older in-flight snapshot read cannot evict a newer cached snapshot. |
| Atomic certificate exchanges | `79267e026a2d`, `6d9b30845a2b` | A certificate update can replace a full set or grow it while removing old identities, then replay idempotently. |

The checksum suite also asserts fixed digest values and lengths. The membership
test uses explicit gates to force the older request to complete after the newer
one and checks cache reuse through the public registry/store boundary.

All nine canonical wire survivors were replayed: six failed tests and three
survived. Two are the previously reviewed empty-hash-write equivalents. The
third adds a redundant empty, unchecksummed wire descriptor; its final
classification remains open.

The final fresh-build replay of all 70 original Rust survivors now detects
45; 25 still survive. Three detected mutations remove
`Session::drop`, remove `Mapping::drop`, or invert its unmap condition. The
ordinary mapping test keeps the backing descriptor alive and checks that its
mapping disappears. The native session test performs a real UFFD/SCM_RIGHTS
handshake, drops the session while retaining a control handle, and requires
pending operations to fail, new operations to reject the closed session, peer
EOF, and mappings to disappear. It needs Linux UFFD permission but no KVM or
physical HugeTLB allocation.

An initial address-only mapping probe raced with address reuse by another
test. The final tests identify backing/VMA names in `/proc/self/maps` instead.
The earlier failed baseline is retained as superseded test-design evidence,
not a production regression. All three drop mutations were replayed again
against the final test version, with an unmutated baseline first.

Eight additional native tests exercise `Session::connect` and `Session::run`
through a real Unix protocol peer and detect another 42 original survivors:

- Attachment fields and READY validation, including the valid 64-region limit.
- Invalid ranges, flags, backing offsets and stale IDs; rejected commands leave
  generations unchanged, and an immediate identical retry remains valid.
- Batch headers rejected before reading runs, matching command IDs, ordered
  disjoint ranges, and generation advancement for every original range after
  adjacent replacements merge.
- Acceptance of exactly 1024 runs and complete error acknowledgements when a
  batch exceeds its mapping budget.

The peer drains UFFD remap events independently of the control stream, so real
replacement operations can complete. These tests need UFFD permission but no
physical huge pages. The 1024-run test reserves slightly over 4 GiB of virtual
address space; the final replay used an 8 GiB address-space limit and a
30-second deadline. It did not allocate a HugeTLB pool.

The replay also exposed a harness problem: Cargo can reuse a previous mutant's
binary when copied sources preserve their timestamps. Nine cases in the first
lifecycle replay had `fresh: true`; their individual outcomes are superseded.
The separate final three teardown replays and all eight Rust timeout replays
had fresh builds. The new complete 70-case run cleans the crate before every
build, requires `fresh: false`, and records source and executable hashes. Its
unmutated baseline and all 70 builds were independently checked. The initial
failed baseline caused by reuse is retained as harness evidence, not a product
regression. Original campaign counts remain frozen.

The final unmutated suite passes all 37 tests with native tests enabled, both
serially and under the test runner's default parallelism. Running the ordinary
suite as the unprivileged Linux user passes 28 tests and leaves nine native
tests ignored. All 183 test PIDs from this investigation are gone, every
per-case temporary directory was empty, and the owned native workspace and
input files were removed. Both huge-page settings remain zero.

## Uncovered paths and confirmed bugs

A fresh whole-module coverage run classified the original 153 unselected Go
locations: 48 fall outside instrumented coverage blocks, 94 remained uncovered,
nine are Linux-only, and two were now covered. Constants and switch conditions
account for many of the 48; absence from a coverage block is not proof that a
test cannot detect their mutation. Several uncovered immutable-envelope helpers
also have no production callers in the current tree.

Direct overlays exercised the 48 locations outside coverage blocks and the two
newly covered locations, using the exact token mappings from Gremlins 0.6.0.
These 50 locations produce 49 distinct programs: 35 failed tests, nine survived,
four failed to build, and one timed out. Every program ran the full module
suite with an unmutated baseline first. These are direct operator replays,
not a second native Gremlins campaign; the retained selection and mapping source
make that distinction explicit.

Two previously uncovered paths exposed actual bugs:

- `Random.Chance` accepted `NaN` as a probability and silently returned false.
  It now rejects `NaN` alongside other values outside the probability domain.
  Tests also cover impossible/certain events and the range of floating samples.
- Canceling the first queued volume write removed it without reconsidering the
  next waiter. A smaller write could remain blocked despite available host
  budget. Cancellation now grants fitting requests immediately. The regression
  uses real simulated volumes and held checkpoint uploads, and also checks
  cancellation in the middle of the queue, budget accounting and resulting
  bytes through public APIs.

Both regressions failed on the previous unmutated tree for their stated
behavior and passed after the fixes. The whole Go module subsequently passed
under the race detector. This verification is separate from mutation outcomes.

## Timeout and resource investigation

All eight original Rust timeout mutations were replayed under a three-second
process deadline and a 1 GiB address-space limit. The unmutated unit baseline
passes; all eight mutated programs still time out. Six remove progress from
generation-range walking; two retry non-interruption socket errors forever.
Existing tests already reach these paths. They remain liveness failures,
classified as timeouts rather than assertion detections.

Both original Linux process/resource mutations were reproduced with individual
test PIDs and memory samples under a 3 GiB address-space limit and 512 MiB RSS
watchdog. They grow the page vector without advancing through its requested
range. The unmutated probe passes; both mutations reach a resource limit.
The probe uses the public connection, attachment and population APIs, an
acknowledging Unix peer and a pipe deliberately rejected by the final UFFD wake
operation. It stays outside the normal suite and allocates no physical huge
pages. This establishes a reproducible cause for the mutations' resource
failure; it does not retroactively establish the missing original PID/OOM-log
association. Intermediate fixture attempts and final evidence are retained.

All 160 distinct macOS timeout programs, representing 171 original invocations,
were replayed with JSON test output, process-group deadlines and memory bounds.
The results are 154 timeouts, two memory-watchdog failures and four passing
programs. Fifty timeout dumps include a panic or failed-test cleanup. The
original native watchdog often left empty logs; these replays retain the running
test and its goroutine dump. Process-group cleanup initially raced with exited
children; the final worker checks for live non-zombie members, and every replay
group was subsequently verified to have no remaining members.

Three of the four newly passing programs now fail focused regressions:
`84cc54443a6b` and `bdd64961b568` are detected by collection-cadence assertions
through `StartNetwork`; `5fecb781eb9b` is detected by checking that a checkpoint
preserves an untouched page's lineage between two disjoint writes. Byte equality
alone had allowed the latter mutation to rewrite unchanged data under a new
identity. The remaining program changes the drain-result comparator and needs
further review. The 14 original Linux timeouts also remain under investigation.
All original classifications stay frozen.

The first collection-cadence fixture crossed its deadline while preparing a
new snapshot. Its failed baseline and early mutation results are superseded by
the final fixture, which prepares validated snapshots before timing delivery.
The final baseline passes under the race detector and both exact mutations fail.

## Determinism constraints

The ordinary Go suite now runs five cross-process checks: dynamic simulator
overlap, log, volume, migration and host workloads. Each runs the same compiled
test binary in three fresh processes with `GOMAXPROCS=1,4,4`, seed 1, and both
caller creation orders. Existing model assertions remain active. Execution and
adapter recordings must be nonempty and byte-for-byte identical, including
event order, times and outcomes. A mismatch reports the first differing record.

A temporary overlay deliberately made scheduler seeding depend on
`GOMAXPROCS`. The new dynamic-workload test failed on a changed execution
stream, and the unmutated version passed. All five new tests passed normally
and under the race detector. The final timeout/signal reporting adjustment was
followed by another normal run of all five checks. Temporary process fixtures
also verified that the mutation auditor classifies a killed child as a process
failure and a stopped child's expired deadline as a timeout, rather than
counting either as an assertion detection. No fixture process remained.

These tests constrain observable behavior for exercised schedules. They do not
enumerate all interleavings or assert that goroutines arrive at gates in the
same order. A separate four-process observation found four full arrival-trace
mismatches, with zero execution or adapter mismatches across six comparisons.
The stricter standalone trace script still reports those full-trace differences
and exits nonzero; its contract was not weakened.

## Evidence and remaining work

[The evidence archive](mutation-gaps-2026-09-12/followup-evidence.tar.gz)
contains exact overlays/diffs, replay commands and outcomes, raw test logs,
source snapshots/hashes, and Linux environment and cleanup records. The
[progress inventory](mutation-gaps-2026-09-12/progress.json) separates later
detections from the frozen original counts.

The later [Rust protocol evidence](mutation-gaps-2026-09-12/rust-protocol-evidence.tar.gz)
contains each test snapshot, the earlier cache-reuse failure, the build audit,
all replay logs and exact diffs, final verification, and cleanup records.
The [final Rust replay report](mutation-gaps-2026-09-12/rust-protocol-final-report.json)
supersedes the first lifecycle replay's individual outcomes. The remaining 25
survivors include shared-backing merge arithmetic, write protection, mapping
advice and kernel capability checks; passing protocol-only cases do not prove
those behaviors equivalent. Conditional equivalence notes are retained in the
archive for the remaining size and bit-mask operators.

The wider goal remains open: review the remaining meaningful Go and Rust
survivors, cover the meaningful remaining unselected paths, and finish the Go
timeout investigation. The temporary Linux replay directories and session
sockets are gone; both huge-page settings remain at their original zero values.
