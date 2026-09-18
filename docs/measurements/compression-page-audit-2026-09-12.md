# Compression and page-unit audit, 2026-09-12

Scope: align storage chunks with the existing 2 MiB pager, compress persisted
and transferred payloads, and audit assumptions inherited from the original
4 KiB pager. There is no existing data to migrate. Formats change together;
legacy readers are deliberately absent.

## Findings and corrections

| Surface | Finding | Correction or retained contract |
| --- | --- | --- |
| Fault read-ahead | The old maximum of 4,096 pages allowed an 8 GiB scratch range at 2 MiB per page. | Cap the byte range at 16 MiB, including region overrides: eight production pages. |
| Initial population | A 65,536-page window grew from 256 MiB to 128 GiB and could enumerate millions of storage extents. | Derive the page count from a 256 MiB byte window. |
| Immutable resident identity | A production page previously combined two 1 MiB chunk identities. | One 2 MiB chunk identifies a production page. Mixed delta lineage cannot share a whole page. |
| Storage deltas | Doubling a chunk doubles its 4 KiB page count. | 512 pages, a 64-byte bitmap, and updated format version and generated schema. Exercise the final bitmap bit. |
| Flush defaults | A full chunk now needs 512 pages in the 4 KiB model, beyond the old 256-page validator. | Allow 512 pages while retaining the 2 MiB byte bound. Production flushes one page. |
| Native test policies | Fixed 32/64-page read-ahead policies exceeded the restored byte bound. | Use eight production pages; retain fault-count and byte-content assertions. |
| Real-guest pressure | After chunk alignment, the old boot workload stayed below the 96 MiB arena and stopped exercising spill. | Source and fork each dirty 48 MiB, then verify markers in every 4 KiB subpage; require actual eviction, spill and refault. |
| Subpage test markers | A one-byte ordinal repeats after 1 MiB, allowing swapped halves to evade comparison. | Add the high ordinal byte to distinguish every subpage in both simulated spill and real-guest pressure checks. |
| Fixture geometry | Some tests' old 1 MiB offsets and sizes no longer crossed a storage boundary. | Express boundary fixtures using `image.ChunkSize`; explicitly retain old-half-boundary and final-subpage reads. |

The scan covered Go pager geometry, lineage, copy-on-write, spill, flush,
capture, migration, storage indexes and caches; Rust mapping alignment and
protocol ranges; Firecracker RAM/PMEM mapping and capture; and scripts and
native fixtures. Integer counts were checked against their consumers rather
than mechanically replacing every occurrence of 4,096.

The following small units remain intentional:

- Checkpoint deltas use 4 KiB edits inside 2 MiB objects. Small disk writes and
  partial volume reads remain valid; a cold object read fetches and decodes
  the whole object before returning the requested bytes.
- Guest filesystem blocks, guest workload touches, ordinary host pages,
  `mincore` results and KVM dirty bitmap bits are not pager page counts.
  Managed capture seals pager regions; it does not reinterpret KVM bitmap
  bits as 2 MiB pages.
- Scaled 4/16/64 KiB simulation geometries are retained. The Rust/native
  adapter requires 2 MiB alignment, including rejection of 4 KiB and 1 MiB
  offsets, lengths and backing offsets.
- Mapping command and queue limits count metadata entries. They do not
  allocate a payload buffer of that many full pages. Pre-copy already
  derives its batch count from 16 MiB, yielding eight production pages.

## Compression contract

`internal/blob` supplies a versioned, length-bounded envelope with a decoded
SHA-256 checksum, fast Zstandard, and raw fallback. Maximum encoded expansion
is 48 bytes. Each blob is independently readable; no external dictionary is
needed. Four shared synchronous workers bound concurrent codec workspaces.
Their internal channels are initialized outside Go `synctest` bubbles.

Volume records are encoded before replication. Replica journals, repair and
recovery retain those exact bytes. Admission and restored pending-byte budgets
charge uncompressed record lengths. Chunks, deltas, checkpoint indexes and
checkpoint VMM state are compressed before upload; cache entries remain
decoded. Publication retries compare verified decoded bytes, including when
the prior object used a different valid encoding.

Migration page replies declare their payload format. The receiver checks the
bitmap and bounds decompression by the exact number of present pages before
installing bytes; malformed replies fall back to the volume. Source budgets
remain based on logical page bytes. Guest RAM, scratch spill and the runtime
`Handoff.State` field remain uncompressed.

Existing publication planning retains raw upload buffers; this change does
not turn publication into a streaming pipeline. Compression adds bounded
concurrent work and buffers under the existing upload slots. Raw fallback
limits stored expansion, not the cumulative allocations spent attempting
compression.

## Codec measurements

Apple M5 Pro, macOS arm64, Go 1.26.6, 2 MiB synthetic inputs, 250 ms benchmark
samples. These are codec controls, not guest-workload throughput or proof that
this codec outperforms every alternative.

| Input | Stored bytes | Encode | Decode | Encode allocations |
| --- | ---: | ---: | ---: | ---: |
| All zero | 465 | 0.861 ms | 1.344 ms | 22,483 B/op |
| Half deterministic random, half zero | 1,048,881 | 1.036 ms | 1.058 ms | 5,161,108 B/op |
| Deterministic random | 2,097,200 | 1.264 ms | 0.719 ms | 10,928,278 B/op |

Sparse zero storage is normally omitted before encoding. The all-zero input
isolates codec behavior. Incompressible input uses raw fallback; decoding
that envelope aliases its payload and allocates nothing in this benchmark.

## Verification and limits

New regression coverage checks compressed publication and partial reads,
final bitmap bits, same-length conflicting retries, equivalent raw/compressed
retries, corruption and decompression bounds, log restart with exact retained
bytes and restored logical budgets, malformed migration replies, and every
4 KiB subpage through 2 MiB copy-on-write, spill and writeback.

The native aarch64 memory suite and all ten normally ignored Rust protocol
tests passed with a temporarily provisioned HugeTLB pool. The real Firecracker
capture/fork/fencing and live migration suites also passed. With a 96 MiB
arena, the pressure fixture observed 144 evictions, 119 spills and 88 spill
refaults while preserving guest markers. The pool was restored to zero pages
and temporary guest directories were verified absent. The Go race suite,
`go vet ./...`, Linux amd64 compilation and a 100,000-input codec fuzz run
passed. A preceding three-second fuzz run ended with the fuzz driver's
`context deadline exceeded`; it reported no failing input. The fixed-count
run completed normally.

Repeated overlap scenarios used four seeds, two runs and `GOMAXPROCS=1,4`.
All 24 execution comparisons and all 24 adapter comparisons matched in each
scenario. Full diagnostic event traces differed in 24/24 volume comparisons
and 18/24 migration comparisons. This is consistent with the documented
distinction between controlled boundaries and arbitrary scheduler submission
order; it is not a claim of complete scheduler determinism.

This run does not qualify x86_64 guests spanning the MMIO memory gap or
production workload performance. The corresponding mapping code was audited,
but aarch64 native execution cannot establish x86_64 runtime behavior.

Raw [native guest evidence](compression-page-audit-2026-09-12/linux-firecracker.log),
[codec measurements](compression-page-audit-2026-09-12/codec-bench.log) and
[source hashes](compression-page-audit-2026-09-12/source-sha256.json) are retained
locally under the measurement artifact ignore rules.

## Closeout review

The scoped implementation snapshot and the later three-line marker refinement
were each reviewed with `autoreview --mode local --no-web-search
--max-priority P2`, with scope supplied through `--prompt`. Both returned no
actionable P0–P2 findings; none were rejected or suppressed. The second review
covered only the marker refinement against the already reviewed snapshot.
The final source hashes match the reviewed files. Temporary review worktrees
were removed; the main checkout remains uncommitted.
