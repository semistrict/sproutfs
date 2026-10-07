# CacheLib's flash engine (Navy) and Kangaroo — 2026-10-02

Research for [the distributed disk cache plan](../../plans/disk-cache-2026-10-02.md).

## Sources

| Tag | Source | Version | License |
| --- | --- | --- | --- |
| cl | github.com/facebook/CacheLib, `~/src/CacheLib` | commit `8ffe18bc3982b01c3d5b89aef2c4a6003684342e`, 2026-10-02 | Apache-2.0 (`LICENSE`, README.md:98-100) |
| CL | Berg et al., "The CacheLib Caching Engine: Design and Experiences at Scale", OSDI 2020, usenix.org/system/files/osdi20-berg.pdf | paper | — |
| KG | McAllister et al., "Kangaroo: Caching Billions of Tiny Objects on Flash", SOSP 2021, pdl.cmu.edu/PDL-FTP/NVM/McAllister-SOSP21.pdf (the authors' copy; SOSP is ACM, not USENIX) | paper | — |

`cl block_cache/BlockCache.cpp:1402` means
`cachelib/navy/block_cache/BlockCache.cpp`, line 1402, at the commit above.
Paths outside `cachelib/navy/` are written from the repository root.
`CL §4.2` and `KG §4.3` are paper sections.

Kangaroo is not in upstream CacheLib; neither the tree nor its history has a
KLog or KSet file. The research code is at github.com/saramcallister/Kangaroo,
last pushed 2021-09-13, and GitHub reports no license for it, so take no code
from it. Upstream Navy has BigHash, the set-associative engine Kangaroo calls
SA.

The paper and the code differ. The paper describes the large object cache
with a B+-tree index, 4 KB alignment and size-class regions (CL §4.2). The
code has a hash index, 512 B alignment and one append-only region per
allocator.

Navy says "reclaim" for wiping a region for reuse. In sproutfs, reclamation
means deleting old checkpoints, and seal means removing a guest's write access
([context](../context.md)). Navy's words appear here only for Navy.

## Navy BlockCache: regions

### Layout

- The device is split into equal append-only regions. Each allocator writes
  into one open region, and an item never spans two
  (cl block_cache/Allocator.cpp:62-120, block_cache/Region.cpp:51-66).
- An item is value, padding, key, then a 32-byte header
  (cl block_cache/BlockCache.h:299-324, block_cache/BlockCache.cpp:702-708).
  The header holds the key size, the value size, a 64-bit key hash, an access
  time, a CRC32 of the header and a CRC32 of the value
  (cl common/Hash.cpp:26-27).
- The index points at the end of an item, so a region can be walked backward
  from its last item. There is no per-region table
  (cl block_cache/BlockCache.h:558-568, block_cache/BlockCache.cpp:673-737).
- Every write first lands in a region-sized DRAM buffer, written to the device
  in one write when the region is full
  (cl block_cache/RegionManager.cpp:128-151, 324-358). The paper credits
  this buffer with lowering fragmentation in CDN caches from 7% to 2%, because
  items can align to 512 B (CL §5.2).

### Region size

- The default region is 16 MiB (cl block_cache/BlockCache.h:66,
  `cachelib/allocator/nvmcache/NavyConfig.h:660`), to amortize flash erasures
  and write strictly in order (CL §4.2).
- The limit is 1 GiB, because the reclaim reads a whole region into DRAM
  (cl block_cache/BlockCache.cpp:76-80,
  block_cache/RegionManager.cpp:384-401).
- There are two region-sized write buffers by default, twice the clean pool
  (`cachelib/allocator/nvmcache/NavyConfig.cpp:169-185`).
- An index address is 32 bits in units of the alloc alignment, at least
  512 B, so the alignment grows with the device: 2 TiB at 512 B
  (cl block_cache/BlockCache.cpp:129-148, block_cache/BlockCache.h:283).
- Kangaroo cites flash erase blocks of about 256 MB (KG §2.2).

### The clean pool and how a region is freed

- The region manager keeps a pool of clean regions, one by default
  (cl block_cache/BlockCache.h:70, `cachelib/allocator/nvmcache/NavyConfig.h:651`).
  The guide says to size it near the regions written per second
  (`website/docs/Cache_Library_User_Guides/Configure_HybridCache.md:238-250`).
- Taking a clean region schedules enough reclaims to bring the pool, plus
  reclaims already running, back to its target
  (cl block_cache/RegionManager.cpp:233-294, especially 266-275).
- A reclaim runs on one of a few worker threads, one by default
  (cl block_cache/RegionManager.cpp:72-79, 360-405). It picks the victim from
  the eviction policy, waits for writers, reads the whole region, and drops or
  rewrites each item. Then it resets the region and returns it to the pool
  (cl block_cache/RegionManager.cpp:469-505).
- A writer never waits on a thread that is not a Navy fiber. With no clean
  region, an insert gets `Retry` (cl block_cache/Allocator.cpp:92-106,
  block_cache/BlockCache.cpp:1279-1304), and its job goes back on the queue
  (cl engine/EnginePair.cpp:122-136). Items are dropped earlier, at
  admission, when too many inserts or bytes are waiting.
- A reinsertion allocates with `canWait` false; if no space is ready, the
  item is evicted (cl block_cache/BlockCache.cpp:1218-1237). This avoids a
  deadlock between reclaim and the writes reclaim makes. Kangaroo keeps one
  free segment per log partition with a background thread for the same reason
  (KG §4.3).
- A failed flush is retried ten times, 100 ms apart. Then the buffer's items
  are dropped from the index and the region goes back to the pool
  (cl block_cache/RegionManager.cpp:324-349).

### Eviction policy

- FIFO and region LRU exist. A hit touches a region only once it is on the
  device (cl block_cache/RegionManager.cpp:93-99,
  block_cache/FifoPolicy.h, block_cache/LruPolicy.h).
- The config default is region LRU (`cachelib/allocator/nvmcache/NavyConfig.h:644`).
  The paper says FIFO is the default and recommends it (CL §4.2).
- Sequential FIFO writes lowered device write amplification from 1.5x to
  1.05x, at a small cost in application writes: 15% fewer NAND writes per
  second (CL §5.2).

### Reinsertion

- `HitsReinsertionPolicy` rewrites an item whose hit count since it was last
  written reaches a threshold (cl block_cache/HitsReinsertionPolicy.cpp:26-38).
  Its comment pairs it with region FIFO
  (cl block_cache/HitsReinsertionPolicy.h:33-38).
- The count resets when the item is written again
  (cl block_cache/SparseMapIndex.cpp:180-197), so each rewrite is paid for by
  at least one read in the last lap of the log.
- The rewrite is a compare-and-swap on the index: it lands only if the key
  still points at the old address (cl block_cache/BlockCache.cpp:1157-1167,
  1246-1256). An item replaced or removed meanwhile is dropped.
- `PercentageReinsertionPolicy` rewrites a random share; its comment says it
  is for testing (cl block_cache/PercentageReinsertionPolicy.h:30-47).
  A newer `ReuseTimeReinsertionPolicy` exists too.
- A new item enters at the lowest priority. A rewritten item's priority is its
  hit count, for segmented FIFO (cl block_cache/BlockCache.cpp:1209-1216).
- Nothing caps rewrite bytes per victim. Rewrites are counted
  (`navy_bc_reinsertion_bytes`) and count against the admission policy's
  write budget, which reads device bytes (cl Factory.cpp:328-332). The paper
  reports that readmission brought CDN's hit ratio close to the specialized
  system with 10% fewer flash writes (CL §6).

### The index

- `SparseMapIndex` is the default. A key's 64-bit hash picks one of 64 Ki
  maps by its high bits and is stored by its low 32 bits
  (cl block_cache/SparseMapIndex.h:191-197). Each record is 8 bytes: a 32-bit
  address, a 16-bit size hint in 512 B units, and hit counters
  (cl block_cache/Index.h:46-73). An item costs at least 12 bytes plus map
  overhead.
- `FixedSizeIndex` is a preallocated table with no rehash. Each slot is a
  5-byte record: address, 2 bits of hits, and a 6-bit size exponent
  (cl block_cache/Index.h:187-297), plus 1 byte of partial key and 2 bits of
  probe offset (cl block_cache/FixedSizeIndex.h:206-224, 623-627). A key
  probes 4 slots (cl block_cache/FixedSizeIndex.h:289). To tell two keys
  apart it reads the key hash back from flash
  (cl block_cache/FixedSizeIndex.h:553-575). It can live in shared memory and
  survive a restart.
- A read fetches the hinted size, reads the real size from the header, and
  reads again only if the hint was short
  (cl block_cache/BlockCache.cpp:1366-1425).
- The paper reports flash index overheads across systems of 8 to 100 B per
  object, and Navy's DRAM overhead below 0.1% (large items) and 0.2% (small
  items) in production (CL §3.2, §5.2).

### A read against a region being freed

Read (cl block_cache/RegionManager.cpp:407-461,
block_cache/BlockCache.cpp:353-398):

1. Load a global sequence number.
2. Look up the index.
3. Open the region for read: raise its reader count, or fail if the region is
   blocked.
4. Load the sequence number again. If it changed, a reclaim finished in
   between: close and look up the index again, once.

Reclaim (cl block_cache/Region.cpp:23-43,
block_cache/RegionManager.cpp:364-405, 469-505):

1. Mark the region. BlockCache allows reads during reclaim
   (cl block_cache/BlockCache.cpp:251), so only writers are waited for.
2. Read the region and remove or move each item in the index.
3. Bump the sequence number.
4. Block new readers, wait for those in flight, reset the region.

### The key check on read

- Every read checks the header CRC, compares the full key on flash with the
  key asked for, then checks the value CRC
  (cl block_cache/BlockCache.cpp:1385-1446). A key mismatch is a miss
  (`navy_bc_lookup_false_positives`). This lets the index hold only a hash
  (CL §4.2 says the same of the paper-era index).
- A checksum error is retried once. If it persists, or the device errored, the
  index entry is removed if it still points there
  (cl block_cache/BlockCache.cpp:409-434).
- Value checksums are on by default; the guide says to keep them on unless a
  higher layer checks (`cachelib/allocator/nvmcache/NavyConfig.h:662`,
  `website/docs/Cache_Library_User_Guides/Configure_HybridCache.md:260-262`).

### Restarts

- Navy persists only at a clean shutdown. `NvmCache::shutDown` drains and
  calls `persist` (`cachelib/allocator/nvmcache/NvmCache.h:1709-1731`), which
  writes the config, each region's fill offset, item count and priority, and
  the whole index into a reserved metadata area
  (cl block_cache/BlockCache.cpp:1574-1587,
  block_cache/RegionManager.cpp:534-557, driver/Driver.cpp:265-272).
- Each metadata record carries a folly RecordIO header whose header and data
  hashes are checked on read (cl serialization/RecordIO.cpp:207-243).
- Recovery requires base offset, size, alloc alignment, checksum flag and
  format version (13) to match
  (cl block_cache/BlockCache.cpp:1604-1626, block_cache/BlockCache.h:292),
  and region count and size (cl block_cache/RegionManager.cpp:559-574). Any
  failure resets the whole cache (cl block_cache/BlockCache.cpp:1589-1602).
- After a good recovery the metadata is zeroed
  (cl driver/Driver.cpp:298-311, serialization/RecordIO.cpp:153-157), so a
  crash any time after start makes the next start cold
  (`cachelib/allocator/nvmcache/NavySetup.cpp:481-489`).
- By default FIFO order resets to region number on restart, which the config
  comment says costs hit ratio. An opt-in flag persists the order
  (cl block_cache/BlockCache.h:104-112,
  block_cache/RegionManager.cpp:585-617).
- BigHash gives each bucket a checksum and a generation. A bucket with a wrong
  checksum or generation is empty. Bumping the global generation invalidates
  every bucket lazily, with no rewrite (cl bighash/Bucket.h:29-46).
- Large hybrid caches take days to warm after a cold restart (CL §3.7, §5.2).

### Direct I/O

- The cache file is opened with `O_DIRECT`. If the filesystem refuses it
  (`EINVAL`, as on tmpfs), Navy falls back to buffered I/O, `fallocate`s the
  whole file and drops it from the page cache with `POSIX_FADV_DONTNEED`
  (cl common/Device.cpp:1148-1231).
- Writes are aligned region buffers, and reads use aligned buffers
  (cl block_cache/RegionManager.cpp:67-70, 128-151). A device flush is an
  `fsync` (cl common/Device.cpp:1063-1065).
- The code gives no reason. Our reading: Navy has its own DRAM cache in front,
  so the page cache would hold the same bytes twice; every read already copies
  into a DRAM item; and buffered writes would not reach the device as the
  region-sized, ordered writes that keep device write amplification near 1x
  (CL §5.2).
- Navy can tag BlockCache and BigHash writes with separate NVMe FDP placement
  handles, so the SSD keeps the sequential and random streams apart
  (cl block_cache/RegionManager.cpp:57, 690-699,
  `website/docs/Cache_Library_User_Guides/FDP_enabled_Cache.md:10`).

## Navy admission

- The driver admits an insert only if the policy accepts it, fewer than
  1,000,000 inserts are in flight and fewer than 256 MiB are waiting
  (cl driver/Driver.cpp:135-179, driver/Driver.h:51-52). Otherwise the insert
  is dropped.
- `RejectRandomAP` admits with a fixed probability
  (cl admission_policy/RejectRandomAP.h). The paper names it the default
  (CL §4.2).
- `RejectFirstAP` admits a key only if it was seen recently or hit in DRAM,
  tracking keys in an approximate split set
  (`cachelib/allocator/NvmAdmissionPolicy.h:176-213`). The paper describes
  it as rejecting the first n DRAM evictions of a key (CL §5.2).
- `DynamicRandomAP` steers toward a target average write rate
  (cl admission_policy/DynamicRandomAP.h:38-50):
  - Every 60 s it reads the device's total bytes written, including rewrites
    and padding (cl admission_policy/DynamicRandomAP.h:64, Factory.cpp:328-332).
  - It computes the rate that would put total writes on target 24 hours from
    now, so unused budget carries over
    (cl admission_policy/DynamicRandomAP.cpp:181-188).
  - The rate is capped, 160 MiB/s by default. The comment calls 120 MB/s a
    usual endurance limit (cl admission_policy/DynamicRandomAP.h:88-96).
  - The admit probability moves at most ±25% per step, within [0.001, 10]
    times the base (cl admission_policy/DynamicRandomAP.h:78,
    admission_policy/DynamicRandomAP.cpp:241-261).
  - The base probability falls with the log of the item size
    (cl admission_policy/DynamicRandomAP.cpp:263-268).
  - Some keys bypass it; their bytes are still charged
    (cl admission_policy/DynamicRandomAP.cpp:114-117, 202-236).
- Admitting every DRAM eviction would write 50% above the rate that lets the
  drives reach their target life. An ML policy that predicts future reads
  wrote 44% fewer bytes with no loss of hit ratio (CL §5.2, Appendix C).
- Kangaroo's experiments assume a 1.92 TB drive rated for three device writes
  per day, 62.5 MB/s sustained (KG §5.1).

## Small objects: BigHash and Kangaroo

- BigHash hashes each key to one 4 KiB bucket on flash with no index. An
  8-byte Bloom filter per bucket in DRAM avoids most reads of empty buckets
  (cl bighash/BigHash.h:47-63, 78,
  `cachelib/allocator/nvmcache/NavyConfig.h:762-764`; CL §4.2 reports more
  than 90%). Every insert rewrites a whole bucket.
- That costs about 6.5x application write amplification and 1.1x to 1.4x
  device write amplification, with flash overprovisioned by 50% (CL §5.2).
  Kangaroo puts the application amplification of such a design at 40x for a
  100 B object (KG §2.3), and shows random 4 KB writes rising from about 1x
  device amplification at 50% full to about 10x at 100% full (KG §2.2,
  Fig. 2).
- Kangaroo puts a small log (KLog, about 5% of flash) in front of a
  set-associative store (KSet, about 95%) (KG §3).
  - Each KLog index bucket is one KSet set. When a log segment is freed, every
    logged object of the same set moves to KSet in one write (KG §4.2-4.3).
  - An object moves only if at least n objects share its set, n = 2 by
    default. With 100 B objects this admits 44% of objects at 23% of the
    write rate (KG §4.3, Table 2).
  - An object hit while in the log is put back at its head (KG §4.3).
  - The index is split into 64 partitions and 2^20 tables. The table is
    implied by the key, so an entry needs only a 9-bit tag, a 19-bit offset
    and a 16-bit next offset. KLog costs 48 bits per object, the whole cache
    about 7 bits (KG §4.2, Table 1).
  - KSet runs RRIP eviction with one DRAM bit per object recording a hit. The
    prediction lives on flash and changes only when the set is rewritten
    (KG §4.4).
- Results at 16 GB DRAM, 1.9 TB flash and 62.5 MB/s: 29% fewer misses than
  SA and 56% fewer than a log with a full index. Kangaroo used 93% of flash,
  SA 81%, the log 61%. Peak throughput was 158 K gets/s against SA's 168 K,
  with a 736 µs p99 (KG §5.2). In a production test, writes fell 38% with
  every object admitted (KG §5.5).
- The paper does not discuss recovery after a crash.

## Lessons for the sproutfs plan

A 2 MiB page's envelope is about 300 KB and a 4 KiB page's about 2 KiB, the
size CacheLib uses to split large items from small (CL §4.2). So the 2 MiB
case fits BlockCache, and the 4 KiB case is where Kangaroo applies. An
identity never names other bytes, so the cache has no update or delete to
lose.

### The layout

- **Keep 64 MiB regions**, on 64 MiB boundaries in the file. Navy's 16 MiB
  limits a reclaim that reads the whole region into DRAM
  (cl block_cache/BlockCache.cpp:76-80); our second chance reads only what was
  read, so a bigger region costs no DRAM. A region holds about 200 envelopes
  of 300 KB.
- **Allocate a region's space when it opens.** Navy `fallocate`s the whole
  file once (cl common/Device.cpp:1204-1221). Our sparse file shares a
  filesystem with spill files and other writers, so a write mid-region can
  fail for want of space. `fallocate` 64 MiB when opening a region, and do not
  open one that cannot be backed.
- **Give each envelope a header** with its own checksum and length, as Navy
  does per item (cl block_cache/BlockCache.cpp:673-737). Keep the table at
  region end, so a restart reads one table per region; the header makes a
  region readable by a scan if its table is lost.
- **Sync a full region before its table.** With buffered writes the kernel may
  write the table before its envelopes. The key check and SHA-256 catch that,
  but restarts would trust tables over missing bytes. At region end:
  `fdatasync` (or `sync_file_range` with wait) the envelopes, write the table,
  sync again. This also bounds a region's dirty page cache.
- **Call a full region "closed"**, since "sealed" means removing a guest's
  write access ([context](../context.md)).

### The index in memory

- **Window index.** Like Kangaroo's buckets (KG §4.2), one window entry shares
  its key across its pages, at about 4 bytes per page: 2 GB per terabyte of
  2 KiB envelopes, against 6 GB at Navy's 12 bytes per item
  (cl block_cache/SparseMapIndex.h:191-197, block_cache/Index.h:73).
- **Bound the sparse case.** One 4 KiB page faulted into a window still makes
  a whole entry; with a 512-bit present map that is about 100 bytes per page,
  up to 50 GB per terabyte of scattered faults. A sparse window should hold a
  short list of (page, length), and the memory charge must be the real size.
- **Key the window by an 8-byte hash.** The full key (checkpoint reference,
  volume, span) is tens of bytes. Navy keeps only a hash and relies on the key
  check after the read (cl block_cache/BlockCache.cpp:1402-1410; CL §4.2); the
  plan checks the key beside every envelope. A collision costs one read.
- **Add a read bit per page** for the second chance, like Kangaroo's bit
  (KG §4.4) and Navy's hit counter (cl block_cache/Index.h:187-297), and count
  it in the budget.
- **Correct the plan's count.** At 2 MiB pages, a terabyte holds about
  3.5 million 300 KB envelopes, not half a million.

### Eviction

- The plan's second chance is Navy's hits policy: rewrite only what was read
  since written, and clear the bit on rewrite
  (cl block_cache/HitsReinsertionPolicy.cpp:26-38,
  block_cache/SparseMapIndex.cpp:180-197; KG §4.3). Its "stop when the free
  region is full" is Navy's rule of dropping a rewrite with no room
  (cl block_cache/BlockCache.cpp:1218-1237).
- **Guarantee progress.** If every envelope in the victim was read, the second
  chance rewrites all 64 MiB: one region freed, one used, the goal never met.
  Navy has the same gap, hidden by its write budget (cl Factory.cpp:328-332).
  Cap the second chance at a share of the victim, for example half, and at
  zero when the cache is over its share or the write budget is spent. The
  model should check "held bytes reach the goal" as a liveness property.
- **Rewrite by compare-and-swap**, as Navy does
  (cl block_cache/BlockCache.cpp:1246-1256), so a window that gained a newer
  region while the victim was read is not pointed back.
- **Keep reader counts; the key check is the backstop.** Navy waits for reads
  in flight before reusing a region
  (cl block_cache/RegionManager.cpp:407-461, 469-505). `checkpoint/disk.go`
  already counts readers (checkpoint/disk.go:346-385); keep that in the new
  log. A peer's `sendfile` is a reader too, so give it a deadline; after it,
  the region may be reused and the receiver's key check makes the result a
  miss.
- **Free regions ahead of demand**, in the background behind the limiter,
  never on the fill path, as Navy does
  (cl block_cache/RegionManager.cpp:233-294, engine/EnginePair.cpp:122-136,
  driver/Driver.cpp:135-179). Make the spare count a setting; Navy's default
  is one.

### Restarts

- **Keep reading tables back.** Navy recovers only after a clean shutdown
  (cl driver/Driver.cpp:298-311), because its keys can be overwritten and
  tables written at region end could bring back stale values. Our identities
  never name other bytes, so a table written when a region closes is safe
  after any crash.
- **Write an order into every table**: a sequence number from a counter in
  the header. Navy lost hit ratio when its FIFO order reset on restart
  (cl block_cache/BlockCache.h:104-112). The restart rebuilds the FIFO from
  it, and when a second chance left a window in two regions, the newer copy
  wins.
- **Put the format in the header, and give regions a generation.** Navy
  refuses recovery on any format mismatch
  (cl block_cache/BlockCache.cpp:1618-1626); our header should hold a format
  version and the region size beside the deployment and the cache's identity.
  As BigHash does (cl bighash/Bucket.h:29-46), a generation in the header and
  each table lets a deployment change empty the cache without touching the
  file: regions whose table has an old generation are free.

### Serving without a copy

- **Keep buffered I/O.** Navy's reasons for `O_DIRECT`
  (cl common/Device.cpp:1148-1231; CL §4.2, §5.2) do not apply: our owner
  serves bytes it never decodes, and `sendfile` needs the page cache. Its
  costs:
  - Dirty pages: bounded by syncing each region when it closes.
  - Duplicates: after a local read, the envelope stays in the page cache while
    the decoded page sits in the memory tier. Drop the range after a local
    read unless the window is hot.
  - The copy it saves is one 300 KB copy per envelope, small next to the
    decode at 2 GB/s of network. Keep "Serving a peer copies nothing into
    memory" only if a measurement shows the page cache costs less.
- Navy checks a value CRC32 beside the key check
  (cl block_cache/BlockCache.cpp:1432-1446). The plan's envelope SHA-256 in
  place of the wire CRC32C is stronger.

### The disk limiter

- **Add a write budget.** The limiter has space goals only. Navy's main
  control is a daily average write rate with a burst cap
  (cl admission_policy/DynamicRandomAP.cpp:162-253); admitting everything
  wrote 50% above the endurance target (CL Appendix C). Our writers are
  store-read fills, publications, pulls, keeps from peers, copies of hot
  windows and second chances; a suspend that publishes 8 GiB writes 8 GiB to
  some owner's disk.
  - Add a goal of bytes per day, with a burst cap and Navy's "on target
    24 hours from now" rule (cl admission_policy/DynamicRandomAP.cpp:181-188).
  - Count what reaches the device (`/proc/diskstats`), as Navy does
    (cl Factory.cpp:328-332), so rewrites and padding count.
  - Over budget, drop by priority: a suspending stop's pages and fills that
    serve a restart last, second chances and copies of hot windows first.
    Navy's random policy costs hit ratio (CL §5.2).
- **Measure admitting a store-read window on its second miss**, as
  `RejectFirstAP` does (`cachelib/allocator/NvmAdmissionPolicy.h:176-213`).
  It would halve writes for cold windows read once. Never apply it to
  publications, which serve restarts.
- **Free space is the SSD's overprovisioning.** Random writes on a nearly full
  SSD reach about 10x amplification (KG §2.2), against near 1x for Navy's
  sequential regions (CL §5.2). Spill files beside our regions write pages at
  random, and a punched hole frees blocks only if the filesystem passes the
  discard on. Mount with discard (or run `fstrim`). GCE does not publish its
  local SSDs' endurance, so measure the write rate in the GCE test and set the
  budget from it.

### Not worth taking

- Kangaroo's set-associative store exists because a 100 B object costs too
  much in a log's index (KG §2.3). Our window index already shares the key
  across 512 pages. Revisit only if sparse 4 KiB windows dominate a measured
  disk.
- Navy's region LRU, segmented FIFO and priorities add little over FIFO with
  a second chance for one class of immutable envelopes.
