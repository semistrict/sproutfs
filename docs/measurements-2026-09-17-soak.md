# The GCE soak — 2026-09-17

`scripts/demo-gce.sh soak` on the two-host demo cluster (`plans/gce-soak-2026-09-16.md`),
seed 1, six rounds, 512 MiB Alpine guests each holding a 256 MiB memory witness
and a 256 MiB witness file, mutated every round. The run that passed is
`.workload-runs/soak-20260917T025539Z` on the machine that ran it (that
directory is not versioned); this records what it showed.

## The run that passed

166 operations, 146 witness checks, every one of them holding: after every
mutation, every fork (two children on the parent's host and two on the other,
each checked against the parent's state and again after diverging under its
own seed), every migration, every stop and warm start, every cold start with
a larger memory and root volume grown in the guest, and a host killed in
round six. Two refusals, both a cold start asking for 768 MiB on a host with
none free, recorded as the plan says. Every VM deleted at the end and
`sproutfsctl check` reporting that the deployment's durable state agrees with
itself.

What the operations reported, per round, is in the run's own table; the
shape of it:

| operation | pause or checkpoint | behind it | wall, slowest |
| --- | --- | --- | --- |
| fork, two local children | 0.1 s | 1.6–10 s to run the children | 10 s |
| fork, two remote children | 0.1 s | 5–20 s of post-copy | 20 s |
| migrate | 0.6–0.75 s | 0.3–4.9 s of stream | 5.7 s |
| stop | the checkpoint it published | — | 6.3 s |
| start, warm | the checkpoint it came back at | — | 1.0 s |
| start, cold with resize | the discarding checkpoint | grow 0.5 s | 2.1 s |
| witness check, cold after a remote fork | — | — | 31 s |

The object store over the whole run, both hosts together: about 17,000 GETs
for 18 GiB, 1,300 PUTs for 41 GiB, 1,100 deletes, 2 failed GETs.

## What it took to get there

Eight runs failed before the ninth passed, each on something the simulated
campaigns and the Lima suites had not reached. In the order found:

1. `create` rolled the host pods while they were importing their templates.
2. A host pod replaced mid-import could never import again: its template's
   fixed identity was refused as used. Templates are now named by their
   image's bytes and imported once per deployment.
3. A same-host fork through the HTTP API was refused: the handler demanded a
   page address a local child never has.
4. A fan-out that failed part way was cleaned up with release instead of
   abandon, and the stale-handover survey retried the refused release for
   ever.
5. A parent's local fork holds were invisible to the orchestrator's survey,
   so nothing but the deadline could ever release them.
6. Every remote handoff ended with pages the destination counted as never
   served: a guest's first store into an unpublished page fetched it through
   copy-on-write without reporting it installed.
7. A finished post-copy closed its receive under a guest fault still on the
   wire, which killed the guest, and the dead child's shared frame then
   failed its sibling's eviction.
8. Every checkpoint killed every command running in the guest: the VMM
   published a vsock transport reset on snapshot creation, and the host side
   of the connection stayed open until its ten-minute timeout.
9. The guest kernel refuses write opens of a mounted block device, so
   `resize2fs` cannot grow a mounted root there; the witness grows the
   filesystem with the ext4 resize ioctl instead.

Not exercised by this run: the host kill landed on a host running no VMs,
so recovery of running VMs after a host loss, and the orchestrator's watch
of a migration's source, remain proven only in simulation. A seed whose kill
lands on a loaded host is the next run to take.
