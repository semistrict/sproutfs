# The GCE soak — 2026-09-17

This is `scripts/demo-gce.sh soak` on the two-host demo cluster
(`plans/gce-soak-2026-09-16.md`), with seed 1 and six rounds. The guests were
512 MiB Alpine VMs. Each guest held a 256 MiB memory witness and a 256 MiB
witness file, and both were mutated every round. The output of the passing run
is `.workload-runs/soak-20260917T025539Z` on the machine that ran it. That
directory is not versioned. This document records what the run showed.

## The run that passed

The run performed 166 operations and 146 witness checks, and every check
passed. Checks ran after each of these:

- every mutation;
- every fork: two children on the parent's host and two on the other host,
  each checked against the parent's state and checked again after diverging
  under its own seed;
- every migration;
- every stop and warm start;
- every cold start with a larger memory and a root volume grown in the guest;
- a host killed in round six.

There were two refusals. Both were a cold start asking for 768 MiB on a host
with no free memory, and both were recorded as the plan specifies. Every VM was
deleted at the end, and `sproutfsctl check` reported that the deployment's
durable state is consistent.

The run's own table has what each operation reported, per round. A summary:

| operation | pause or checkpoint | behind it | wall, slowest |
| --- | --- | --- | --- |
| fork, two local children | 0.1 s | 1.6–10 s to run the children | 10 s |
| fork, two remote children | 0.1 s | 5–20 s of post-copy | 20 s |
| migrate | 0.6–0.75 s | 0.3–4.9 s of stream | 5.7 s |
| stop | the checkpoint it published | — | 6.3 s |
| start, warm | the checkpoint it came back at | — | 1.0 s |
| start, cold with resize | the discarding checkpoint | grow 0.5 s | 2.1 s |
| witness check, cold after a remote fork | — | — | 31 s |

Object-store traffic over the whole run, for both hosts combined: about 17,000
GETs for 18 GiB, 1,300 PUTs for 41 GiB, 1,100 deletes, and 2 failed GETs.

## What it took to get there

Eight runs failed before the ninth passed. Each failure was in something that
the simulated campaigns and the Lima suites had not reached. In the order
found:

1. `create` did a rollout of the host pods while they were importing their
   templates.
2. A host pod replaced during an import could never import again, because its
   template's fixed identity was refused as already used. Templates are now
   named by their image's bytes and imported once per deployment.
3. The HTTP API refused a same-host fork, because the handler required a page
   address that a local child never has.
4. A fan-out that failed partway was cleaned up with release instead of
   abandon. The stale-handover survey then retried the refused release
   indefinitely.
5. The orchestrator's survey could not see a parent's local fork holds, so only
   the deadline could release them.
6. Every remote handoff ended with pages that the destination counted as never
   served. The cause was that a guest's first store into an unpublished page
   fetched the page through copy-on-write without reporting it as installed.
7. A finished post-copy closed its receive path while a guest fault was still
   on the wire. This killed the guest. The dead child's shared page then made
   its sibling's eviction fail.
8. Every checkpoint killed every command running in the guest. The VMM
   published a vsock transport reset when it created a snapshot, and the host
   side of the connection stayed open until its ten-minute timeout.
9. The guest kernel refuses write opens of a mounted block device, so
   `resize2fs` cannot grow a mounted root there. The witness now grows the
   filesystem with the ext4 resize ioctl instead.

This run did not exercise everything. The host kill landed on a host that ran
no VMs. Recovery of running VMs after a host loss, and the orchestrator's watch
of a migration's source, are therefore still proven only in simulation. The
next run to take is a seed whose kill lands on a host that runs VMs.
