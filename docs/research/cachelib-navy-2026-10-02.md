# CacheLib's flash engine (Navy) and Kangaroo — 2026-10-02

Research for [the distributed disk cache plan](../../plans/disk-cache-2026-10-02.md).

## Sources

| Tag | Source | Version | License |
| --- | --- | --- | --- |
| cl | github.com/facebook/CacheLib, `~/src/CacheLib` | commit `8ffe18bc3982b01c3d5b89aef2c4a6003684342e`, 2026-10-02 | Apache-2.0 (`LICENSE`, README.md:98-100) |
| CL | Berg et al., "The CacheLib Caching Engine: Design and Experiences at Scale", OSDI 2020, usenix.org/system/files/osdi20-berg.pdf | paper | — |
| KG | McAllister et al., "Kangaroo: Caching Billions of Tiny Objects on Flash", SOSP 2021, pdl.cmu.edu/PDL-FTP/NVM/McAllister-SOSP21.pdf (the authors' copy; SOSP is ACM, not USENIX) | paper | — |

A citation such as `cl block_cache/BlockCache.cpp:1402` means
`cachelib/navy/block_cache/BlockCache.cpp`, line 1402, at the commit above.
Paths outside `cachelib/navy/` are written in full from the repository root.
`CL §4.2` and `KG §4.3` are paper sections.

Kangaroo is not in upstream CacheLib. Neither the tree nor its history has a
KLog or KSet file. The research code lives at github.com/saramcallister/Kangaroo,
last pushed 2021-09-13, and GitHub reports no license for it. So it is a
design to learn from, never code to take. Upstream Navy has BigHash, the
set-associative engine Kangaroo calls SA.

The paper and the code differ in places. The paper describes the large object
cache with a B+-tree index, 4 KB alignment and size-class regions (CL §4.2).
The code today has a hash index, 512 B alignment and one append-only region
per allocator (below). The code is what runs; the paper is the reasoning.

Navy says "reclaim" for wiping a region for reuse. In sproutfs,
reclamation means deleting old checkpoints, and seal means removing a guest's
write access ([context](../context.md)). This note uses Navy's words only
when it describes Navy.

## Navy BlockCache: regions

### Layout

- The device is split into equal regions. A region is append-only. Each
  allocator writes into one open region, and an item never spans two
  (cl block_cache/Allocator.cpp:62-120, block_cache/Region.cpp:51-66).
- An item is laid out as value, padding, key, then a 32-byte header at the end
  (cl block_cache/BlockCache.h:299-324, block_cache/BlockCache.cpp:702-708).
  The header holds the key size, the value size, a 64-bit key hash, an access
  time, a CRC32 of the header and a CRC32 of the value
  (cl common/Hash.cpp:26-27).
- The index points at the end of an item, so a region can be walked backward
  from its last item without any table (cl block_cache/BlockCache.h:558-568,
  block_cache/BlockCache.cpp:673-737). There is no per-region table.
- Every write first lands in a region-sized buffer in DRAM. The buffer is
  written to the device in one write when the region is full
  (cl block_cache/RegionManager.cpp:128-151, 324-358). The paper credits
  this buffer with lowering fragmentation in CDN caches from 7% to 2%, because
  items can then align to 512 B (CL §5.2).

### Region size

- The default region is 16 MiB (cl block_cache/BlockCache.h:66,
  `cachelib/allocator/nvmcache/NavyConfig.h:660`). The paper's reason is to
  amortize flash erasures and write strictly in order (CL §4.2).
- The hard limit is 1 GiB. The reclaim reads a whole region into DRAM, so a
  larger region means larger allocations (cl block_cache/BlockCache.cpp:76-80,
  block_cache/RegionManager.cpp:384-401).
- The open region costs DRAM too. There are two region-sized buffers by
  default, twice the clean pool (`cachelib/allocator/nvmcache/NavyConfig.cpp:169-185`).
- An address in the index is 32 bits in units of the alloc alignment, at least
  512 B. So the alignment grows with the device: 2 TiB at 512 B
  (cl block_cache/BlockCache.cpp:129-148, block_cache/BlockCache.h:283).
- Kangaroo cites flash erase blocks of about 256 MB (KG §2.2).

### The clean pool and how a region is freed

- The region manager keeps a pool of clean regions, one by default
  (cl block_cache/BlockCache.h:70, `cachelib/allocator/nvmcache/NavyConfig.h:651`).
  The guide says to size it near the regions written per second
  (`website/docs/Cache_Library_User_Guides/Configure_HybridCache.md:238-250`).
- Taking a clean region schedules enough reclaims to bring the pool, plus the
  reclaims already running, back to its target
  (cl block_cache/RegionManager.cpp:233-294, especially 266-275).
- A reclaim runs on one of a few worker threads, one by default
  (cl block_cache/RegionManager.cpp:72-79, 360-405). It picks the victim from
  the eviction policy, waits for writers to finish, reads the whole region,
  walks its items, and drops or rewrites each one. Then it resets the region
  and returns it to the pool (cl block_cache/RegionManager.cpp:469-505).
- A writer never waits on a thread that is not a Navy fiber. With no clean
  region, an insert gets `Retry` (cl block_cache/Allocator.cpp:92-106,
  block_cache/BlockCache.cpp:1279-1304), and its job goes back on the queue
  (cl engine/EnginePair.cpp:122-136). Items are dropped earlier, at the
  door, when too many inserts or bytes are waiting (see admission below).
- A reinsertion never waits either. It allocates with `canWait` false. If no
  space is ready, the item is evicted instead
  (cl block_cache/BlockCache.cpp:1218-1237). This is how Navy avoids a
  deadlock between reclaim and the writes reclaim makes. Kangaroo does the same
  thing with one free segment per log partition, kept by a background thread
  (KG §4.3).
- A failed flush is retried ten times, 100 ms apart. Then the buffer's items
  are dropped from the index and the region goes back to the pool
  (cl block_cache/RegionManager.cpp:324-349).

### Eviction policy

- FIFO and region LRU exist. A hit "touches" a region only once it is on the
  device (cl block_cache/RegionManager.cpp:93-99,
  block_cache/FifoPolicy.h, block_cache/LruPolicy.h).
- The config default is region LRU (`cachelib/allocator/nvmcache/NavyConfig.h:644`).
  The paper says FIFO is the default and recommends it (CL §4.2).
- Sequential FIFO writes lowered device write amplification from 1.5x to
  1.05x, at a small cost in application writes. The net was 15% fewer NAND
  writes per second (CL §5.2).

### Reinsertion: the second chance

- `HitsReinsertionPolicy` rewrites an item whose hit count since it was last
  written reaches a threshold (cl block_cache/HitsReinsertionPolicy.cpp:26-38).
  Its comment pairs it with region FIFO
  (cl block_cache/HitsReinsertionPolicy.h:33-38).
- The count resets when the item is written again
  (cl block_cache/SparseMapIndex.cpp:180-197). So an item is rewritten once
  per lap of the log only if it was read during that lap. Each rewrite is paid
  for by at least one read.
- The rewrite is a compare-and-swap on the index: it lands only if the key
  still points at the old address (cl block_cache/BlockCache.cpp:1157-1167,
  1246-1256). An item replaced or removed meanwhile is simply dropped.
- `PercentageReinsertionPolicy` rewrites a random share. Its own comment says
  it is for testing (cl block_cache/PercentageReinsertionPolicy.h:30-47).
  A newer `ReuseTimeReinsertionPolicy` exists too.
- A new item enters at the lowest priority. A rewritten item's priority is its
  hit count, for segmented FIFO (cl block_cache/BlockCache.cpp:1209-1216).
- Nothing caps rewrite bytes per victim. Rewrites are counted
  (`navy_bc_reinsertion_bytes`) and land on the device, so they count against
  the admission policy's write budget, which reads device bytes
  (cl Factory.cpp:328-332). The paper reports that adding readmission
  brought CDN's hit ratio close to the specialized system with 10% fewer
  flash writes (CL §6).

### The index

- `SparseMapIndex` is the default. A key's 64-bit hash picks one of 64 Ki
  maps by its high bits and is stored by its low 32 bits
  (cl block_cache/SparseMapIndex.h:191-197). Each record is 8 bytes: a 32-bit
  address, a 16-bit size hint in 512 B units, and hit counters
  (cl block_cache/Index.h:46-73). So an item costs at least 12 bytes plus the
  map's overhead.
- `FixedSizeIndex` is a preallocated table with no rehash. Each slot is a
  5-byte record: address, 2 bits of hits, and a 6-bit size exponent
  (cl block_cache/Index.h:187-297). Beside it are 1 byte of partial key and
  2 bits of probe offset per slot (cl block_cache/FixedSizeIndex.h:206-224,
  623-627). A key probes 4 slots (cl block_cache/FixedSizeIndex.h:289). When
  it must tell two keys apart, it reads the key hash back from flash
  (cl block_cache/FixedSizeIndex.h:553-575). It can live in shared memory and
  survive a restart.
- The size is a rounded hint. A read fetches the hinted bytes, reads the real
  size from the header, and reads again only if the hint was short
  (cl block_cache/BlockCache.cpp:1366-1425).
- The paper reports flash index overheads across systems of 8 to 100 B per
  object, and Navy's DRAM overhead below 0.1% (large items) and 0.2% (small
  items) in production (CL §3.2, §5.2).

### A read against a region being freed

The read protocol (cl block_cache/RegionManager.cpp:407-461,
block_cache/BlockCache.cpp:353-398):

1. Load a global sequence number.
2. Look up the index.
3. Open the region for read. This raises the region's reader count, or fails
   if the region is blocked.
4. Load the sequence number again. If it changed, a reclaim finished in
   between: close, look up the index again, once.

The reclaim side (cl block_cache/Region.cpp:23-43,
block_cache/RegionManager.cpp:364-405, 469-505):

1. Mark the region. BlockCache allows reads during reclaim
   (cl block_cache/BlockCache.cpp:251), so only writers are waited for.
2. Read the region and remove or move each item in the index.
3. Bump the sequence number.
4. Block new readers and wait for those in flight. Then reset the region.

### The key check on read

- Every read checks the header CRC, then compares the full key stored on
  flash with the key asked for, then the value CRC
  (cl block_cache/BlockCache.cpp:1385-1446). A key mismatch is a miss
  (`navy_bc_lookup_false_positives`). This is what lets the index hold only a
  hash (CL §4.2 says the same of the paper-era index).
- A checksum error is retried once. If it persists, or the device errored, the
  index entry is removed if it still points there
  (cl block_cache/BlockCache.cpp:409-434).
- Value checksums are on by default, and the guide says to keep them on unless
  a higher layer checks (`cachelib/allocator/nvmcache/NavyConfig.h:662`,
  `website/docs/Cache_Library_User_Guides/Configure_HybridCache.md:260-262`).

### Restarts

- Navy persists only at a clean shutdown. `NvmCache::shutDown` drains and then
  calls `persist` (`cachelib/allocator/nvmcache/NvmCache.h:1709-1731`).
  Persist writes the config, every region's fill offset, item count and
  priority, and the whole index into a reserved metadata area
  (cl block_cache/BlockCache.cpp:1574-1587,
  block_cache/RegionManager.cpp:534-557, driver/Driver.cpp:265-272).
- Each metadata record carries a folly RecordIO header, whose header and data
  hashes are checked on read (cl serialization/RecordIO.cpp:207-243).
- Recovery rejects the data unless base offset, size, alloc alignment,
  checksum flag and format version all match. The format version is 13
  (cl block_cache/BlockCache.cpp:1604-1626, block_cache/BlockCache.h:292).
  Region count and region size must match too
  (cl block_cache/RegionManager.cpp:559-574). Any failure resets the whole
  cache (cl block_cache/BlockCache.cpp:1589-1602).
- After a good recovery, the metadata is zeroed at once
  (cl driver/Driver.cpp:298-311, serialization/RecordIO.cpp:153-157). So a
  crash at any time after start leaves no metadata, and the next start is
  cold (`cachelib/allocator/nvmcache/NavySetup.cpp:481-489`).
- By default, FIFO order is not kept across a restart. It resets to region
  number, and the config's comment says that costs hit ratio. An opt-in flag
  persists the order (cl block_cache/BlockCache.h:104-112,
  block_cache/RegionManager.cpp:585-617).
- BigHash gives each bucket a checksum and a generation. A bucket whose
  checksum or generation is wrong is treated as empty. Bumping the global
  generation invalidates every bucket lazily, with no rewrite
  (cl bighash/Bucket.h:29-46).
- The paper reports that large hybrid caches take days to warm after a cold
  restart (CL §3.7, §5.2).

### Direct I/O

- The cache file is opened with `O_DIRECT`. Only if the filesystem refuses it
  (`EINVAL`, as on tmpfs) does Navy fall back to buffered I/O. It then
  `fallocate`s the whole file and drops it from the page cache with
  `POSIX_FADV_DONTNEED` (cl common/Device.cpp:1148-1231).
- Every write is an aligned region buffer, and reads use aligned I/O buffers
  (cl block_cache/RegionManager.cpp:67-70, 128-151). A device flush is an
  `fsync` (cl common/Device.cpp:1063-1065).
- The code gives no reason. Our reading of the design gives three. Navy has
  its own DRAM cache in front, so the page cache would hold the same bytes
  twice. Every
  read already copies into a DRAM item. And a buffered write would not reach
  the device in the region-sized, ordered writes that keep device write
  amplification near 1x (CL §5.2).
- Navy can tag BlockCache and BigHash writes with separate NVMe FDP
  placement handles, so the SSD keeps the sequential and random streams apart
  (cl block_cache/RegionManager.cpp:57, 690-699,
  `website/docs/Cache_Library_User_Guides/FDP_enabled_Cache.md:10`).

## Navy admission: guarding the SSD's endurance

- The driver admits an insert only if the policy accepts it and two bounds
  hold: at most 1,000,000 inserts in flight and 256 MiB of bytes waiting
  (cl driver/Driver.cpp:135-179, driver/Driver.h:51-52). Over a bound, the
  insert is dropped.
- `RejectRandomAP` admits with a fixed probability
  (cl admission_policy/RejectRandomAP.h). The paper names this the default
  (CL §4.2).
- `RejectFirstAP` admits a key only if it was seen recently, or it was hit in
  DRAM. It tracks keys in an approximate split set
  (`cachelib/allocator/NvmAdmissionPolicy.h:176-213`). The paper describes
  it as rejecting the first n DRAM evictions of a key (CL §5.2).
- `DynamicRandomAP` steers toward a target average write rate
  (cl admission_policy/DynamicRandomAP.h:38-50):
  - Every 60 s it reads the device's total bytes written
    (cl admission_policy/DynamicRandomAP.h:64, Factory.cpp:328-332). That
    count includes rewrites and padding.
  - It computes the rate that would put total writes on target 24 hours from
    now. So a quiet hour earns budget for a busy one
    (cl admission_policy/DynamicRandomAP.cpp:181-188).
  - That rate is capped by a maximum, 160 MiB/s by default. The comment
    calls 120 MB/s a usual endurance limit
    (cl admission_policy/DynamicRandomAP.h:88-96).
  - It moves the admit probability by at most ±25% per step, within
    [0.001, 10] times the base (cl admission_policy/DynamicRandomAP.h:78,
    admission_policy/DynamicRandomAP.cpp:241-261).
  - Larger items are admitted less often: the base probability falls with the
    log of the size (cl admission_policy/DynamicRandomAP.cpp:263-268).
  - Some keys can bypass it. Their bytes are still charged to the budget
    (cl admission_policy/DynamicRandomAP.cpp:114-117, 202-236).
- The paper's numbers: admitting every DRAM eviction would write 50% above
  the rate that lets the drives reach their target life. An ML policy that
  predicts future reads wrote 44% fewer bytes with no loss of hit ratio
  (CL §5.2, Appendix C).
- Kangaroo's experiments assume a 1.92 TB drive rated for three device writes
  per day, which is 62.5 MB/s sustained (KG §5.1).

## Small objects: BigHash and Kangaroo

- BigHash hashes each key to one 4 KiB bucket on flash and keeps no index.
  An 8-byte Bloom filter per bucket in DRAM avoids most reads of empty
  buckets (cl bighash/BigHash.h:47-63, 78,
  `cachelib/allocator/nvmcache/NavyConfig.h:762-764`; CL §4.2 reports more
  than 90%). Every insert rewrites a whole bucket.
- That costs about 6.5x application write amplification and 1.1x to 1.4x
  device write amplification. To hold device amplification there, the flash
  is overprovisioned by 50% (CL §5.2). Kangaroo puts the application
  amplification of such a design at 40x for a 100 B object (KG §2.3). It
  also shows random 4 KB writes rising from about 1x device amplification at
  50% full to about 10x at 100% full (KG §2.2, Fig. 2).
- Kangaroo puts a small log (KLog, about 5% of flash) in front of a
  set-associative store (KSet, about 95%) (KG §3).
  - Each KLog index bucket is one KSet set. When a log segment is freed, every
    logged object of the same set moves to KSet in one write (KG §4.2-4.3).
  - An object moves only if at least n objects share its set, n = 2 by
    default. With 100 B objects this admits 44% of objects at 23% of the
    write rate (KG §4.3, Table 2).
  - An object hit while in the log is put back at its head instead of being
    dropped (KG §4.3). That is a second chance.
  - The index is split into 64 partitions and 2^20 tables. The table is
    implied by the key, so an entry needs only a 9-bit tag, a 19-bit offset
    and a 16-bit next offset. KLog costs 48 bits per object, and the whole
    cache about 7 bits per object (KG §4.2, Table 1).
  - KSet runs RRIP eviction with one DRAM bit per object. The bit records a
    hit. The prediction itself lives on flash and changes only when the set is
    rewritten anyway (KG §4.4).
- Results: 29% fewer misses than SA and 56% fewer than a log with a full
  index, at 16 GB DRAM, 1.9 TB flash and 62.5 MB/s. Kangaroo used 93% of
  flash, SA 81%, the log 61% (KG §5.2). Peak throughput was 158 K gets/s
  against SA's 168 K, with a 736 µs p99 (KG §5.2). In a production test,
  writes fell 38% with every object admitted (KG §5.5).
- The paper does not discuss recovery after a crash.

## Lessons for the sproutfs plan

The plan's envelopes are large next to Navy's objects. A 2 MiB page's
envelope is about 300 KB, and a 4 KiB page's is about 2 KiB. That is the size
CacheLib uses to split large items from small ones (CL §4.2). So the 2 MiB
case is an easy BlockCache, and the 4 KiB case is exactly where Kangaroo
starts to matter. Our one advantage over both: an identity never names other
bytes, so the cache has no update and no delete to lose.

### The layout

- **Region size: keep 64 MiB.** Navy's 16 MiB is tuned for small items and
  for a reclaim that reads the whole region into DRAM
  (cl block_cache/BlockCache.cpp:76-80). Our second chance reads only what
  the table and the index mark as read, so a bigger region costs no DRAM. At
  300 KB a region holds about 200 envelopes, so a region is not too coarse a
  unit of FIFO. Regions should start on a 64 MiB boundary in the file.
- **Allocate a region's space when it opens.** Navy `fallocate`s the whole
  file once (cl common/Device.cpp:1204-1221). Our file is sparse and shares a
  filesystem with spill files and other writers. Without that, a write in the
  middle of a region can fail for want of space. So the limiter should
  `fallocate` all 64 MiB when it opens a region, and refuse to open one it
  cannot back. A region then either has its space or is never opened.
- **Make the key beside each envelope a real header.** Navy puts key, sizes,
  a key hash and two CRCs at the end of every item, and walks a region
  backward without any table (cl block_cache/BlockCache.cpp:673-737). Our
  plan's table at the end is still worth having: it is what a restart reads,
  and one small read per region beats walking 64 MiB. But a header with its
  own checksum and the envelope's length makes a region readable without its
  table. Then a lost table costs a scan, not the region.
- **A sealed region must be on the disk before its table counts.** With
  buffered writes, the kernel may write the table before the envelopes it
  describes. The key check and SHA-256 still catch that, so it is not a
  correctness bug, but restarts would then trust tables over missing bytes.
  At the end of a region: `fdatasync` (or `sync_file_range` with wait) the
  envelopes, write the table, sync again. This also bounds the dirty page
  cache a region can hold. Navy's `O_DIRECT` writes give it this for free.
- **Word choice.** "Sealed" already means removing a guest's write access
  ([context](../context.md)). The plan should use another word for a region
  that is full, such as "closed".

### The index in memory

- **The window index is Kangaroo's main trick, and it is right.** Kangaroo
  saves DRAM by making entries share the bits that the bucket already implies
  (KG §4.2). Our window entry does the same: one header per window, and only
  a length per page. Navy's per-item index costs at least 12 bytes
  (cl block_cache/SparseMapIndex.h:191-197, block_cache/Index.h:73). At
  500 million 2 KiB envelopes per terabyte, that is 6 GB. Our plan's about
  4 bytes per page is 2 GB.
- **Bound the sparse case.** The 4-byte figure holds only for a full window.
  A fault that fills one 4 KiB page of a window still makes a whole window
  entry. If the entry has a 512-bit present map, one page costs about 100
  bytes. A disk filled by scattered faults could then cost 50 GB per
  terabyte. The entry should switch to a short list of (page, length) for a
  sparse window. The memory charge must be the real size, not the 4-byte
  average.
- **Key the window by a hash.** A window's full key (checkpoint reference,
  volume, span) is tens of bytes. Navy keeps only a hash and relies on the
  key check after the read (cl block_cache/BlockCache.cpp:1402-1410;
  CL §4.2). Our plan already checks the key beside every envelope, so an
  8-byte hash per window is enough. A collision costs one wasted read.
- **Add the read bit.** The second chance needs to know what was read since
  it was written. That is one bit per page, like Kangaroo's one bit
  (KG §4.4) and Navy's hit counter (cl block_cache/Index.h:187-297). Count it
  in the budget.
- **Fix one number in the plan.** At 2 MiB pages, a terabyte holds about
  3.5 million envelopes of 300 KB, not half a million. Still nothing worth
  counting.

### What comes out

- **The second chance matches Navy's hits policy.** Rewrite only what was
  read since it was written, and clear the bit when rewritten
  (cl block_cache/HitsReinsertionPolicy.cpp:26-38,
  block_cache/SparseMapIndex.cpp:180-197). Kangaroo does the same in its log
  (KG §4.3). Keep it. Navy's percentage policy is for tests only.
- **Never wait to make room for a second chance.** Navy's rewrites never
  block; an item with no room is dropped (cl block_cache/BlockCache.cpp:1218-1237).
  The plan's "stop when the free region is full" is the same rule. Keep it.
- **But the plan does not guarantee progress.** If every envelope in the
  victim was read, the second chance rewrites all 64 MiB into the region kept
  free. One region is given back and one is used. The disk holds as much as
  before, so the limiter's goal is never met, and the SSD writes 64 MiB for
  nothing. Navy has the same gap; its write budget hides it
  (cl Factory.cpp:328-332). Cap the second chance at a share of the victim,
  for example half. Then each eviction gives back at least half a region.
  When the cache is over its share, or the write budget is spent, the share
  should fall to zero. The model should check this as a liveness property
  ("held bytes reach the goal"), not only deadlock freedom.
- **Rewrite in index order, by compare-and-swap.** Navy moves an index entry
  to the new copy only if it still points at the old one
  (cl block_cache/BlockCache.cpp:1246-1256). Our window entries need the same:
  a window that gained a newer region while the victim was read must not be
  pointed back.
- **Keep reader counts; the key check is the backstop, not the guard.** Navy
  waits for reads in flight before reusing a region, and uses a sequence
  number to send a read that raced a reclaim back to the index
  (cl block_cache/RegionManager.cpp:407-461, 469-505). `checkpoint/disk.go`
  already counts readers and gives blocks back after the last one
  (checkpoint/disk.go:346-385). Keep that in the new log. A peer's
  `sendfile` is a reader too. A slow socket could then hold a region for a
  long time, so give each `sendfile` a deadline. Once it passes, the region
  may be reused, and the receiver's key check turns the result into a miss.
- **Free regions ahead of demand.** Navy frees regions on background threads
  so that a clean one is waiting. A writer that finds none is queued again,
  and the queue's bounds drop work at the door
  (cl block_cache/RegionManager.cpp:233-294, engine/EnginePair.cpp:122-136,
  driver/Driver.cpp:135-179). The plan's bounded fill queue is the same
  shape. Eviction should run in the background behind the limiter, never on
  the fill path. One spare region is Navy's default; make the count a
  setting.

### Restarts

- **The plan goes further than Navy, and that is right.** Navy recovers only
  after a clean shutdown and zeroes its metadata once it has read it
  (cl driver/Driver.cpp:298-311). Any crash means a cold cache. Navy cannot
  do better cheaply: its keys can be overwritten and removed, so tables
  written at region end could bring back stale values. Our identities never
  name other bytes, so a table written when a region closes is safe to trust
  after any crash. Keep reading tables back.
- **Write an order into every table.** Navy lost hit ratio when its FIFO order
  reset to region number on restart, and had to add an option to keep it
  (cl block_cache/BlockCache.h:104-112). Each table should carry a sequence
  number from a counter in the header. The restart rebuilds the FIFO from it.
  It also decides which copy wins when a second chance left a window in two
  regions: the newer one.
- **Put the format in the header, and give regions a generation.** Navy
  refuses recovery unless size, offsets, alignment, checksum mode and format
  version all match (cl block_cache/BlockCache.cpp:1618-1626). Our header
  should hold a format version and the region size beside the deployment and
  the cache's identity. BigHash invalidates every bucket at once by bumping a
  generation, with no rewrite (cl bighash/Bucket.h:29-46). A generation in
  the header and in each table would let a deployment change empty the cache
  without touching the file. Regions whose table has an old generation are
  free.

### Serving without a copy

- **Navy chose `O_DIRECT`, and its reasons do not transfer.** Navy copies
  every hit into a DRAM item anyway, has its own DRAM cache in front, and
  wants its writes to reach the SSD as whole regions
  (cl common/Device.cpp:1148-1231; CL §4.2, §5.2). Our owner serves bytes it
  never decodes, and `sendfile` needs the page cache. Keep buffered I/O.
- **Take the costs Navy avoided seriously.**
  - Dirty pages: bound them by syncing each region when it closes (above), so
    a fill burst does not grow the pod's dirty memory.
  - Duplicates on one host: when this host reads its own disk, the envelope
    stays in the page cache while the decoded page sits in the memory tier.
    Drop the range after a local read unless the window is hot, as the plan
    already does for the second chance.
  - Measure the copy that `O_DIRECT` would cost. Without `sendfile`, a
    300 KB envelope costs one copy of 300 KB. At 2 GB/s of network that is
    small next to the decode. The property "Serving a peer copies nothing
    into memory" is worth keeping only if the measurement shows the page
    cache costs less than that copy.
- **CRC on cache replies.** Navy checks a CRC32 of the value even beside the
  key check (cl block_cache/BlockCache.cpp:1432-1446). The plan replaces the
  wire CRC32C with the envelope's SHA-256, which is stronger. Agreed.

### The disk limiter

- **Add a write budget.** The plan's limiter has goals for space only. Navy's
  main control is a write rate: a daily average measured from the device's
  own byte count, with a cap on bursts (cl admission_policy/DynamicRandomAP.cpp:162-253).
  CacheLib found that admitting everything wrote 50% above the endurance
  target (CL Appendix C). Our writers are store-read fills, publications,
  pulls, keeps from peers, copies of hot windows and second chances. A
  suspend that publishes 8 GiB writes 8 GiB to some owner's disk.
  - The limiter should take one more goal: bytes per day, with a burst cap.
  - It should count what reaches the device (`/proc/diskstats` for the
    cache's disk), not what the cache meant to write, as Navy does
    (cl Factory.cpp:328-332). Rewrites and padding are then counted.
  - Over budget, it should drop by priority, not at random. A suspending
    stop's pages and fills that serve a restart come first. Second chances
    and copies of hot windows come last. Navy's random policy pays for its
    simplicity with hit ratio (CL §5.2).
  - Navy's "on target 24 hours from now" rule is a good shape: a quiet night
    earns a busy morning (cl admission_policy/DynamicRandomAP.cpp:181-188).
- **Admit a store-read window on its second miss?** `RejectFirstAP` admits a
  key only on a repeat (`cachelib/allocator/NvmAdmissionPolicy.h:176-213`).
  For store reads of cold windows, that would halve writes for windows read
  once. It must not apply to publications, which serve restarts. This is a
  candidate for measurement, not a default.
- **Free space is the SSD's overprovisioning.** Navy keeps device write
  amplification near 1x with sequential region writes (CL §5.2). Random
  writes on a nearly full SSD reach about 10x (KG §2.2). Our regions are
  sequential, but spill files beside them write 4 KiB and 2 MiB pages at
  random, and punching a hole frees blocks only if the filesystem passes the
  discard on. So mount the cache's filesystem with discard (or run `fstrim`),
  and treat the limiter's free-space goal as overprovisioning, not slack.
  GCE does not publish its local SSDs' endurance, so measure the write rate
  in the GCE test and set the budget from it.

### Things to leave alone

- Kangaroo's set-associative store is not worth it here. It exists because a
  100 B object costs too much in a log's index (KG §2.3). Our window index
  already shares the key across 512 pages. Revisit only if sparse 4 KiB
  windows dominate a measured disk.
- Navy's region LRU default, segmented FIFO and priorities add little over
  FIFO with a second chance for one class of immutable envelopes.
- Kangaroo's code has no license. Learn from the paper; take no code.
