# Desired properties

Properties the system should have, one per file, each stated as a user would
observe it. Some hold today and some do not yet.

- [Stopping does not evict](stop-does-not-evict.md)
- [Tiers evict independently](tiers-evict-independently.md)
- [Resident pages come from memory](restart-reads-memory.md)
- [Then from the cluster's disk cache](restart-reads-disk-cache.md)
- [The object store is the last resort](object-store-last-resort.md)
- [One disk cache for the whole host](disk-cache-shared-by-every-vm.md)
- [The disk cache uses the disk it is given](disk-cache-uses-the-disk.md)
- [The hosts' disk caches form one cache](disk-caches-form-one-cache.md)
- [A cached page survives losing a host](a-page-survives-losing-a-host.md)
- [A hot page spreads its load](a-hot-page-spreads-its-load.md)
- [A slow host does not slow reads](a-slow-host-does-not-slow-reads.md)
- [Two hosts are enough](two-hosts-are-enough.md)
- [Serving a peer copies nothing into memory](serving-a-peer-copies-nothing.md)
- [One disk limiter](one-disk-limiter.md)
- [The disk limiter follows a goal](disk-limiter-goals.md)
