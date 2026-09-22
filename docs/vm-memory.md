# Managed VM memory

A VM's RAM and PMEM are [volumes](volumes.md). The Go package `vmmemory` is the
pager: shared resident pages, fault resolution, private copy-on-write pages,
scratch spill and eviction. A host runs one instance of it per kind of region —
one for its guests' RAM and one for their PMEM disks — each with an arena of its
own, a spill file of its own and a page of its own. The independent Rust library
in `rust/sproutfs-vm-memory` owns the mappings inside one VMM process and has no
Firecracker dependency. `vmmachine` supervises the process; `host` pauses and
seals a VM, for a checkpoint or for a fork point. The `vmtest` package is
the low-level syscall fixture for mapping races and malformed commands.

Three bodies of `vmmemory` are nested packages, importable from `vmmemory`
alone: `internal/pageranges` is the interval map page state is held in,
`internal/latency` the fixed log-scale histograms of the fault path, and
`internal/slots` the arena free set and the consecutive runs of it one mapping
command covers. Each reaches nothing of the pager — no host lock, no region, no
page. Everything else stays in `vmmemory`, because the arena, the spill file,
the UFFD session and the binding blocks all read and write the pager's state
under its metadata lock: they are files of one package, not packages.

## Why a pager of its own

The pager does for a guest's memory what the kernel's page cache and swap do
for a file: it decides which pages are resident, faults the rest in on demand,
shares one resident page among every mapping that names it, tracks what the
guest dirtied, writes it back, evicts, and spills. The kernel already has all
of that, and the integration does not use it. The first reason rules it out on
its own; the rest would each rule it out too.

1. **The page cache duplicates what VMs share.** It caches per file. Once each
   VM has its own writable copy of an image — a reflink included, since a
   reflinked file is its own inode with its own cached pages — a fleet of VMs
   from one image holds one copy of the same bytes per VM, and the kernel has
   no way to know they are the same. The guest does it again: a guest with a
   virtio-blk disk keeps its own page cache of that disk in its RAM, so the
   same inherited bytes are resident once more per VM, invisible to the host.
   The pager keeps one resident page per page identity, however many VMs map
   it, and the guest reaches it through PMEM over DAX, mapping the host's page
   directly, so the host's one copy is the only copy. That is held to at three
   places: a VM has no block device at all, only PMEM; the host refuses a guest
   command line without `rootflags=dax=always`, under which ext4 fails the
   mount, and so the boot, where DAX is not to be had; and the guest's witness
   refuses a file on a PMEM device that the kernel does not report as DAX.
   The guest's init also remounts the root `noatime`: an image's files all
   have an access time no newer than their modification time, so under the
   kernel's `relatime` a fork that only reads writes the inode of every file it
   reads, and dirties pages it shares — guest memory for the inode, root pages
   when the journal commits. It is a flag of the mount rather than of ext4, so
   `rootflags=` cannot carry it: ext4 refuses the option and the root does not
   mount.
2. **Sharing is by name, across VMs and across checkpoints.** A child's memory
   is its parent's checkpoints plus its own writes, over tens of thousands of
   pages. As file mappings that is one VMA per run,
   against the kernel's mapping limit, and rearranged at every checkpoint.
3. **Durability is one pause of the whole machine, not writeback.** The kernel
   writes dirty pages back when it chooses. A checkpoint needs every dirty page
   frozen at one moment while the guest keeps running on them — write
   protection and copy-on-write the pager controls through userfaultfd — and
   the pages take a new name once the upload lands.
4. **The backing is not only a file.** After a fork or a migration a page's
   bytes are on another host that still holds them. The page cache faults from
   a filesystem; a filesystem in front of the store serves reads, but it cannot
   fault a page out of a peer host's memory, and every fault would go through
   the kernel and back out at 4 KiB.
5. **The budgets are the host's.** Resident, logical and dirty pages are
   admitted explicitly, so a guest that dirties faster than it publishes is
   checkpointed out of turn or stalled, rather than growing until the kernel
   swaps or kills something. The pages are HugeTLB, which the kernel does not
   swap at all, so the pager owns eviction and spill in any case.
6. **It has to run inside the simulation.** The same pager, on a simulated
   arena, disk and clock, is what the deployment's campaigns exercise. The
   kernel's page cache cannot be put inside a deterministic harness.

Upstream Firecracker's own userfaultfd restore copies pages into each VM's
anonymous memory and cannot share a page between VMs; the integration replaces
that path with this one.

## One pager per kind of region

A pager instance is built with a fixed page, and everything it does is counted
in it: its arena slots, its spill slots and the extent of its spill file, its
resident, logical and dirty budgets, the read-ahead and write-ahead runs a
deployment states in bytes and each instance converts, its buffers, the
comparison a settle makes, the byte conversions of `RegionStats` and `Sharing`,
the size and alignment it requires of a region, and the fault arithmetic of the
Linux connection. The page must be one a volume can be published in —
`checkpoint.GeometryFor` is the one place that says which those are — so a
pager and the volumes it maps cannot disagree about what a page number means.
A volume published in another page size is refused when it is attached, which
is also how a region that reached the wrong pager of a host is caught.

A host assembles two: `vmmemory.Pagers` is the RAM pager and the PMEM pager, and
a region attaches to the one of its own kind. Nothing adds their page counts
together — a RAM page and a PMEM page need not be the same number of bytes — so
everything a host reports across the two is in bytes: the two arenas' capacities
and the two dirty budgets come to what the deployment gave the host, the sharing
gauges are reported per kind under one metric name with a `kind` label, and
`Host.PrivateBytes` adds a VM's regions up across both. A checkpoint asked for by
either pager's pressure seals the whole VM, once, because one pause seals every
region it maps; a VM's loss window is the oldest unpublished write across its
regions in both; and a store neither pager can admit stops that VM alone.

**RAM's page is 4 KiB and PMEM's is 2 MiB**, on a real host and in the
simulation alike. The page and the memory behind it are one statement: a 2 MiB
page is a page of the host's provisioned HugeTLB pool, and a 4 KiB page is an
ordinary shared memfd out of the pod's own memory, which a host with swap may
swap. A session states its region's page and its arena's kind when it attaches,
and both ends check the pair before a byte of guest memory exists; the two
arenas of one host are separate memfds and never draw on the same allotment.

## Bounded host pager

Each pager takes an arena, a dedicated scratch spill file, and explicit
resident, logical and dirty page budgets. The arena is a fixed-size sealed
memfd holding exactly the resident page slots. Logical admission bounds
metadata for every attached region, including pages never touched. Every
private page takes a spill slot before a write can resume, covering resident
and spilled dirty state together; the dirty budget is what sizes the spill
file, so that file's whole extent is the pager's fixed disk cap and a store
never has to ask anything else for room. Concurrent page I/O has its own limit,
each permit covering one read-ahead or spill buffer, with one additional permit
reserved so a checkpoint's read of a sealed set progresses while cold faults
saturate the rest.

`LossWindow` bounds the same private state in time. The pager dates the oldest
page each region holds that no landed checkpoint covers, and while that age
across the regions of one VM exceeds the window it admits no further dirty page
for that VM: the store waits exactly as a store past the dirty budget waits, and
a checkpoint of that VM is asked for through the same `Pressure.Checkpoint`.
Zero disables it. The window is a VM's rather than a region's, because the
checkpoint that ends it is, and the pager knows nothing about VMs: it asks
whoever owns the region, through `Pressure.Oldest`. The stamp moves with the
pages it is measured over — to a checkpoint at the seal, back to the region when
that checkpoint is abandoned, and across a handoff as the age
`Region.Handoff` reports and `Region.SetUnpublishedAge` applies — so neither a
failed publication nor a migration restarts the bound. Where no checkpoint of
that VM will ever be taken the wait ends as a budget stall does, as
`ErrWindowStalled`, which the region's owner answers by stopping that VM.

Allocation, sharing, copy-on-write, write protection, spill, eviction, mapping
commands and wire generations all use that instance's page, and region addresses
and lengths must be aligned to it. What an instance may run is what a volume can
be published in and what the transport maps, which are the same two sizes.

The host must provision a 2 MiB HugeTLB pool before starting VMs for the **PMEM**
arena; the **RAM** arena is an ordinary memfd charged to the pod's memory, so a
node provisions the two separately and the pool is no longer divided between
them. Either arena reserves virtual address space without reserving its entire
logical capacity, then allocates each resident slot with `fallocate` before
touching its mapping. Exhaustion — of the pool, or of the pod's memory — returns
an allocation error, with no fallback to another page. Eviction punches a whole
slot only after revoking every alias. HugeTLB pages are unswappable and shmem
pages are swappable, so the pager's own spill path remains responsible for
reclaim in both. The kernel must support missing, minor and write-protect faults
on userfaultfd for HugeTLB **and** for shmem, because a host runs a pager of each
kind. A pool is shared by all HugeTLB arenas on the host, so deployment admission
must budget their combined resident capacity.

Read-ahead and write-ahead select one page when a configuration leaves them
zero, which is no read-ahead and no write-ahead. A store page is the same unit
this pager runs — a volume is published in a page size of its own, and one whose
page is not this pager's is refused when it is attached — so a sealed pager page
is exactly one member of a checkpoint's part.
Read-ahead is one policy per pager, a power of two of that pager's pages capped
at 16 MiB; no region overrides it. Population walks metadata in at most 256 MiB
windows, independent of pager size. The Linux transport also bounds pending
faults, control requests and fault workers.

The production host does not leave any of them zero. It is the supervisor, in
`internal/host`, that chooses them, for each pager, from the share of the arena
the deployment gave that kind and the node it is on rather than from an
environment variable each. The two runs are stated in bytes and converted by
each instance, because a run is a buffer and a number of pages would mean
different amounts of memory in the two:

| bound | production value | why |
| --- | --- | --- |
| `ReadAheadPages` | 8 MiB of this pager's pages — four at 2 MiB, 2,048 at 4 KiB | a boot, a restore and a working set all walk memory forwards, so one fault serves what would otherwise be four, and the run lands in consecutive arena slots so one command installs it |
| `WriteAheadPages` | one page for RAM always; for PMEM the same 8 MiB, or one page where its dirty budget holds fewer than 64 such runs | RAM's unit of ownership is the whole point of its page, and a run that made a store's neighbours writable and privately dirty before the guest had used them would give back the sharing the small page buys. For PMEM it is the same run, charged to the dirty budget whether the guest uses it or not, so a small budget keeps one page |
| `ConcurrentIO` | four per processor, held between 16 and 256, and never more read-ahead runs than that pager's arena has room for | each permit can hold one read-ahead or spill buffer, so it is both the parallelism a node can use and a bound on the buffers it costs |
| `SettleWorkers` | the node's processors, capped at 64 | a settle compares resident pages and takes no I/O permit, so processors are what it can use, and it is time the upload waits for |
| `ConnectionConfig.FaultWorkers` | two per processor, held between 8 and 64 | a fault spends most of its life in a store read; the I/O budget is what bounds the reads |
| `ConnectionConfig.MaxVMAs` | half of `/proc/sys/vm/max_map_count`, disabled below 128 and capped at 2²⁰ | the pager's mappings are not the VMM's only ones, so half the kernel's limit is the budget and the rest is headroom; an unreadable limit disables the budget, exactly as a client without `/proc` does |

A starting host logs every one of them, per pager and beside that pager's page,
so what a node chose for each kind is on the record next to the arena and the
budgets the deployment set.

Attaching a region admits its metadata and verifies writer authority before
exposing it. The mapping must initially consist entirely of armed missing-fault
traps. A caller must not attach one writable volume to two regions: the pager
is the volume's only content mutator while attached. The backing interface is
satisfied by a volume directly; its load and locate calls serve the handle's
current view. Locate needs no I/O; a cold load is a range read inside the part
part that holds the page. The pager never writes to a volume: a region's dirty
pages reach storage only through the checkpoint that reads its sealed set.
Verify confirms that the handle still owns its VM and makes nothing durable.

A region may also attach through a backing put in front of its volume, which is
what `vmmachine.Config.Backings` names per volume and what a
[migration](migration.md) destination binds: loads ask the host that still holds
those pages first, and the volume answers everything else. The volume remains
the region's identity — its name, its size, its writer, its page identities and
every write — so a seal, a checkpoint and a fence are unchanged by the substitution.

## Sharing by identity

Resident pages are keyed by the [page identity](volumes.md#reads) the
volume reports for a page. Any region in the same pager whose current identity
matches a resident page maps that page, whatever region loaded it. A fork
inherits its parent's identities until it writes a page, exactly as a forked
process shares pages until copy-on-write; a first store allocates a private
page. A page is named by the checkpoint that published it, so independently
written identical bytes stay distinct however alike they are. Checkpoint
publication may still recognize an all-zero page for sparse representation;
that is separate from resident sharing.

A page the parent held dirty at a fork point has no published identity of its
own, and a child would have to read every one of them back through the seal.
Instead the fork point names the pages it sealed: the point takes a reference
of its own and publishes nothing under it, so the identity it gives each of
those pages belongs to every child of that point and to nothing else, ever,
and the pager enters the pages in the sharing index under it. A child on the
parent's host then maps them like any inherited page — the eager restore
population included, so a machine forked at a point maps every page its
parent has resident before its vCPUs run. The pages stay the parent's private
dirty state under the name: nothing is copied, nothing becomes durable, the
parent still copies on write and still owns the reservation that spills them,
and the name lasts exactly as long as the seal, whose bytes cannot change while
it does. Ending the seal takes the name back — a published checkpoint replaces
it with the identity the volume then reports, and a retired fork point hands the
page back to the guest as dirty state it may store into in place, which is why
the retire takes the page away from anything still sharing it. By then whatever
inherited it has copied, published or pulled the page and reads it from there.

A resident page is exactly one store page, so one identity covers the whole page
and there is no partial identity to rule out: a page is published whole or not
at all. A page with no published bytes of its own — what a migration destination
holds for the source's unpublished pages — has no identity and loads privately
until a checkpoint gives it one. A one-byte store still copies and charges the
whole page, which is why the page a pager runs is what a store costs its guest.
Storage compression does not compress mapped pages or change this accounting.

An explicit sparse zero has no arena slot and no page identity. Contiguous zero
ranges use Linux's shared zero page, so a large hole does not consume resident
slots. These sparse zero mappings and untouched missing-fault traps have no
resident page of their own; their anonymous page tables are the exception to the
arena's own backing. A zero range is one mapping command however many pages it
covers, so a sparse hole costs neither a command nor a mapping per page at
4 KiB. The first write replaces one page of it with a private page of the arena.
The zero mapping is not an identity: nothing is shared under it.

## Faults and read-ahead

Ordinary volume reads on the fault path take no round trip of their own.
Migration backings may request pages from the source peer, and cold volume
loads may read object storage. Ownership is confirmed by the supervisor's
periodic verification.

A read fault serves its whole aligned read-ahead run, one pager page by
default, when it can: pages already resident under the same page identity
are mapped without any read, and the rest are loaded with **one backing read for
the window** into consecutive arena slots and installed as one mapping
command, with their page tables pre-installed to avoid missing-page faults
while those mappings remain valid. Read-ahead uses only free slots and never
evicts; only the faulting page itself may. Eviction can later revoke a mapping
and require a refault. The read-ahead run is the host's, and every region of a
host uses it.

That one read asks for the pages of the window that need bytes and leaves out
the ones the region already holds — `vmmemory.SparseLoader`, which
`volume.Volume` is — rather than splitting itself at them. A window is a run of
a volume and what a run costs is the volume's to decide: the pages that are
wanted are still grouped by the part their members lie in, one ranged read each,
and a few pages a reader leaves out in the middle of a run are read through
exactly as the pages a later checkpoint rewrote there are. Splitting the read
instead put that decision in the pager and cost a request per stretch — a
512-page window with 64 pages scattered through it already resident was 65 loads
and 195 object reads where it is now one load and three. A backing that cannot
be asked for part of a range, which a migration destination's peer backing is,
is still read one stretch of wanted pages at a time.

Attach populates every page whose identity is already resident in the same
pager before the region is exposed, independently of the read-ahead size,
loading nothing. The Rust session serves mapping commands after descriptor
exchange and only then reports the region addresses to the VMM, so eager
mappings finish before VMM setup uses those addresses and before any vCPU runs.
This covers cold boot and snapshot load alike; a restore still returns paused.
Pages absent from the arena keep the ordinary fault path. An explicit later
population requires quiescent guest memory.

A region the pager refuses is the one failure whose two halves sit on opposite
sides of the socket. The VMM builds its sessions inside its own boot or load
request, so a refusal fails that request with the only thing the VMM has — the
backing descriptor that never came — while the reason is on the pager's side,
in a connect result the supervisor would otherwise never read. Both sides log
it where it happens, and the VMM's own half names which of the ways the
attachment failed it saw — the pager closed the session before attaching, it
closed after the descriptor and before the frame, it answered with no descriptor
at all, it attached more than the one a session takes, or the descriptors did not
fit this side's ancillary buffer — because each of those has a different end of
the connection to look at. A failed request to the VMM is reported together with
every session's result: the supervisor closes the process first, which ends the
listeners and every connect attempt, so those results are final and joining
them waits for nothing. A start is bounded at every step it could otherwise wait
out: the region listeners give the VMM two minutes to connect, and the wait for
its API socket has the same bound, because a process that neither binds it nor
exits is one that is never going to finish starting. A start that fails anywhere
after it takes its directory from the shared scratch goes through Close, so the
scratch is not left counting a process it will never see stop.

A store into a page this region holds no memory for reads its whole read-ahead
run in first, exactly as a read fault does, and then copies the one page the
guest stored into. It has to: on x86-64 KVM finishes a fault that had to wait
for the pager from a worker that asks for the page writable whatever the guest's
access was, so a guest merely reading memory it inherited reaches the pager as a
store, and a store that read its own page alone made a fork's first pass over
its memory one round trip per 4 KiB — a GCE fan-out on 2026-09-22 took 21,130
faults of which 20,016 were copy-on-writes. The run is installed shared: every
page of it but the faulting one is mapped read-only under the identity its
volume gives it, so the pages the guest goes on to read are served without a
fault and stay shared, and only the page it stored into becomes private. The
faulting page is not mapped read-only first — its copy is about to replace it —
so a store still costs no revocation. A migration destination's backing is read
a page at a time, because whether the source still holds a page is an answer
only a load can give and it gives it per page.

A store into fresh memory, a zero-mapped page or a hole in the volume the guest
has never touched, has no page to copy and nothing to fence, so nothing is
revoked: one mapping command replaces the zero mapping or the trap with a
private page, and a zero mapping goes on serving reads until the replacement
lands. The page is a free arena slot, which is punched and so already reads as
zeros: the Linux arena allocates it with `fallocate` instead of writing zeros
into it, the kernel clears it as the resolving `UFFDIO_CONTINUE` installs it,
and the volume is not read. A store into a shared page or a sealed page still
revokes the guest's alias before it copies.

Such a store also writes ahead. The fresh zero pages after it in its read-ahead
run, and before it where the run ends first, get private pages in the same
command, up to `Config.WriteAheadPages` pages in all — four on a production
host, one where the dirty budget is too small for runs of that size.
Consecutive slots continue the previous page's where those are free. Like
read-ahead, write-ahead takes only free arena slots and free dirty reservations
and never evicts or waits; only the faulting page may. The run is mapped
writable, so a guest writing fresh memory in order faults once per run rather
than once per page, and the pager never learns which pages of the run it stored
into: each holds a dirty reservation, spills under pressure and is written back
by a checkpoint like a stored page, zeros included. A guest whose stores would
just fit the dirty budget can therefore run out of it sooner by the write-ahead
pages it never used, and a migration source serves those pages as held.
`Stats.WriteAheadPages` counts the pages runs mapped beyond the faulting ones,
and `Stats.WriteAheadZeroPages` those whose written-back bytes were still all
zero, which is as near as the bytes can tell to never stored into, since a store
of zeros looks the same.

## What the sharing is worth

`Stats` counts what the pager has done: `IdentityHits` is every page ever mapped
to an already resident identity, `CopyOnWrites` every page a store took a
private copy of, and `UnchangedPages` every page a settle found to hold exactly
the bytes of the page it was copied from — a write fault the guest never stored
through, which no checkpoint publishes. None of them ever falls, so a host whose
guests have all diverged reads the same as one whose guests share everything.
The checkpoint's log line carries the settle's count beside its dirty set, as
`unchanged_pages`.

`Host.Sharing` is the gauge beside them, per region kind. `UniqueBytes` is the
host memory the arena holds, one resident page counted once however many regions
map it; `MappedBytes` is the sum over regions of the resident pages each maps,
so a page three regions map counts three times; `SavedBytes` is the difference,
which is the memory this host did not have to find. Every alias counts in
`MappedBytes`, including two regions of one VM and a checkpoint's copy of a page
the guest still shares with it. A page no region maps at all is still memory the
arena holds — the page a store copied away from and left behind, waiting to be
compared with that copy or to be inherited by the next region that names its
identity — so it counts in `UniqueBytes` and in no mapping, under the kind of
the region that created it. It measures resident sharing only: a fork
inherits every one of its parent's page identities, and the ones neither has
faulted in are shared in the store and on the wire without costing this host a
byte, so none of them are here.

`RegionStats` says the same for one region: `ResidentPages`, the pages holding
host memory; `PrivatePages`, the pages whose bytes are this region's own and not
yet its volume's, resident, spilled or held by a checkpoint; and `SharedPages`,
the resident ones at least one other region of this pager also maps. The three
are also reported in bytes, because page counts of different geometries cannot
be added and bytes can.

A region carries its kind, RAM or PMEM, which `Attach` is given. Nothing about a
fault, a seal or a page depends on it inside one pager; what it decides is which
of a host's two pagers the region attaches to, and it is what lets a host say
which of its guests' memory and its guests' disks the sharing is in. It is
stated by whoever attaches the region, never inferred from a volume's name — a
host deciding which pager a VM's regions would be admitted against, before there
is a machine to ask, says so itself.

The host adds those per-region numbers up per VM, because the pager has regions
and no idea of a VM: `Host.PrivateBytes` in `internal/host` is one VM's private
bytes across every region it maps, which `/status`, `/metrics` and the VM
listing report; the pager it reads each region through is the pager of that
region's kind. The Prometheus gauges are `sproutfs_pager_unique_resident_bytes`,
`sproutfs_pager_mapped_resident_bytes` and `sproutfs_pager_shared_saved_bytes`,
each carrying `kind="ram"` or `kind="pmem"`, and every other pager series
carries the same label, because the two run their own pages and a sum of their
page counts would mean nothing. `sproutfs_pager_arena_bytes` is the one total,
and it is bytes for that reason.

Faults serialize only within one read-ahead run; different runs and different
volumes proceed concurrently. A short host lock accounts for capacity, binding
pointers and the shared index, and no backing read, spill or mapping
acknowledgement holds it. Each resident page has its own transition lock for
mapping changes and reclaim, and read-ahead skips a page whose lock is busy
rather than waiting. A region's per-page state — whether a page is mapped, what
it is dirty under, which checkpoint holds it — lives under that region's binding
map lock, because the one pair of holders that do not exclude each other is a
reclaim revoking its victim's pages under a page's lock and a seal reading
those pages under the region.

Reclaim uses fault and read-ahead recency. Accesses through already present
page tables do not refresh it, so eagerly mapped pages can be reclaimed while
hot. Evicting a shared page necessarily coordinates that page's aliases across
processes. An ambiguous mapping acknowledgement pins its possibly live slots
until the process exits.

## Seal, checkpoint and verification

A checkpoint is how a region's dirty pages become durable, and the only way.
There is no flush: the pager never writes to a volume, and a guest's
virtio-pmem flush makes nothing durable and returns success at the device.

A seal also takes the region's loss window: the age of the oldest page it is
freezing becomes the sealed set's, and the region's own starts again at its next
store. Retiring that set published drops it, because the store holds those bytes
then; abandoning it — a publication that did not land, or an unseal — hands it
back, and the region keeps the older of that and whatever it has written since.
A window that restarted at every failed publication would bound nothing, since
the host that cannot publish is exactly the host whose publications keep failing.

Sealing a region revokes write access to its dirty pages, write-protecting the
pages the guest already has, records those pages as the sealed set, and
returns; nothing is copied and no byte crosses the network. Write protection is
applied in place: one range write-protect per run of consecutive dirty pages,
whatever memory those pages hold and however many of the client's mappings that
run spans. No mapping is replaced and no page table is installed or dropped, so
the guest keeps reading the same pages through the same page tables and only
its next store traps. The seal portion of a capture's pause depends on the runs
and per-page bookkeeping of the dirty set. Resuming waits for nothing beyond it.

The seal waits for page-table work and never for bytes. A fault holds the region
shared for its planning, its metadata and its page-table commands, and gives it
up across the two things that can be slow: the backing read — a volume load, or
a migration source that keeps answering BUSY — and the reclaim a new page may
cost, which revokes a victim's mappings and writes its bytes to the spill file.
A seal taken meanwhile runs straight through both. The window's own stripe still
owns that window for the whole fault, and the stripe is taken before the region
so that the two orders are one order. What the region's exclusive holders are,
then, is a seal, a retire, an unseal, a handoff and a detach; only the detach
waits for faults, and it waits on a separate lock a fault holds from end to end.

A reclaim can therefore be holding a page the seal is about to take. The seal
never waits for it: a reclaim is the only thing that can hold a page of the
region being sealed, and it ends with the page nonresident and its bytes in that
page's own dirty reservation, so the seal joins the checkpoint's copy of the
page to that resident page without its lock and lets the reservation carry the
bytes. Which is why whether a reservation's slot holds its page's bytes is the
slot's state rather than the binding's: the seal hands the reservation to the
copy while the reclaim is still writing to it, and only the slot is named by
both. The seal revokes such a page rather than write-protecting it, which is
what the reclaim is doing anyway and is stronger. The reclaim reads the page's
aliases and then the reservations they name, and the seal joins the copy to the
page before it hands that copy the reservation, so an alias set that has grown
since those reservations were read is walked again: the alias the reservation
moved to is in it, and the page's bytes reach a reservation whichever order the
two take.

Retiring a checkpoint walks a set as large as the capture's, so it walks it the
way the seal takes it: in bounded batches, with the region given back between
them, so a fault waits for one batch rather than for the walk. Each batch's
volume metadata — the identity the volume now gives each page, which decides
whether its page joins the sharing index — is one lookup per read-ahead window,
taken before the region and before any page. One retire or unseal runs at a
time, and a page already retired is skipped, so repeating a failed one finishes
what it left.

The sealed pages are detached copies: they alias the pages the guest had, and
they own the dirty reservations those pages were admitted under. A store into a
sealed page takes the write-protect fault and copies on write, so the guest gets
a fresh private page with the current contents while the seal keeps the
original; the sealed bytes never change. The page stays the checkpoint's, memory
and all, until that fresh page is bound, so a store that fails on the way —
an arena that cannot fill the slot, a reclaim it cannot have — leaves the page
where the seal left it and the guest simply faults again. A seal ended while
that reclaim runs hands the page its own reservation or clean state instead, and
the store decides what it needs from the top. The dirty budget counts sealed pages
together with live private pages, and a guest that dirties faster than its
checkpoint uploads waits at that budget rather than failing or overrunning it.

That budget is the host's, so the wait is too: a store waits for whichever
checkpoint lands next anywhere on the host, not only for its own region's. A
migration destination's read of a page the source still holds waits the same
way: the load takes that page as this region's dirty state, so the fault's
window extents are what decide it needs a reservation, and the fault gives up
the region, its page and its I/O permit to take one through the waiting path
before it loads anything. Read-ahead around it takes only reservations that are
free, and leaves for a later fault the pages it finds none for. With
none in flight the pager asks for one instead of refusing the store — `Pressure`
is the pair of callbacks the region's owner answers with, and the host offers
the regions holding the largest dirty sets first, since those release the most.
A fault that crosses three quarters of the budget asks before any store has to
wait at all, so the checkpoint is already being taken when the last quarter runs
out. The crossing is measured as the fault returns, holding no region, page or
reservation, so every path that admits a page counts against it: the
reservation a store waits for, the one a peer-served load takes, and the run of
them write-ahead takes without waiting, which can cross the mark by itself.
A sealed region can take no further checkpoint, so it is offered none; it
answers the wait only when ending its checkpoint relieves the budget, which a
publication's does — it gives its reservations back, and where it holds none it
gives the region the right to take the checkpoint that will. A fork point's
hold does neither while the children it named are still reading it, so a fork
held here leaves every other region's pressure to be answered by a checkpoint of
that region's own.
Only a budget no region can be checkpointed for fails a store, with
`ErrDirtyStalled` rather than `ErrCapacity`, and the same pressure reports the
region to its owner so that a VM is stopped deliberately, publishing what it
holds. A failed fault kills the VMM and loses those bytes with no reason
recorded, which is what the whole path exists to avoid; for the same reason a
fault carries no deadline of its own, since the round trips inside it are each
bounded by the command timeout and the one thing that makes a fault long is this
deliberate stall. `Stats.DirtyWaits`, `CheckpointRequests` and `DirtyStalls`
count the three outcomes; a host that stalls has a budget or an interval too
small for its guests.

The publication reads the sealed set straight out of those pages.
`Region.Checkpoint` is that set as `volume.DirtySource`: the pager pages it
holds, one whole pager page at a time under an I/O permit and that page's lock,
and the retire that ends it. A pager page is a store page, so each sealed page
is written as one member of the checkpoint's parts, at the part and offset the
index records. Nothing copies those bytes into the volume package on the way;
the guest runs throughout. A read of a set that has already ended fails —
`ErrNotSealed`, or why a detached region discarded it — rather than answering
out of pages that are the guest's own again.

A write fault is not always a store, so a sealed page is not always dirty. On
x86-64 KVM finishes a guest fault that has to wait for the pager from a worker
thread which always asks for the page writable, so a cold **read** reaches the
pager as a write fault; on aarch64 the guest kernel's cache maintenance on a
page it executes for the first time is reported by the architecture as a write.
The pager cannot tell those faults from real ones while they wait — the worker
will not finish until the page is writable — so it copies, and tells afterwards.

**A sealed page whose bytes equal the page it was copied from is not dirty.**
The copy remembers its origin: when a store is served by copying away from a
resident page that holds a published page identity, the binding keeps a pointer
to that resident page. Not its identity — eight bytes per binding, and an origin
that has been evicted is simply no longer an origin. A page copied from a
checkpoint's held copy, from the name a fork point lent a private page, from
another host's unpublished page or made from zeros has none, and a store into a
page this region holds no memory for reads that page in first, under the
identity its volume gives it, so that what it copies away from is a page the
settle can compare it with and every region inheriting that identity maps rather
than reads. The page a store copied from stays in the arena when the binding
leaves it: nothing pins it, and the next reclaim short of a slot takes it like
any other clean page.

`RegionCheckpoint.Settle` is the comparison, and `volume.DirtySource.Settle` is
what the publication calls once, behind the pause and before it enumerates the
pages, with the guest already running. A published identity's bytes are
immutable, so comparing the sealed page with its origin under both pages' locks
is a `bytes.Equal` and nothing else: no hash, which would make a wrong answer
possible and would put work on the fault path, and no read of the store, which
would double a checkpoint's I/O for the pages that did change. A sealed page the
pager has spilled is left alone for the same reason. An unchanged page leaves
the checkpoint's set, so `DirtyPages` does not list it and it costs the store
nothing; if the guest still shares the checkpoint's copy, the binding takes the
origin as its resident page and becomes clean, **the guest's mapping of the copy
is revoked**, the private page is released and the dirty reservation returned. A
store that lands first copies away from the checkpoint as it does today, and
then only the checkpoint's copy is released. A checkpoint a settle leaves
holding nothing holds no unpublished write either, so the loss window it took at
the seal ends there.

The mapping is taken away rather than swapped underneath the guest, and that is
deliberate. A settle runs with the guest running and holds neither the region
nor the window that serializes a page's mappings against one another, so the one
replacement it may issue is the one that installs no page table and wakes
nothing. Installing the origin in its place — one command, no fence, the bytes
identical and the page write-protected either way — is what it used to do, and
it is a large part of an open defect: a fan-out of two children at a 4 KiB RAM
page panics a child's guest kernel on a list entry the guest itself had removed.
Revoking instead took that from about one run in five to about one in thirty.
**It did not remove it, and this is not a fix for it** — see the entry in
[open-work.md](open-work.md), which carries the rates and what has been ruled
out. What revoking costs is one fault per page a settle re-shares, which is the
fault the guest was going to take for that page's next write in any case — and
on a host whose kernel answers a cold read as a write fault, that fault copies
the page again and the next settle undoes it again.

The settle is parallel, and then it applies what it decided. Each page is
compared alone, under that page's lock and its origin's and nothing wider — so a
settle hands its pages to `Config.SettleWorkers` workers, the host's processors
by default, and the regions of one VM settle at the same time as each other. The
comparison mutates nothing; what it decided is applied afterwards, in page order
and in bounded batches under the region, as a retire is. That is what lets the
revocations go as **one command per run of consecutive pages**: a settle at
4 KiB re-shares thousands of pages at every checkpoint, and a round trip each,
serialized on the mapping lock, is a stall the guest feels. The region is given
back between batches, so a fault waits for one batch rather than for the walk.
What bounds it is memory bandwidth and not the pager's I/O permits, which it
does not take: it reads no disk and no store. The workers share nothing but the
counter of unchanged pages and the set the checkpoint will list, both under the
checkpoint's own mutex, so the result does not depend on the order they finish
in — which is what lets the simulation run the same code. An arena that can
compare two of its own slots does so in place; every other arena is read into
two buffers per worker.

A fork point is not settled. It publishes nothing and its pause is what a child
waits for; its children inherit an unchanged page as an unpublished one, which
is correct and no worse than not settling at all. The next checkpoint of each
settles it. It is the pager's rule, so it holds for RAM and PMEM alike.

What it does not do is stop the copy. Between the fault and the next checkpoint
the host holds the page twice. Preventing that needs a host kernel that passes
the guest's access through, or KVM userfault; both are recorded in
[open-work.md](open-work.md).

Retiring the seal is the publication's. A sealed set whose checkpoint was
selected retires as published: a page the guest has not stored into since the
seal becomes ordinary clean state, its resident page joining the sharing index under the
identity the volume now reports and staying mapped to the guest, while a page
the guest copied away from keeps only the sealed copy, which is released. Either
way the dirty reservation goes back, and the page and the sealed copy of it are
retired together under the resident page they share. A sealed set whose checkpoint never
landed is abandoned instead, which is also what `Region.Unseal` does: pages the
guest still shares take their reservations back and are dirty again, so nothing
is lost, and their mappings are revoked so the next store faults and maps them
writable again rather than trapping on the protection the seal left. The next
checkpoint takes them.

One seal is outstanding per region: sealing a sealed region reports `ErrSealed`,
and so does handing one off, because a publication is reading its pages under a
volume handle a handoff would give away. A seal that fails partway captures
nothing. Its half-protected pages go back to the guest as ordinary dirty state,
so sealing again takes the whole of whatever is dirty then. Pages count as
sealed as the seal takes them, so a capture that never completes still reports
the work its pause paid for.

The spill file is scratch storage and is never an acknowledged crash-recovery
image. A starting process truncates it: a restart is a host loss, and nothing
that was in it meant anything afterwards. It is written but not synced, for the
same reason. Returning a released slot's blocks to the filesystem is a courtesy
rather than accounting, so a failed or unsupported hole punch costs nothing.

Verification checks writer authority even when cached accesses never fault. The
Linux connection runs it per volume on a timer with bounded deadlines; a
failure closes the control channel and requires the supervisor to terminate the
process before mappings are detached. It observes authority at the call and is
not an expiring lease: the storage guarantee is that a fenced writer cannot
obtain another conditional write.

## Ownership

The Go pager owns:

- The shared memfd and its slots, page identities and alias references.
- Fault resolution, copy-on-write decisions and private page allocation.
- Spill, reload, and the decision to evict a page.
- Punching the arena and reusing a slot, only after accounting for every alias.

The Rust library owns:

- The stable host virtual address ranges identified as PMEM or RAM.
- UFFD creation, registration and descriptor transfer to the Go pager.
- Applying mapping changes in its own process and acknowledging them.
- Keeping mappings, descriptors and generation bookkeeping alive for the
  session.

PMEM and RAM share one mapping implementation and one durability contract:
both become durable only through a checkpoint, and a guest's PMEM flush makes
nothing durable. A local spill is not durability either.

## Rust interface and lifetimes

A session maps exactly one volume as one contiguous range. A VM's RAM is one
session, and so is each of its PMEM devices, each over its own socket: a VM has
one RAM volume, `ram0`. The library knows no guest addresses at all; placing a
volume's bytes in the guest's address space is the VMM's work, described under
[the Firecracker build](#firecracker-build-and-process-lifecycle).

`Session::connect` reserves the region's range, creates the UFFD and exchanges
descriptors. The range is reserved before the page is known, so it is reserved
at the largest page this transport maps, which is aligned for both; the
attachment then states this region's page and what its arena is made of, and the
client checks that it maps that page, that the descriptor really is that memory
— an explicit 2 MiB HugeTLB file, or an ordinary shared memfd of 4 KiB pages —
and that the region's length is a nonzero multiple of it. A mismatch ends the
session before an address is exposed to the embedder. `Session::page_size`
reports what was agreed. `Session::region` returns an address descriptor, not a Rust borrow; the
session owns its lifetime. `Session::run` services commands on a dedicated
thread, and the Go pager reads the UFFD itself rather than having Rust relay
fault messages.

`Session::control` returns a cloneable device-side handle whose `start_seal`
asks the host for this region's seal while the session thread keeps serving
mapping commands. Sessions seal independently, with at most one request pending
per session, so a coordinated capture issues them all and then waits.

The host's deadline is the only one that decides a seal. It bounds the command
and answers a seal it could not finish with a failure, which is a checkpoint
that did not happen: every page the seal took goes back to the guest as ordinary
dirty state, the region stays usable, and the next checkpoint takes the whole
dirty set. The VMM answers its capture request with that failure and keeps
running; its caller unseals and resumes. The client's own wait is far longer
than the host's — five minutes against the host's command timeout — so it is
only the backstop for a host that has stopped answering at all; a timer there
that fired first would close the control session and kill a guest whose
checkpoint was merely slow. A wait that does expire closes the session.

All raw-pointer users must stop before the session is dropped, and ordinary
Rust references must not be held across mapping changes. External device and
kernel pins require coordination by the embedding process; the library provides
no long-lived pin interface. On a control or mapping error the session becomes
terminal and retains its UFFD and mappings until dropped: closing the UFFD
while continuing to run would let anonymous fault traps revert to ordinary
zero-filled memory. Both the fixture and the production supervisor terminate
the process on unexpected control loss, and the supervisor holds every
connection and descriptor until `waitpid` confirms exit. For an orderly
shutdown, stop all memory users and unregister the KVM slots first, then send
STOP and let the process release its session.

## Mapping replacement

Never expose a new mapping and register or protect it afterward: another thread
could access it in that interval and bypass demand loading or copy-on-write.
Instead the library builds the mapping away from the live address, registers it
with UFFD and write-protects it when it is shared immutable backing, replaces
the live range with `mremap(MREMAP_FIXED | MREMAP_MAYMOVE)`, and acknowledges
only after that syscall completes.

Nonresident ranges are anonymous readable and writable mappings registered for
missing and write-protect faults, with no populated pages. They are not
`PROT_NONE` ranges, which would raise ordinary protection faults instead.

Resident ranges are `MAP_SHARED` views of the arena registered for missing,
minor and write-protect faults; a shared immutable page is armed for
synchronous write protection before it becomes accessible. The pager installs
resident backing with `UFFDIO_CONTINUE`, including its write-protect mode for
shared data, rather than copying bytes into a private anonymous destination,
which is what makes the page physically shared. It issues that over whole
mapped ranges rather than only faulting pages, so read-ahead and populated
pages get their page tables before any access; present pages are skipped. A
fault is resolved over its whole pager page, every host page of it. A range
`UFFDIO_CONTINUE` that meets a present host page reports only that one, so the
range is then finished one host page at a time rather than trusted. One
mapping command covers a run of consecutive pages whose arena slots are also
consecutive, which keeps the process's mapping count near the number of runs
rather than the number of pages.

Taking write access away is not a replacement at all. The pager issues one
`UFFDIO_WRITEPROTECT` over the whole run on the live addresses, which the kernel
applies to every registered mapping the range covers. That is what a seal does,
and it is why a seal costs runs rather than pages: nothing is built away from
the live address, nothing is remapped, and no page table is rebuilt.

## Kernel and VMM constraints

The library requires `UFFD_FEATURE_EVENT_REMAP`, since Linux drops the UFFD
context on remap without it; with it, remap completion waits for the event to
be consumed. **The Go UFFD reader must keep draining remap events while another
goroutine waits for a mapping acknowledgement.** Also required, and required
together: `UFFD_FEATURE_PAGEFAULT_FLAG_WP`, missing and minor faults on HugeTLB
(`UFFD_FEATURE_MISSING_HUGETLBFS`, `UFFD_FEATURE_MINOR_HUGETLBFS`) and on shmem
(`UFFD_FEATURE_MISSING_SHMEM`, `UFFD_FEATURE_MINOR_SHMEM`), and
`UFFD_FEATURE_WP_HUGETLBFS_SHMEM`, which is write protection for both. The set
is negotiated once, when the UFFD is created and before the attachment states
which of the two geometries this session runs, because a host runs a pager of
each kind and a kernel that serves only one cannot run this build at all. The
negotiation fails clearly: a second descriptor asks the kernel what it does
support, and the error names the features that are missing rather than saying
that some feature is. Both that negotiation and the per-range ioctl mask are
checked. Anonymous write protection appeared in Linux 5.7, shmem minor faults in
5.14 and shmem write protection in 5.19; those are introduction versions, not a
qualification.

A seal additionally requires `UFFDIO_WRITEPROTECT` to apply across every
registered mapping its range covers, since the pages of a run of dirty pages
are separate mappings whenever their arena slots are not consecutive, which for
a real guest is usually. Linux walks the VMAs of the range; the suite qualifies
that directly, at the syscall fixture and through the real client. A kernel that
demanded one mapping per call would fail the whole ioctl rather than protect
part of a run, which fails the seal and undoes it; the seal would then have to
split its runs at mapping boundaries it does not currently track.

Faults must be kernel-visible: `UFFD_USER_MODE_ONLY` does not cover KVM's own
accesses. Deployment must therefore grant kernel-fault UFFD through the
permitted syscall route or `/dev/userfaultfd` and allow it under the actual
seccomp policy. The client's mapping-count budget is opt-in: zero disables it
and reads nothing from `/proc`, which is what a jailed process without `/proc`
needs to attach at all; a nonzero limit is an admission bound that
`/proc/self/maps` only makes less conservative.

Eviction cannot rely on `madvise`: dropping anonymous contents leaves shared
backing resident elsewhere, and punching the backing makes future accesses read
zeroes. The pager therefore revokes every alias and waits for acknowledgements
before it releases a slot.

Guest PMEM is byte-addressable memory whose stores become durable only through
the host's checkpoint, so neither store completion nor a guest flush is
durability; guest DAX bypasses the guest page cache but changes nothing about
the host backing. Firecracker requires 2 MiB PMEM alignment. Its save order is
fixed: pause vCPUs, save devices before KVM state because device completion can
inject interrupts, then capture memory, which is why a seal must survive device
accesses after the vCPUs pause: those accesses copy on write like any other
store, and the sealed bytes stay. The adapter refuses any `huge_pages` setting
for managed RAM — the pager owns that memory and states its page when the
session attaches, so a setting there would be the VM claiming something about
backing it does not choose — and rejects ballooning, memory hotplug, vhost-user
and asynchronous block I/O with managed RAM, and a coordinated capture requires managed PMEM for
every disk and refuses ordinary block devices. A managed virtio-pmem flush
completes with success at the device and asks the host for nothing. KVM slots
are unregistered before mappings drop. Ordinary CPU and the tested KVM accesses
are covered by Linux mapping invalidation, not by a Rust lock around each load.
Upstream's own UFFD restore copies pages into each VM's anonymous memory and
cannot share a page between VMs; this integration replaces it.

## Control protocol, version 7

Version 6 clients are rejected because the page is no longer one number both
ends know: ATTACH carries the page this session's region runs and the kind of
memory its arena is made of, and a 2 MiB page number read as a 4 KiB one names
another page. Version 6 had itself rejected version 5 for two removals at once —
a session carries one region, so the `region` field is gone from the frame and
the frame is 56 bytes, and HELLO carries no page size — and version 4 before it
for the removal of the FLUSH request, whose frame kind SEAL took. The
supervisor, Rust adapter and Firecracker integration must be deployed together.

A Unix stream carries fixed 56-byte frames of seven little-endian `u64` fields:

```
kind, id, offset, length, backing, generation, flags
```

Frames must be read and written completely; stream boundaries are not message
boundaries. `SCM_RIGHTS` carries exactly one descriptor on HELLO and ATTACH.
The receiver closes unexpected descriptors and rejects truncated ancillary
data. A pager that closes the socket rather than sending ATTACH — which is what
refusing the region looks like on the wire, a full logical-page cap being the
usual reason — is read as an orderly close and reported as the pager closing
before attaching, not as a malformed descriptor message: the client's account
of why it could not start has to name the end the answer never came from.

| Kind | Value | Fields |
| --- | --- | --- |
| HELLO | 1 | `id` protocol version, every other field zero; carries UFFD |
| REGION | 2 | `offset=host address`, `length=bytes`, `flags=kind` (1 PMEM, 2 RAM) |
| ATTACH | 3 | `id` protocol version, `offset=this region's page size`, `length=arena size`, `backing=arena kind` (1 explicit 2 MiB HugeTLB, 2 ordinary shared memfd), `flags=mapping-count budget`; carries the arena memfd |
| MAP | 4 | Command ID, region-relative offset and length, arena offset in `backing`, next generation; `flags=1` immutable and shared or `0` private and writable |
| REVOKE | 5 | Command ID, region-relative offset and length, next generation; installs nonresident fault traps |
| ACK | 6 | Echoes command ID and generation; `flags=0` success or a positive Linux errno |
| STOP | 7 | Command ID; the embedder must have stopped all memory users |
| SEAL | 8 | Client request ID; write-protects this session's region's dirty set and records it, completing in page-table time. There is no durability request in this protocol |
| RESULT | 9 | Echoes the request ID; `flags=0` success or a positive Linux errno |
| MAP_BATCH | 10 | `length=run count` (1–1024), followed by that many MAP or MAP_ZERO frames; one ACK for the batch |
| READY | 11 | Ends the mandatory attach population; acknowledged before `Session::connect` returns |
| MAP_ZERO | 12 | Explicit sparse zero range, `backing=0`, `flags=1`; installs populated anonymous shared-zero page tables under write protection |

The attach READY handshake and batched mappings are mandatory. All batch ranges
are validated before any mutation and must be ordered and disjoint. The Rust
client populates sparse zero page tables before UFFD registration; the pager
installs arena page tables with ranged `UFFDIO_CONTINUE`, retrying the
transient `EAGAIN` a concurrent mapping change causes.

Client request IDs are separate from mapping command IDs and increase within
each session, so independent regions' requests can arrive out of global order.
The control reader dispatches requests without waiting for their work, because
a seal can itself need revoke acknowledgements. Writes serialize complete
commands, including every frame of a batch.

All ranges and backing offsets must be aligned to the page the attachment stated
and bounded, and every command covers whole pages of it. Generations start at zero and advance by exactly one
for every affected page; a command spanning several pages requires all of them
at the previous generation. Command IDs increase across accepted commands. An
exact retry of the immediately preceding successful single-range command repeats
only the acknowledgement; batches are never retried after an uncertain
acknowledgement, and the connection becomes terminal. Stale generations, invalid
ranges, overflow and unknown operations are rejected before any mutation.

A syscall failure after mutation begins is terminal rather than a rejected
command: a failed `mremap` can leave its destination unmapped. A pager that
does not receive the matching successful acknowledgement must not infer that
old backing can be freed. The protocol assumes a trusted local pager and VMM
process; it is not a guest-accessible transport.

A rejected command is therefore the one failure that is known to have changed
nothing, and the pager treats it as one: a failed operation, not a failed
session. The refusal a guest meets in practice is the mapping budget above —
the client admits a command against it before it touches anything and answers
`ENOSPC` as an ordinary acknowledgement. The pages that command would have
mapped are not recorded as mapped, which is the opposite of what every
ambiguous failure requires: a command whose acknowledgement never arrives may
have been applied, so its pages stay recorded as mapped, because a revocation
skips an unmapped binding and would release a page the guest still reads
through. The fault fails, the region goes on serving, and the worker queues that
fault again once the pager has made some progress — what frees the budget is
revocation, which is other work of this pager's. The guest waits there as it
waits for the dirty budget, and the VM stays alive to be checkpointed or
migrated off the host.

What makes that recoverable is what the budget admits. Only the replacements
that install a mapping are charged against it. A revocation installs none — the
range it replaces becomes the trap mapping the region was attached as, which
merges with the traps around it — so it can only lower the count and is admitted
whatever the budget holds, and so is a batch of them however large. Charging a
revocation like a mapping would refuse it exactly when it is needed: a refusal
leaves less of the limit than one command reserves, so the pager sent to revoke
would have nothing left to try and the deferred fault would wait for a command
that could never be admitted. What a revocation costs transiently is covered by
the kernel headroom the limit leaves, which is why a limit above half of
`/proc/sys/vm/max_map_count` is refused at attachment. A batch split across
several commands is the exception:
once one of them has landed, the pager knows the runs it sent but not the frames
they became, so a refusal after that is as ambiguous as a lost acknowledgement
and terminal like one. `Stats.RefusedMappings` counts the deferred faults; a
host that refuses has given its client a budget too small for the mappings its
guest's access pattern fragments into.

## Eviction ordering

1. Hold the page transition so no new alias can be installed.
2. Revoke every mapped alias and wait for each matching acknowledgement.
3. Read the now-stable contents of private backing and write the spill file.
4. Punch the arena range and make its slot reusable.
5. Let queued faults reload and remap the current page identity.

A page whose backing is reconstructible writes no spill data. A spill failure
preserves the resident slot even though its aliases were revoked, so a later
fault can map it again. Private pages are conservatively spilled again after
another writable residency period. No slot is reused merely because a revoke
was sent.

Step 2 is the one that can meet another machine's death. A region whose memory
session has stopped answering cannot take a mapping away, so a page it is
reachable from is never this host's to reuse — the revocation that discovers
that makes that region terminal, and the page stays mapped there until the
region is closed. But the eviction that discovered it belongs to whichever
machine happened to need a page, and children of one fork share every page
they inherited: answering that machine with the dead one's failure would end it
too, and then the next one sharing a page with it. It takes another victim
instead, and the terminal region excludes its pages from every later pass, so
an arena made entirely of them reports capacity exhaustion rather than spreading
one guest's death across the host.

## Capture, fork and restore

A capture is the checkpoint's pause. Preparing pauses the vCPUs, drains device
completions, saves device and register state and seals every memory region,
returning each region's sealed set by the name of the volume it maps. Resuming
restarts the vCPUs with the regions still sealed. The publication then writes
those sealed pages with the captured VMM state and retires the seals when it
lands. The guest's pause is the state capture and the seal, and nothing else: no
byte crosses the network inside it.

The pause happens under the VM's publication lock, so an explicit capture and
the host's interval checkpoint serialize there rather than racing to seal the
same regions. A phase that fails before the publication starts releases the VM:
every region is unsealed and the guest resumes.

None of these operations is the caller's to cancel. Pause, snapshot create,
resume and release run on a context of `vmmachine`'s own, bounded by its own
timeout; the caller's context bounds the wait for the process lock and nothing
past it. A control request abandoned halfway leaves the VMM's state unknown, and
the only answer to unknown is killing the process, which loses every guest write
since the last checkpoint that landed — so an HTTP client that disconnects, a
drain whose deadline passed or a migration that gave up must not reach the VMM
as a cancelled request, and the release that is the way back from a failed
capture above all runs on a live one. A request the VMM answers and refuses is
different in kind: the VMM is running and its vCPUs are where they were, so a
refused pause or a refused seal fails the checkpoint and leaves the guest alone.

The state file the capture stages is never made durable. It is read back and
removed before the guest resumes, a host that restarts wipes the directory it
lives in, and its bytes become authority only once the checkpoint carrying them
is published; the VMM is told not to sync it either, so nothing in the pause
waits on a disk for bytes nothing will look for. Once the publication owns the
sealed sets nothing else unseals them — it retires each when it lands, and hands
their pages back to the guest when it does not.

A fork publishes no checkpoint of the parent. `host.Seal` takes the same
pause — stop, save state, seal, resume — and returns a `volume.ForkPoint`:
the checkpoint the parent's control record already selects, pinned there, plus
the pages no checkpoint holds, which are exactly the pages the seal froze. Any
number of children start from one point, each one hold on it, so a fan-out costs
the parent one pause; the parent stays sealed, and is not checkpointed, until
the last hold retires and hands every page back to its guest. On the parent's
host a child reads the sealed pages through the point and shares them in the
same pager; on another host the child's pager pulls those pages from the
parent's page server as a migration destination does. The child's first
checkpoint publishes them as its own, and until that checkpoint lands, opening
the child anywhere reports `volume.ErrForkPending`.

A host that never held the parent rebuilds the point from the pinned checkpoint
alone, which is what a fork of a template is, and a host restores a VM it never
ran by reading the VMM state of the published checkpoint. Restore replays those
state bytes with exact managed PMEM overrides and never loads a full RAM image
file.

Every VM a host runs is checkpointed on `host.Config.CheckpointInterval`,
sixty seconds by default, each wait jittered by up to an eighth either side so
VMs do not checkpoint in lockstep, and measured from the completion of the previous
publication. That interval is the only thing that makes a running guest durable:
guest PMEM stores, guest RAM stores, live registers and local scratch spill are
all volatile until a checkpoint includes them, and losing the host rewinds the
VM to its last checkpoint.

## Live migration

The pager's half of [live migration](migration.md) is two things: serving pages
to the destination, and giving the volume up. It is post-copy only; nothing is
uploaded inside the pause.

`Region.ReadResident` copies one page for a peer, reports plainly when this host
does not hold it, which sends the destination to the volume instead, and reports
separately whether the page it served is this region's own state rather than the
volume's. It never loads, since a page server that loaded would turn a
destination's fault into a source-side volume read. Held means host memory or
private state of this host's own; unpublished means the page is dirty here, so
no checkpoint has it. `Resident` lists the pages this host holds and
`Region.Unpublished` the subset no checkpoint has; both are snapshots, and a page
reclaimed before a stream asks for it is simply reported absent.

On the destination's side of the same protocol, a load is not an install. A
backing that reports pages as unpublished — `vmmemory.UnpublishedLoader` — fills
a buffer, and the pager may still not keep the page: a read-ahead page on a full
dirty budget is dropped rather than failing the fault that carried it, and the
bytes are then nowhere, because the volume's bytes for that page predate the
guest's write. A backing that also implements `UnpublishedInstaller` is told,
after the load's pages have been bound, which of them this region now holds as
its own dirty state. Only those may be struck off the set the destination still
owes the source; a page the pager dropped is asked for again, and a read that
cannot get it fails rather than answering from the volume.

`Region.Handoff` gives the volume up while keeping the pages. It belongs after
the guest is stopped: the control record is about to be released so another host
can take it, so verification stops checking authority this host no longer has,
and a seal, a population or any fault reports `ErrHandedOff` instead — the guest
is stopped, and a fault would mean it is not. Serving continues until `Detach`
releases the pages. A sealed region is not handed off: a publication is reading
its pages under the handle the handoff would give away, so it reports
`ErrSealed` and keeps its volume.

The supervisor exposes this per VM. `Process.Regions` names every region by the
volume it maps — `ram0` and one per PMEM device id, which are the names the
destination opens the same volumes under. `Process.Stop` is the stop
phase: it pauses the vCPUs, drains device completions and returns the VMM state
with the VM left paused. It seals nothing and waits for nothing, because the
pages it leaves behind are exactly what the destination fetches. It is distinct
from `Prepare`, which seals for a capture the same VM resumes from; a migration
abandoned after `Stop` can still be released, which resumes the guest.

## Firecracker build and process lifecycle

The integration is a commit on the `sproutfs` branch of the
[fork](https://github.com/semistrict/firecracker.git), and the gitlink pins it:

```sh
git submodule update --init third_party/firecracker
```

Edit source inside the submodule and commit it on the fork's `sproutfs` branch.
The integration covers the feature, API schema, mapping owners, PMEM worker,
snapshot and restore paths, and the seccomp policy source. Snapshot format
version 13 records managed backing, so an ordinary memory-file snapshot cannot
silently capture a managed VM, and a build without the feature rejects managed
configuration. A managed capture is always a full snapshot with no memory file,
and a managed restore requires fixed RAM with no huge-page setting, in this
architecture's own layout.

Starting a machine takes the shared pager, the VM whose volumes it maps, a
feature-enabled binary and an explicit compiled seccomp
policy, including the VMM's memory-worker filter. RAM binds to the one volume
named `ram0`, whose size must be a multiple of the RAM pager's page, and each
PMEM device binds to the volume named by its device ID, whose size must be a
multiple of 2 MiB — Firecracker's own alignment, and the PMEM pager's page. Cold boot additionally supplies a
kernel, an optional initrd and boot arguments. A root PMEM device boots
directly with an appropriate Linux kernel; the qualification guest uses ext4
with `dax=always`.

RAM is one volume and one session, and the guest's physical address space is
not contiguous: an architecture reserves holes in it for MMIO. On x86_64 the
hole from 3 GiB to 4 GiB means any VM with more than 3 GiB of RAM sees two
regions, and a second hole at 256 GiB splits a larger one again. The volume does
not carry the holes. The VMM maps its one byte range onto the guest's regions in
ascending guest order, so on x86_64 volume bytes `[0, 3 GiB)` are guest
addresses `[0, 3 GiB)` and the rest begins at guest address 4 GiB: volume offset
3 GiB + x is guest address 4 GiB + x on every path the volume takes — fault,
seal, checkpoint, restore, fork and migration. On aarch64 RAM below 256 GiB is
one region and the volume is that region. The split is Firecracker's own
architectural layout for the memory size, and a restore whose snapshot records
any other layout is refused rather than read at shifted offsets. Nothing outside
the VMM knows a hole exists: the volume, the index, the pager and the control
protocol see one contiguous byte range. There is no 3 GiB limit on an x86_64
guest.

The supervisor creates private Unix sockets and checks the peer credentials
against its child process. Attachments initialize concurrently. A managed PMEM
device completes a guest flush with success on the VMM thread and asks the host
for nothing: durability is the host's interval checkpoint, taken under a vCPU
pause, so a flush is neither a fence nor a trigger. A timeout, pager loss or
authority failure terminates the VMM, which cannot keep running against
anonymous replacement pages. Failed starts and shutdown remove private sockets
and state files, detach logical pages and release arena allocation; console
output is in memory and goes with the process. Close the process before closing
the volumes, the spill file or the arena.

## Qualification

The [2 MiB HugeTLB qualification](measurements/hugetlb-2026-09-11.md) records
the current x86_64 execution, performance comparison and pool cleanup.

```sh
SPROUTFS_GCE_PROJECT=your-project bash scripts/bench-memory-gce.sh all
```

The Linux suites exercise real UFFD and KVM under strict seccomp. The GCP
qualification script provisions a disposable x86_64 host with a HugeTLB pool,
a renewable 24-hour lease and a hard 24-hour deletion deadline. Its `all`
command deletes the VM and boot disk and verifies their absence on exit.
Lima qualification requires a separately provisioned 2 MiB HugeTLB pool.

```sh
scripts/test-vm-memory-lima.sh
SPROUTFS_VM_MEMORY_REPEAT=20 scripts/test-vm-memory-lima.sh
scripts/test-firecracker-lima.sh
```

Use `SPROUTFS_LIMA_INSTANCE` to select an existing instance. The host needs Go,
Cargo and `limactl`, plus Python 3 for the full-guest suite; the guest needs
Cargo, Clippy, a source mount, KVM and kernel-fault UFFD support, and for the
full-guest suite a C compiler with static libc, libseccomp, curl and e2fsprogs.
The dedicated test process runs through `sudo -n` for UFFD, KVM and
physical-page inspection. Neither script changes device permissions, sysctls
or the VM configuration, and both remove their temporary artifacts on exit. The
ordinary Go suite skips these tests unless `SPROUTFS_VM_MEMORY_CLIENT` names the
built Rust adapter; there, a missing capability or inaccessible physical-page
information is an error rather than a silent pass. `SPROUTFS_PAGER_MEASURE` and
`SPROUTFS_FRAGMENT_MIB` enable the opt-in measurement runs.
`SPROUTFS_FIRECRACKER_RESIDENT_PAGES` sets the full-guest resident budget in
pages of the larger of the two, 2 MiB; it defaults to 48 pages (96 MiB), and
each pager's arena is that many bytes. At that budget, source and fork
each dirty 48 MiB of guest RAM, then verify markers in every 4 KiB subpage
after both allocations, forcing eviction, spill and refault. The earlier 32 MiB
budget is below this fixture's working set with 2 MiB pages; even 64 MiB
thrashes when both forks run. The 96 MiB qualification requires observed
eviction, spill and refault.

The memory suite runs a PMEM pager's 2 MiB page throughout, with one 4 KiB RAM
pager beside it in `small_page_linux_test.go`; the full-guest suites run the
production pair, a 4 KiB RAM pager over an ordinary memfd and a 2 MiB PMEM one
over the pool. One fault installs a whole pager page, a store copies it, a seal
write-protects it and a checkpoint publishes it as one part member, and a
spilled page comes back whole. Migration requests default to one 2 MiB page with
an 8 MiB per-peer in-flight byte budget; what a reply is counted in is the page
of the volume it answers for.

`small_page_linux_test.go` is the 4 KiB contract against the real kernel: a
parent reads one byte of a 512-page run, read-ahead loads the whole 2 MiB into
consecutive arena slots, a child of the same fork point maps that run with one
mapping command rather than 512 and reads nothing, and a store of one byte into
one of its pages makes exactly one page private — 4 KiB of arena, 511 of the run
still shared, the parent's own memory untouched.

The pager suite covers two processes physically sharing pages, verified
through pagemap; private writes including a first access that is a write;
shared eviction revoking every alias and releasing arena allocation; dirty
spill, refault and backing-slot reuse; a failed spill retaining the only
current copy; concurrent loads racing eviction; kernel access as the first
accessor without synthetic prefaulting; tiny KVM guests using both region kinds
across eviction and refault; stale, overflowing and out-of-range commands,
identical retries, control loss and teardown; fragmented mappings with reported
mapping counts; eager sparse zeros, first-store copy-on-write and mixed
zero-and-data read-ahead; two restores of an unpublished point plus a nested
fork after private writes, requiring that every resident page maps during
connect with no load and no fault on the first KVM reads; and a seal of a run of
pages whose slots descend, which must be one range write-protect covering
several of the client's mappings, leave the guest reading those pages without a
fault, and still trap and copy on the next store.

The simulated pager tests additionally require that a seal return before
anything reads the sealed set, that a store into a sealed page run at once and
leave the published bytes unchanged, that the sealed set survive reclaim and
refault of its pages, that a sealed page refaulted from spill share its memory
again so that retirement retires it rather than stranding a private page no
reservation covers, that sealed pages hold the dirty budget so an over-budget
store waits for the publication instead of failing, that an abandoned seal keep
every page for the next one, and that retiring a page never leave its private
page reachable from a binding owning neither a reservation nor a seal, which a
concurrent eviction is run against. A store into a page an abandoned seal handed
back, a seal cancelled partway, and an abandon racing another volume's faults
are all covered, the concurrent ones under the race detector.

They require of the settle that a region sharing pages with a sibling, which
takes one writable and stores nothing, publish no page and end with that page
shared again, its private bytes zero, its reservation back, its mapping of the
copy gone and a read of it taking exactly one fault onto the sibling's page; that a page the guest really stored into be published exactly
as before; that a store landing between the seal and the settle leave the guest
its own copy for the next checkpoint while this one publishes nothing; that a
copy whose origin was evicted, one made from zeros, one made from the
checkpoint's own held copy, one made from the name a fork point lent a page and
one made from a page only another host holds all be published untouched; that a
region whose only private pages were unchanged let a store waiting on the loss
window through; and that one worker and sixteen leave exactly the same set.

For migration the simulated tests require that a seal issue one range protection
per run and no mapping command at all, keeping every page where it was. They
require that the page server hand back a held page's current bytes, the sealed
copy for a page still sharing one and the guest's own copy for a page stored
into since the seal, that it say which served pages no checkpoint has, and that
it report a never-touched page and a page reclaimed since it was listed as
absent without ever reading the volume. A destination's pager must hold every
peer-served unpublished page as private dirty state and publish it in its next
checkpoint, and must wait at a full dirty budget for a checkpoint to relieve it
rather than fail the post-copy read. Finally they run a region against a volume whose
every call fails, as a handed-off host's volume does: verification, listing and
serving must all continue, a seal or a fault must report `ErrHandedOff`, and
detaching must release every resident page, reservation and logical page.

The full-guest suite builds the feature-enabled VMM and the restrictive aarch64
seccomp policy, downloads a pinned official Firecracker CI kernel with a
recorded checksum, creates an ext4 PMEM root containing a static guest
workload, and boots without an initrd or a memory file. It uses real KVM and
real TCP over loopback; object storage is simulated. The guest verifies actual
DAX with `statx`, performs mapped stores, `msync` and `fsync`, and reports its
extent so the host can check acknowledged bytes before the capture. The test
captures known disk and RAM values, changes the source, restores a fork at the
original values, verifies isolated fork writes and shared physical pages, then
replaces the fork's PMEM writer and requires the stale VMM to exit. It then
migrates that source away: it names both regions by their volumes, stops the VM,
and requires that the stop seal nothing and that the stopped source still serve
the pages it lists. A failed start and the final shutdown check private-file
removal, zero logical, dirty and resident pages, and zero memfd blocks.

It also runs the fan-out, which is the one shape neither the simulated
campaigns nor the rest of these suites has: one fork point, two children, both
received onto a second pager and page server one after the other exactly as the
orchestrator does — each one's root index published and each one's hold on the
parent released before the next — and then both guests reading every page of
their memory and their whole root volume at the same time while both are
checkpointed on an interval. The page server runs at the budgets a deployment
runs, because one peer is one destination host and both children are that host,
so their regions share every budget counted per peer; the destination's RAM
arena is a quarter of the memory the two of them map, so eviction, spill and
refault are on every one of those reads, while its PMEM arena holds both roots,
because pressing the disk as well would measure that instead. Both children must
answer: a child whose read never returns is a guest nothing can tell from a dead
one. At 4 KiB that phase takes minutes rather than seconds — read-ahead takes
only free arena slots, so under a quarter-sized arena every page of a scan is
its own fault — and the bound on it is sized for that.

Both suites report residency, sharing, load, mapping and fault counters; those
are small-workload observations, not throughput or latency targets. Recorded on
2026-09-10 on the Lima aarch64 instance: a second machine restored from an
point its sibling had already loaded, two regions of 512 pages, mapped all
1,024 pages with 2 commands, loaded nothing and took no fault; the full-guest
fork of a 128 MiB RAM and 64 MiB PMEM machine mapped 47,600 pages with 4
commands before resume, loaded 15 pages and took 3 faults.

The same run's capture of 8,495 dirty pages paused the guest for 144 ms: 142 ms
to pause the vCPUs, save the VMM state and seal, then 2 ms to resume. It moved
the sealed pages to storage in a further 373 ms with the guest already running,
after which selecting the checkpoint took 0.4 ms. The seal's share of the
pause scales with the runs of the dirty set while the 373 ms scales with bytes
and object-store latency, which is the whole point of the split; against a real
object store and a larger guest the ratio is far more extreme than this local
simulation shows.

Migrating that same source away took a stop pause of 7 ms, which is the state
capture alone: the stop seals nothing and uploads nothing. The stopped source
then listed 7,599 resident RAM pages for its destination to stream.

Seal cost is what a migration's pause pays for its dirty set, so it is measured
on its own with `SPROUTFS_PAGER_MEASURE=1`, over a guest that dirties 8,192 and
200,000 pages either contiguously or with no two dirty pages adjacent. The
scattered shape is the worst case for run coalescing, and what a guest touching
memory sparsely produces; the contiguous shape is the best case the mapping
replacement it superseded could ever have had. Recorded on 2026-09-10 on the Lima aarch64 instance, before
and after replacing per-run mapping replacement with range write-protection:

| dirty pages | runs | commands before / after | seal before | seal after |
| --- | --- | --- | --- | --- |
| 8,192 contiguous | 8 | 8 maps / 8 protects | 5.4 ms, 654 ns/page | 3.1–3.4 ms, 0.4 µs/page |
| 8,192 scattered | 8,192 | 8 maps / 8,192 protects | 610 ms, 74.4 µs/page | 12–14 ms, 1.5–1.7 µs/page |
| 200,000 contiguous | 196 | 196 maps / 196 protects | 189 ms, 943 ns/page | 122–185 ms, 0.6–0.9 µs/page |
| 200,000 scattered | 200,000 | 196 maps / 200,000 protects | 20.4 s, 102 µs/page | 338–367 ms, 1.7–1.8 µs/page |

Each figure is one run of a single-threaded measurement on a shared laptop VM;
the "after" column is the spread of two such runs, and the contiguous rows vary
by half as much again between them. The scattered rows do not need that
caution: two orders of magnitude is not measurement noise.

A real guest sits between the two shapes, and closer to the contiguous one than
the worst case suggests: the full-guest seal of 7,827 mapped dirty pages took
263 range protections, some thirty pages each, because a guest writing memory
tends to get neighbouring pages. That capture's pause barely moves. What the
change buys is the case this qualification cannot produce on a 128 MiB guest —
a large VM whose dirty set is scattered — where it is the difference between a
sub-second stop and a pause of tens of seconds.

A run is bounded by the seal's page-lock batch of 1,024, which is why 200,000
contiguous pages are 196 commands rather than one. The batched mapping
replacement issued few commands even for a scattered dirty set, but each of its
runs was a fresh mapping whose page tables had to be reinstalled, which is the
20 seconds: 200,000 pages of a guest dirtying scattered memory made a
migration's pause unusable, and protecting them in place makes it a third of a
second. What remains, under a microsecond per page in the contiguous cases, is
the per-page bookkeeping of taking the pages into the sealed set, not kernel
work.

Smaller pager pages were measured on 2026-09-11 on the same instance before the
2 MiB page was fixed — boot, pnpm-install, a capture and a fan-out of four forks
at 4, 16 and 64 KiB ([records](measurements/page-unit-2026-09-11.json)). Further
work includes other kernels and architectures, jailer namespaces and cgroups,
scheduling fairness across many guests under memory and I/O pressure, and real
build or filesystem workloads with HugeTLB backing.
