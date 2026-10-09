# Demo manifests

Plain YAML for the one-node demo of
[the demo plan](../plans/demo-gce-2026-09-13.md): a namespace, two Deployments,
two Services, two NetworkPolicies, a PodDisruptionBudget, and the
ServiceAccount, Role and RoleBinding the orchestrator reads the Kubernetes API
with.

```
kubectl apply -f deploy/
```

`scripts/demo-gce.sh create` applies these on the demo VM. Files apply in name
order, so the namespace exists first. `deploy/testdata` holds a probe pod the
script runs by name; `kubectl apply -f` does not recurse into it.

## What the manifests assume

Both containers come from one image, `sproutfs:demo`, with
`imagePullPolicy: Never`: the image is imported into the node's containerd
(`k3s ctr -n k8s.io images import`). Until it is, the pods stay in
`ErrImageNeverPull`.

The node must have a 2 MiB HugeTLB pool of 12 GiB and `/dev/kvm`.
`scripts/lib/gce-demo-startup.sh` sets both up before k3s starts, so kubelet
advertises `hugepages-2Mi: 12Gi` when it registers the node. A host runs one
pager per kind of memory region, both on 2 MiB pages from the pool, so both
arenas come out of the pod's `hugepages-2Mi` allotment. A host whose 2 MiB
arenas are larger than that allotment or the node's pool refuses to start and
says by how much. RAM at 4 KiB
(`SPROUTFS_RAM_PAGE_BYTES=4096`) puts the RAM arena on ordinary memory charged
to the pod's `memory` request; PMEM at 4 KiB (`SPROUTFS_PMEM_PAGE_BYTES=4096`)
does the same for the PMEM and ephemeral arenas. The sizes come from the
workload template: a 2 GiB guest and a few forks need about 5 GiB of arena per
host to stay resident, which the default share splits into 3.75 GiB of RAM and
1.25 GiB of PMEM.

The node has one 200 GiB disk, a 193 GiB filesystem, holding both pods'
scratch `emptyDir`s and the page caches' files under
`/opt/sproutfs-demo/cache`. The caches' files outlive the pods, so a replaced
pod reads back what its predecessor kept. Both must be on one filesystem,
because a host's disk limiter measures one filesystem; a host whose cache
directory is elsewhere refuses to start.

## What the image brings, and what the node brings

`deploy/Dockerfile` puts these fixed paths in the image:

| Path | What |
| ---- | ---- |
| `/usr/local/bin/firecracker` | the static x86_64 VMM |
| `/usr/share/sproutfs/seccomp.bpf` | its compiled x86_64 seccomp policy |
| `/usr/share/sproutfs/vmlinux` | the pinned guest kernel |
| `/usr/local/bin/sproutfs-guest-agent` | the agent that runs inside a guest, copied into the guest image |
| `/usr/local/bin/sproutfs-guest-witness` | the witness that checks a guest's memory and disk, copied into both guest images |
| `/usr/local/bin/sproutfs-guest-chase` | the client that loads Valkey with dependent reads and walks it, copied into the valkey guest image that `scripts/bench-app-restore-gce.sh` builds |

The guest root images are built on the node by `scripts/lib/demo-image.sh` at
`/opt/sproutfs-demo/guest/guest.ext4` and
`/opt/sproutfs-demo/guest/workload.ext4`. The host pods mount that directory
read-only at `/usr/share/sproutfs/guest` (named by `SPROUTFS_TEMPLATES`). The
mount is `DirectoryOrCreate`, so a pod starts on a node where the build has not
run and finds no image.

Two templates are registered. `alpine` is the minirootfs with an interactive
shell, used by every flow of `demo-gce.sh run`. `workload` adds git, ripgrep,
Node, pnpm and three MIT-licensed TypeScript repositories with their
dependencies in a pnpm store; `demo-gce.sh workload` measures it. Its VMs get
2 GiB of RAM rather than the 512 MiB default.

## Generated configuration

`scripts/demo-gce.sh` generates a ConfigMap next to these files in the copy it
applies on the VM, naming the bucket and the arena mode:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: sproutfs-demo
  namespace: sproutfs
data:
  bucket: sproutfs-demo-<project>
  prefix: demo
  arena: isolated
```

`arena` is `SPROUTFS_DEMO_ARENA`, `isolated` unless that names `shared`;
`scripts/demo-gce.sh redeploy` with it set changes the mode. The host reads it
as `SPROUTFS_ARENA`; without the key the host uses its default, `isolated`.

Applying `deploy/` alone leaves the pods in `CreateContainerConfigError` until
that ConfigMap exists. Create it by hand for a cluster the script did not
provision.

## Ports

| Port | Name   | Workload     | What listens                                              |
| ---- | ------ | ------------ | --------------------------------------------------------- |
| 8080 | `api`  | host         | the host's HTTP API: create, open, fork, capture, console, exec, migrate-out, migrate-in, release, drain, stop, kept, delete, status |
| 8081 | `page` | host         | the peer server: memory of every VM this host has handed to another |
| 8080 | `api`  | orchestrator | the orchestrator's HTTP API, behind the ClusterIP Service |

Both Services target the port names, not the numbers.

## Required HTTP endpoints

The manifests depend on these.

| Method | Path       | Workload | Called by | Contract |
| ------ | ---------- | -------- | --------- | -------- |
| GET    | `/healthz` | both     | readiness probe | 2xx once the process can take work. A host answers it once every configured guest image has a published template, so no VM is placed on a host that would read a whole image inside the request. Until then the host is not an endpoint of the headless Service. Served without the token. |
| GET    | `/livez`   | both     | liveness probe | 2xx while the process can work, 503 once it cannot: a host whose supervisor has closed or whose context was cancelled has released its pager and VMM processes. Separate from readiness so a host importing an image is not restarted. Served without the token. |
| GET    | `/version` | both     | an operator | the build's `git describe`, stamped at link time |
| GET    | `/metrics` | host     | a scraper | Prometheus text: the pager's occupancy and sharing (`sproutfs_pager_unique_resident_bytes`, `sproutfs_pager_mapped_resident_bytes`, `sproutfs_pager_shared_saved_bytes`, each labelled `kind="ram"` or `kind="pmem"`), what an isolated arena copies between its files (`sproutfs_pager_moved_pages_total`, `sproutfs_pager_fork_copies_total`), the peer server's counters, the memory and cache allotments, the disk limiter's choice, the page cache's disk (`sproutfs_cache_disk_*`: what it holds, its hits, what it read back at start) and the object-store counters per operation. It requires the token; nothing in this namespace scrapes these pods, and the demo reads them with `kubectl exec` or a port-forward. |
| POST   | `/drain`   | host     | preStop hook | Migrate every VM to another host and return when none are left to hand over (`Status().Serving` empty). Four at a time, 60 s per VM, 30 min overall; the preStop command gives up at 31 min. `terminationGracePeriodSeconds` is 1920 s: those 31 min plus 60 s of shutdown, 30 s to stop the API and 30 s for the supervisor's close, which publishes a final checkpoint of every VM that did not move. |

Draining is a POST carrying the token, because proxies and link checkers GET
any URL they see. The `preStop` hook therefore execs `sproutfs-host drain`,
which calls the container's own API over the loopback with the server's
environment; an `httpGet` hook can carry neither a method nor a header.

## Authentication

Every request to either API except `/healthz` and `/livez` carries the
deployment's shared bearer token, `Authorization: Bearer <token>`, and is
refused with 401 without it. One token admits the whole control plane: the
orchestrator to the hosts, a draining host to the orchestrator, and
`sproutfsctl` to either. It grants no authority over a VM (that is the epoch in
its control record) and carries no identity.

`scripts/demo-gce.sh` generates it per deployment into the Secret
`sproutfs-api-token`, which every pod reads as `SPROUTFS_API_TOKEN`. Every side
trims whitespace from it, so a Secret with a trailing newline is the same
token. A pod without the Secret does not start. `sproutfsctl` run through
`kubectl exec` in the orchestrator pod inherits it from the container's
environment. A process started by hand with no token serves an API that admits
anyone and says so at startup.

`30-networkpolicy.yaml` restricts ingress to the host API, the page server and
the orchestrator API to pods labelled `app.kubernetes.io/part-of: sproutfs`.
k3s enforces NetworkPolicy by default with its own controller.

## The host API

Port 8080. Requests and replies are JSON. A failure is
`{"op":"...","error":"..."}` with 400 for a bad request, 404 for a VM this host
does not run, 409 for one it already runs or cannot hand over, and 503 while it
is closing.

Every shape is declared in `api/host`, which holds only wire types. A handoff
is plain data the control plane carries unread, so `sproutfsctl` and the
orchestrator use it without linking a pager or a checkpoint store. `host`
converts between it and its own types.

| Method | Path | What it does |
| ------ | ---- | ------------ |
| GET | `/healthz` | readiness |
| GET | `/status` | the VMs this host runs; the handovers it still holds pages for (VMs it migrated away and the children of every fork point it took); the VMs a receive is in flight for; the pager's residency and sharing; what the peer server has answered; the RAM allotment and page-cache cap; the disk limiter's choice; the page cache's disk (its file, what it holds, the reads it served, what it read back at start); the host's name and disk in the membership and the membership generation it holds; and the object-store calls, failures and bytes per operation since start |
| POST | `/templates` | the guest image as the body, `?memory=` the RAM a VM of it starts with and `?tenant=` the tenant it is for (a tenant's VMs fork only its own templates): stage the image under the scratch, import it into the template its bytes name, and report the template's identity and checkpoint. An image already imported costs one control record read. Any host creates from the template by that identity |
| POST | `/vms` | `{"id","template","memory"?,"disk"?,"vcpus"?,"ephemeral"?,"nested"?}`: fork the template's root checkpoint, publish the VM's own first checkpoint, and boot it, with the RAM, root volume size and processors asked for, else the template's or the host's defaults. The template is the guest image in a published checkpoint, named by the image's bytes and imported once per deployment. Until the VM's root is published nothing can recover or fork it; nothing has run yet, so the root seals and uploads no pages. `{"id","from":{"vm","checkpoint"?},...}` starts from another VM's published checkpoint instead: the one its record selects, one it keeps, or one a pin holds. That VM need not run anywhere. The checkpoint is pinned in its record without its epoch. A checkpoint with VMM state resumes the guest unless the create names a shape; any other boots cold over the disk it inherits. `resumed` in the result says which. 409 for a checkpoint that is not published or that its VM's writer may be reclaiming. `ephemeral` adds a PMEM device of that many bytes that no checkpoint holds (docs/volumes.md#ephemeral-disks). `nested` (experimental, x86_64 only) lets the guest run VMs; its RAM is never captured or moved, so capture, suspend, fork and migration of it are refused (docs/hosting.md#nested-vms) |
| POST | `/vms/{id}/open` | open a VM from the checkpoint its control record selects and resume it; this is how a host loss is recovered. `{"cold":true}` instead discards its memory and VMM state in one checkpoint and boots the kernel from the root volume as the last checkpoint published it; the guest's journal recovers as after a power cut. `{"memory","disk"}` set the VM's shape: any memory the host admits, up or down, and a root volume that may only grow, whose new pages read as zeroes until the guest's `witness grow` takes them. Both require `cold`. A host whose VMM starter cannot boot a kernel refuses a cold start before discarding anything |
| POST | `/vms/{id}/fork` | `{"ids": [child...], "destination"?}`: seal the running VM once and hand every child over from that pause. The reply is a handoff per child for the control plane to give the destination's `/vms/receive`. This host holds the fork point for each child until told to release it, or for `hold_seconds` at most. With a destination it serves that child's unpublished pages; without one the child comes back here over the sealed pages. Nothing is published; the parent keeps running |
| POST | `/vms/{id}/capture` | take a checkpoint now; returns the pause and how long its pages took to become durable. `{"into"}` captures the running VM into a new VM of that identity: one pause, and a new VM whose root publishes the sealed pages and the VMM state and which never boots. The VM keeps running. Any host can open the new VM, which resumes where the pause left it. 409 for an identity that exists. `{"keep":true}` keeps the checkpoint, so a create can start from it later |
| GET | `/vms/{id}/console?since=N` | the serial console from byte N, out of the newest 1 MiB the host keeps in memory. A read from before that starts at the oldest byte kept, which `offset` reports and `dropped` says |
| POST | `/vms/{id}/console` | `{"data":"..."}`: type into the serial console |
| POST | `/vms/{id}/exec` | `{"cmd","timeout"}`: run a shell command in the guest over the VM's vsock and return `{"exit","stdout","stderr","seconds"}`. 503 while the guest's agent is not answering, as while the VM boots |
| POST | `/vms/{id}/migrate` | `{"destination":"host:port"}`: stop the guest and hand the VM over; returns the handoff the control plane carries to the destination |
| POST | `/vms/receive` | a handoff: open the VM, resume it from the captured state and stream the source's pages in. Returns once the source may release them |
| POST | `/vms/{id}/released` | stop serving a migrated VM's pages and close the process that held them. 409 until the destination has fetched every page this host holds that no checkpoint has. For a fork's child, ends the hold on the fork point instead; for a child on this host it is 409 until this host has taken the child in |
| POST | `/vms/{id}/abandoned` | give up one handover that will never be received (a fork's child whose destination refused it, or one a fan-out never offered): whatever is held for it goes, and a fork's parent takes its sealed pages back. Unlike `released`, it refuses nothing |
| POST | `/drain` | migrate every VM away and return when none are left to hand over |
| POST | `/vms/{id}/stop` | publish everything the guest still holds, then close the VMM process, give the pages back and release the handle. The control record and objects stay, so any host can open the VM again at the bytes this published. Returns the checkpoint it published. Nothing is given up until the checkpoint lands, so a publication the store refused leaves the VM running. 409 for a VM a fork point holds sealed, as for a delete. `{"suspend":true}` also publishes the memory and VMM state, and `{"keep":true}` keeps the checkpoint |
| GET | `/vms/{id}/kept` | the VM's kept checkpoints from its control record: each one's `checkpoint`, `time`, `state` (a create from it resumes the guest) and `forked` (a VM was created from it). Any host answers for any VM |
| POST | `/vms/{id}/kept/{checkpoint}/release` | give up one kept checkpoint and delete what only it held. 409 for a checkpoint a VM was created from, and for one the VM does not keep. Any host does this for any VM |
| DELETE | `/vms/{id}` | close the VM and delete its control record. A VM this host does not run is deleted from the bucket all the same. Refused for one a fork point holds sealed |

## The orchestrator API

Port 8080; `sproutfsctl` drives it. Hosts come from the Kubernetes API. VMs come
from the control records in the bucket, one key each under `control/`, so a
listing reads a page of keys rather than every checkpoint object, and from the
hosts. A survey asks every host at once and waits seconds for each, so a host
that never answers is reported with its failure. A survey also answers the read
requests that arrive within a second of it, so a polled console does not fan
out per frame.

The orchestrator keeps one SQLite table on the node: which host each VM is on
and what it was last asked to do (creating, running, migrating between a named
pair, stopped or recovering). Requests for one VM are routed by it. A row also
keeps the VM's template, its parent, its RAM and whether it is marked to pull
its memory, which every start, recovery and migration carries. There is no
table of hosts: where they are and what they run are read fresh for each
request.

The table is not authoritative. It is rebuilt from a survey and the bucket at
start and every thirty seconds, and a row that disagrees with the host loses.
Only operations that move a VM and the reconciler write it. The reconciler also
releases handovers nothing waits on and forgets VMs another orchestrator
deleted. Reads write nothing.

A row saying an operation is in flight is trusted for two minutes. While it is,
the source's pages stay served, the reconciler leaves the VM, and recovery is
refused. Two minutes is within the four checkpoint intervals a host holds one
handover, so a stale row stops pinning a source before the host gives the
pages up.

| Method | Path | What it does |
| ------ | ---- | ------------ |
| GET | `/healthz` | readiness |
| GET | `/hosts` | every host pod, what it runs, what it still serves, its pager's residency and sharing, its member of the membership, and why it did not answer if it did not |
| GET | `/vms` | every VM, with the host running it or none when its host is gone. A deleted VM is absent |
| POST | `/templates` | the guest image as the body, `?memory=`: stream it to the ready host with the most memory free, which imports it; reports the template's identity for a create's `template` |
| POST | `/vms` | `{"template","memory"?,"disk"?,"vcpus"?,"ephemeral"?,"nested"?}`: allocate a ULID and create the VM at that shape on the host whose guests have promised the least of its arena; 503 when the VM's RAM (asked for, or its template's) fits on none. `{"from":{"vm","checkpoint"?},...}` creates it from another VM's published checkpoint, such as a stopped VM's: the one its record selects, one it keeps, or one a fork pinned. Its RAM is that VM's unless it asks for its own, and the table records that VM as its parent. A checkpoint with VMM state resumes the guest unless the create names a shape, and `resumed` says so; any other boots cold. `nested` (experimental) makes a nested VM; migrating one, also in a drain, stops it and boots it cold on the destination and says `rebooted` |
| POST | `/vms/{id}/fork` | `{"count", "to"?}`: fork the running VM. All children come from one pause and are handed over like a migration. By default they go to the parent's host, over the sealed pages; `to` places them on another host, which pulls the pages no checkpoint holds from the parent. The fan-out is admitted against the receiving host before the parent pauses, and each child is released once it holds every page it inherited. Returns the children and what the fork cost |
| POST | `/vms/{id}/capture` | take a checkpoint on the host running it. `{"new":true}` captures the VM into a new VM whose identity this allocates; the new VM never boots and is recorded `stopped` with the VM as its parent, so `start` and `create --from` work on it. `{"keep":true}` keeps the checkpoint; it does not combine with `new` |
| POST | `/vms/{id}/migrate` | `{"to"}`: migrate out, receive, then release. An empty `to` picks the other host with the most memory free, as a draining host asks; a named destination without room is refused |
| POST | `/vms/{id}/recover` | `{"force"?}`: reopen a VM on a live host that does not run it. Reopening takes the record's epoch and fences whatever held it, so it needs every listed pod to answer and none to run the VM. Refused while a live host runs it, while any host still serves its unpublished pages, or while the table shows an operation in flight; `force` does not override these. Without `force`, also refused while any listed pod is silent |
| POST | `/vms/{id}/stop` | have the host running the VM publish what its guest holds and close it. The VM stays listed on no host, recorded `stopped`. Returns the checkpoint it published. 404 for a VM no host runs, 409 for one two hosts claim. `{"keep":true}` keeps the checkpoint |
| GET | `/vms/{id}/kept` | the VM's kept checkpoints, through any ready host |
| POST | `/vms/{id}/kept/{checkpoint}/release` | release one kept checkpoint through any ready host. 409 for one a VM was created from |
| POST | `/vms/{id}/start` | `{"to"?, "cold"?, "memory"?, "disk"?, "vcpus"?}`: open a stopped VM on the named host, refused without room, or on the ready host whose guests have promised the least of its arena. Unlike `recover` it needs no evidence of a loss, so a silent host does not refuse it. It is refused while a host runs the VM, two hosts claim it, a host still serves its unpublished pages, or an operation is in flight. `cold` starts it without its memory, and `memory` and `disk` resize it. After a resize, the VM's committed RAM is its own rather than its template's, and later placements admit against it |
| GET | `/check` | check the whole object namespace and report every violation: the key, what is wrong, and the class of host loss that could excuse it. Violations are a 200 with a body; a store that could not be read is a 500. These leftovers are allowed: a publication in flight, the checkpoints a deleted VM pinned, a VM's own root, a template import's intermediate checkpoints, and the superseded epoch every takeover leaves, including a template whose unfinished import a later one recovered |
| POST | `/hosts/{name}/kill` | delete a host pod with no grace period, skipping the preStop drain: the demo's host loss |
| DELETE | `/vms/{id}` | close and delete the VM on its host, or on any ready host when none runs it. The record goes, which frees the identity, then the VM's checkpoint objects, except the checkpoints it was forked at, which a descendant may still read |
| GET | `/vms/{id}/console?since=N` | the VM's console, proxied from its host |
| POST | `/vms/{id}/console` | type into the VM's console |
| POST | `/vms/{id}/exec` | `{"cmd","timeout"}`: run a shell command in the guest through the host the table names; returns `{"host","vm","result"}` |
| POST | `/drains` | `{"host","vm","phase","error"}`: a draining host reporting that it began or finished handing one VM over. It only updates the table |

## The CLI

`sproutfsctl` is in the same image and uses only the orchestrator API. It reads
`SPROUTFS_ORCHESTRATOR`, `http://localhost:8080` by default:

```
kubectl port-forward -n sproutfs service/sproutfs-orchestrator 8080:8080 &
sproutfsctl create --template alpine
sproutfsctl console vm-01j...
sproutfsctl fork vm-01j... --count 20
sproutfsctl fork vm-01j... --to sproutfs-host-7f9c4-qk2wd
sproutfsctl migrate vm-01j... --to sproutfs-host-<other>
sproutfsctl kill-host sproutfs-host-<one>
sproutfsctl recover vm-01j...
sproutfsctl exec vm-01j... -- uname -a
sproutfsctl stop vm-01j...
sproutfsctl create --from vm-01j...
sproutfsctl capture vm-01j... --new
sproutfsctl capture vm-01j... --keep
sproutfsctl kept vm-01j...
sproutfsctl create --from vm-01j...@CHECKPOINT
sproutfsctl release vm-01j...@CHECKPOINT
```

`create --from VM` starts a VM from another VM's published checkpoint.
`--from VM@CHECKPOINT` names a checkpoint that VM keeps or a pin holds. A
checkpoint with VMM state resumes the guest and the CLI prints "resumed". A
checkpoint without state, or a create with `--memory`, `--disk` or `--vcpus`,
boots cold over the disk it inherits.

`capture VM --keep` and `stop VM --keep` keep the checkpoint they publish.
`kept VM` lists a VM's kept checkpoints: when each was taken, whether it holds
VMM state, and whether a VM was created from it. `release VM@CHECKPOINT` gives
one up and deletes what only it held; a checkpoint a VM was created from cannot
be released.

`capture VM --new` captures a running VM into a new, stopped VM; `start`
resumes it where the capture paused.

`exec` stops parsing at the `--`, so the guest's flags and quoting reach it
unchanged. The command's stdout and stderr go to this process's, and a non-zero
exit makes `sproutfsctl` exit non-zero. It uses the VM's vsock, not a guest
network.

A console session ends with its input. `--for` keeps it reading for a fixed
time, which is how `scripts/lib/demo-run.sh` asks a guest a question:

```
echo 'uname -a' | sproutfsctl console vm-01j... --for 8s
```

`sproutfsctl hosts` reports each host's resident and shared page counts, the
pages its peer server has handed to other hosts, and what it runs.
`sproutfsctl store` reports each host's object-store counters, one row per host
and operation; subtract two readings for the cost of the work between them. The
cost of one checkpoint is a line the host logs when its publication ends: the
dirty pages sealed, the bytes uploaded, the objects written, and the pause and
upload durations.

## Environment

### `sproutfs-host`

| Variable | Source | Value | Meaning |
| -------- | ------ | ----- | ------- |
| `SPROUTFS_BUCKET` | ConfigMap `sproutfs-demo` key `bucket` | `sproutfs-demo-<project>` | the bucket holding control records, checkpoints and pages |
| `SPROUTFS_PREFIX` | ConfigMap `sproutfs-demo` key `prefix` | `demo` | the deployment's prefix inside that bucket |
| `SPROUTFS_API_PORT` | literal | `8080` | port for the host API |
| `SPROUTFS_API_TOKEN` | Secret `sproutfs-api-token` key `token` | generated per deployment | the shared bearer token. Without it the process serves an API that admits anyone and says so at startup |
| `SPROUTFS_PAGE_SERVER_PORT` | literal | `8081` | port for the peer server |
| `SPROUTFS_HUGEPAGE_DIR` | literal | `/hugepages-2Mi` | the pod's hugetlbfs mount. The PMEM arena is a `MFD_HUGETLB` memfd, not a file in it, but the kubelet grants the HugeTLB allotment through the mount, so the host refuses to start without it. At the default 2 MiB RAM page the RAM arena is also a `MFD_HUGETLB` memfd; at 4 KiB it is an ordinary memfd and does not touch the pool |
| `SPROUTFS_SCRATCH_DIR` | literal | `/var/lib/sproutfs` | node-disk `emptyDir` for the spill files and VMM scratch. A starting host wipes it. It has no size limit: the disk limiter bounds what the host writes, and a kubelet limit would evict the pod |
| `SPROUTFS_CACHE_DIR` | literal | `/var/cache/sproutfs` | the page cache's directory, a `hostPath` at `/opt/sproutfs-demo/cache/<namespace>` on the same filesystem as the scratch, which the host checks. The host takes the first file (`cache-0`, `cache-1`, ...) no other process holds locked, so the two pods never share one and a replaced pod reads back its predecessor's. Unset, the cache is in the scratch ([hosting](../docs/hosting.md#the-caches-file)) |
| `SPROUTFS_ARENA_BYTES` | literal | `5368709120` | the host's resident page store, divided between the two pagers by `SPROUTFS_RAM_SHARE_PERCENT` into two memfds. At the default 2 MiB pages both shares come out of the pod's HugeTLB allotment; a pager at 4 KiB takes its share from the pod's `memory` request |
| `SPROUTFS_RAM_PAGE_BYTES` | unset | `2097152` | the RAM pager's page: `2097152` on the HugeTLB pool, where the RAM arena comes out of the HugeTLB allotment, or `4096` on ordinary memory, which holds a tenth of the memory for forks that write little and scattered and is slower otherwise (docs/vm-memory.md). RAM budgets, template memory and `SPROUTFS_VM_MEMORY_BYTES` are counted in this page |
| `SPROUTFS_PMEM_PAGE_BYTES` | unset | `2097152` | the PMEM and ephemeral pagers' page: `2097152` on the HugeTLB pool, or `4096` on ordinary memory charged to the pod's `memory` request, so a checkpoint publishes the 4 KiB pages a guest wrote. PMEM and ephemeral budgets are counted in this page. A disk is still a whole number of 2 MiB, as Firecracker requires. A host refuses volumes published at the other page, so a fleet runs one PMEM page; changing it means new templates and VMs |
| `SPROUTFS_RAM_SHARE_PERCENT` | unset | `75` | the share of the arena, the spill file and the page budgets for the RAM pager; PMEM takes the rest. RAM is where forks diverge and the root is mostly read: the 2026-09-19 fan-out measured a fork holding about 114 MB of RAM privately against 10 MB of root. A share that cannot divide the arena or spill file into whole pages of both pagers is refused |
| `SPROUTFS_ARENA` | ConfigMap `sproutfs-demo` key `arena`, optional | `isolated` | how each pager divides its resident pages between files: `isolated` gives each memory region a private file and every VMM read-only files for pages another region may map; `shared` keeps them in one file every VMM maps read-write. `isolated` also sends a fork point's lent pages to its children on the host in a read-only file, and checks a published page against its upload's digest before another region shares it. It confines a VMM to its own VM's memory only while the VMM runs as neither root nor the host's user (`SPROUTFS_VMM_JAIL`). Any other value is refused |
| `SPROUTFS_MEMORY_BYTES` | literal | `12884901888` | the RAM allotment both pagers take their pages from, in bytes. Defaults to the arena plus 1 GiB |
| `SPROUTFS_CACHE_BYTES` | literal | `1073741824` | the page cache's own cap |
| `SPROUTFS_EPHEMERAL_BYTES` | literal | `2147483648` | the ephemeral pager's spill file: every ephemeral disk this host admits, in whole 2 MiB pages. Unset, the host runs no ephemeral pager and refuses a VM with an ephemeral disk (docs/volumes.md#ephemeral-disks). Set it on every host if any VM has one, since such a VM may open on any host |
| `SPROUTFS_EPHEMERAL_ARENA_BYTES` | unset | `268435456` | the ephemeral pager's share of the HugeTLB pool, in whole 2 MiB pages, read only where `SPROUTFS_EPHEMERAL_BYTES` is set. The default memory allotment grows by it |
| `SPROUTFS_SPILL_BYTES` | literal | `17179869184` | the host's spill store in the `emptyDir`, divided by the same share into one file per pager and allocated whole at start. Each file bounds that pager's dirty pages |
| `SPROUTFS_CACHE_CLUSTER_PERCENT` | literal | `0` | the share of windows, 0 to 100, the cluster cache is on for, chosen by a hash of the window. A window inside it is kept as the stripes the membership ranks this host's disk for, under the membership's code; every other window is kept whole. The manifest sets 0; a deployment can raise it a share at a time while watching `cache_read` ([hosting](../docs/hosting.md#the-code)). A host given this above 0 and `SPROUTFS_HOT_TIER` refuses to start |
| `SPROUTFS_HOT_TIER` | unset | | a second bucket URL that every read of a checkpoint object tries before `SPROUTFS_BUCKET`, filled by reads and publications: `gs://bucket/prefix` or `s3://bucket/prefix`, with an `endpoint` query parameter for an emulator or S3-compatible server. Off by default. A host given it and `SPROUTFS_CACHE_CLUSTER_PERCENT` above 0 refuses to start. The regional bucket stays the only durable copy, and nothing expires the hot tier yet ([hosting](../docs/hosting.md#reading-through-a-hot-tier)) |
| `SPROUTFS_DISK_FREE_PERCENT`, `SPROUTFS_DISK_USED_BYTES` | literal | `20`, `60129542144` | two disk limiter goals for the filesystem under `SPROUTFS_SCRATCH_DIR`: a fifth kept free, and at most 56 GiB used by each host. The free share is above the kubelet's eviction marks and its image collection mark, 85 % used. The used goal splits the 193 GiB filesystem between the two pods. That leaves about 37 GiB for the system, k3s, images and the build: 193 less 38.6 kept free, 112 for the two hosts, the 1 GiB reserve and up to 4 GiB of band. The image has no reserved blocks. On 2026-10-03 everything else held 21 GiB (docs/measurements/gce-deploy-cache-2026-10-03.md). Only the limiter caps the page cache's disk ([hosting](../docs/hosting.md#budgets)) |
| `SPROUTFS_DISK_FREE_BYTES` | unset | | the third goal, bytes kept free. Each goal is optional and every one set is kept; with none, the host keeps a tenth free. A host whose promises the filesystem could never keep refuses to start; one that does not fit only because other writers hold space starts unready |
| `SPROUTFS_DISK_RESERVE_BYTES` | unset | `1073741824` | what the cache leaves free above the floor for promises not yet made, so a VM can start or arrive however full the other pod's cache is |
| `SPROUTFS_DISK_BAND_BYTES` | unset | `4294967296` | the most the limiter keeps the cache above the floor and reserve: a fifth of the headroom, at most this |
| `SPROUTFS_CACHE_WRITE_BYTES_PER_DAY` | literal | `1099511627776` | the disk cache's write budget, 1 TiB a day, measured by the block device counter under `SPROUTFS_SCRATCH_DIR`. A host that cannot read the counter refuses a budget. The node's persistent disk has no rated endurance, so this is a ceiling, about five disk writes a day. A node with a local SSD sets a share of its rated endurance |
| `SPROUTFS_CACHE_WRITE_BURST_BYTES` | unset | an hour's average | how far ahead of its average the budget may run |
| `SPROUTFS_RAM_LOGICAL_PAGES`, `SPROUTFS_PMEM_LOGICAL_PAGES` | unset | that pager's arena pages × 32 | bounds each pager's per-memory-region metadata, including never-faulted pages, and so the VMs a host will start (see below). Each is in its own pager's page |
| `SPROUTFS_RAM_DIRTY_PAGES`, `SPROUTFS_PMEM_DIRTY_PAGES` | unset | that pager's spill share, or its logical cap where that is smaller | bounds private state in RAM and spill together, per pager; at most its logical cap and its spill share |
| `SPROUTFS_TEMPLATES` | literal | `alpine=…/guest.ext4,workload=…/workload.ext4:2147483648` | the guest images a VM can be created from, as `name=path` pairs, or `none` for a host that creates only from templates imported on request. `name=path:bytes` sets the RAM its VMs get. A create that names none takes the only one |
| `SPROUTFS_VM_MEMORY_BYTES` | literal | `536870912` | the RAM of a VM whose template names no size, a whole number of 2 MiB pages. It is read when an image is imported, once per deployment, so changing it does not resize VMs from an image already imported; use a create's `memory` or a cold start's `--memory` |
| `GOMEMLIMIT` | literal | `6GiB` | Go's ceiling, under the container's, so the collector runs before the cgroup kills the process |
| `SPROUTFS_VM_VCPUS` | literal | `1` | the processors of a VM that records none |
| `SPROUTFS_CHECKPOINT_INTERVAL` | literal | `60s` | how often every VM on this host is checkpointed, which bounds what killing the pod rewinds a guest by. At least 1 s, or negative to disable the loop |
| `SPROUTFS_LOSS_WINDOW` | unset | `5m` | how long a VM may hold a write no landed checkpoint covers. Past it the pager admits no further dirty page for that VM: stores that need a dirty reservation wait, and a checkpoint is requested out of turn. Lost writes then span at most this window plus one checkpoint attempt's pause. At least `SPROUTFS_CHECKPOINT_INTERVAL`, or `0` to disable |
| `SPROUTFS_BOOT_ARGS` | unset | `console=ttyS0 reboot=k panic=1 i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd init=/init rootfstype=ext4 rootflags=dax=always` | the guest kernel command line of a cold boot. It must carry `rootflags=dax=always` or the host refuses to start; without it the guest keeps a page cache of pages the host already shares. The `i8042` flags stop the kernel probing a controller the guest has only for `reboot=k`, which costs half a second per cold boot; keep them in a custom line. The root device is the PMEM device declared root |
| `SPROUTFS_FIRECRACKER` | unset | `/usr/local/bin/firecracker` | the VMM in the image |
| `SPROUTFS_SECCOMP` | unset | `/usr/share/sproutfs/seccomp.bpf` | its compiled policy, including the memory-worker filter |
| `SPROUTFS_KERNEL` | unset | `/usr/share/sproutfs/vmlinux` | the guest kernel a cold boot uses. It must have `CONFIG_VMGENID=y` or the host refuses to start: otherwise every child of a fork point and every restore would draw the same random bytes. On x86_64 the boot arguments must not carry `acpi=off` or `acpi=ht`, which hide the device; see [a new generation and the right clock](../docs/vm-memory.md#a-new-generation-and-the-right-clock) |
| `SPROUTFS_VMM_JAIL` | unset | `$SPROUTFS_SCRATCH_DIR/jail` | where the host jails every VMM, as Firecracker's jailer does: built afresh at first start with a copy of the VMM, its seccomp policy and the kernel, and a tmpfs `/dev` holding `kvm` and `userfaultfd` for the VMMs' group. Each VMM runs chrooted in its own directory as its own user, so it cannot reopen a descriptor it holds read-only, reach `/proc`, or signal, trace or read another. Needs the host to run as root (the pod is privileged) and a static VMM. `none` runs every VMM unjailed as the host's user, which isolates no VM from another's VMM; the host logs this at startup |
| `SPROUTFS_VMM_FIRST_UID`, `SPROUTFS_VMM_USERS` | unset | `100000`, `1024` | the users jailed VMMs run as, one each, which bounds how many VMMs run at once |
| `SPROUTFS_VMM_GID` | unset | `100000` | the group every jailed VMM runs in, which alone may open the jail's devices |
| `SPROUTFS_ORCHESTRATOR_URL` | literal | `http://sproutfs-orchestrator.sproutfs.svc:8080` | the orchestrator's Service, where a drain asks where to put its VMs and reports on each |
| `SPROUTFS_ORCHESTRATOR` | unset | the same, from `SPROUTFS_NAMESPACE` | the older name, for a host started by hand. `SPROUTFS_ORCHESTRATOR_URL` wins |
| `SPROUTFS_GCS_ENDPOINT` | unset | | a GCS emulator to use instead of the ambient Google credentials |
| `SPROUTFS_OBJECT_STORE` | unset | `gcs` | the object store provider, `gcs` or `s3`. S3 uses the ambient AWS configuration |
| `SPROUTFS_S3_ENDPOINT` | unset | | an S3-compatible server to use instead of S3, addressed by path |
| `SPROUTFS_POD_IP` | downward API `status.podIP` | | the address the host advertises for its API and peer server |
| `SPROUTFS_POD_NAME` | downward API `metadata.name` | | the host's name to the orchestrator and operators. Nothing durable is named after it |
| `SPROUTFS_NAMESPACE` | downward API `metadata.namespace` | `sproutfs` | |
| `SPROUTFS_SHARDS` | ConfigMap `sproutfs-demo` key `shards`, optional | unset | `gce` keeps the cache on shards, network disks the orchestrator attaches to this node, instead of a file in `SPROUTFS_CACHE_DIR`. Unset, the host keeps its own cache disk. See [Shards](#shards) |
| `SPROUTFS_NODE_NAME` | downward API `spec.nodeName` | | the node, which is the Compute Engine instance a shard is attached to; read only with `SPROUTFS_SHARDS` |
| `SPROUTFS_SHARD_DEVICE_DIR` | literal | `/host/dev/disk/by-id` | where the node names its disks, with the node's `/dev` mounted at `/host/dev`: a shard attached after the pod started does not appear in the container's `/dev`. Unset, `/dev/disk/by-id` |

#### Two pagers, and how the budgets are divided

A host runs one pager for its guests' RAM and one for their PMEM disks, each
with its own arena, spill file and page. Both pages are 2 MiB by default, with
the arena a `MFD_HUGETLB` memfd from the node's 2 MiB pool. A deployment may run
RAM at 4 KiB instead, on an ordinary memfd from the pod's memory, where a store
copies 4 KiB rather than 2 MiB.

One share divides all the byte budgets: RAM gets the same fraction of the
arena, the spill file and the budgets. PMEM takes the remainder, so the two
come to what the host was given. Page counts are never added across
pagers, because their pages may differ in size. Host-wide numbers
(`SPROUTFS_MEMORY_BYTES`, the arena total, `GET /status`) are in bytes, and
per-pager numbers are reported under `pager.ram` and `pager.pmem` and with a
`kind` label on every pager metric.

#### How many VMs the logical caps admit

A logical cap is charged one memory region at a time, at attachment, against
the pager of that region's kind. It reserves nothing, since an untouched page
has no metadata. A host refuses a VM that would exceed either cap before
starting anything, rather than starting its VMM and failing part way through a
restore. `GET /status` reports what each cap has left under
`logical_pages_free`.

For this deployment, each column in its own pager's page:

| | RAM pages (2 MiB) | PMEM pages (2 MiB) |
| --- | --- | --- |
| arena, 5 GiB at the default 75/25 share | 1,920 | 640 |
| a workload VM's RAM, 2 GiB | 1,024 | |
| its root, the 5 GiB image | | 2,560 |
| the workload run's VMs, all on one host because every fork lands on its parent's: the base, its two forks and their two | 5 | 5 |
| **what that run charges** | **5,120** | **12,800** |
| arena × 32 | 61,440 | 20,480 |

A deployment with larger guests sets `SPROUTFS_RAM_LOGICAL_PAGES` and
`SPROUTFS_PMEM_LOGICAL_PAGES` the same way: the VMs one host holds times each
one's RAM, and times each one's disks.

The host chooses the pagers' other bounds from the arenas and the node, and
logs them per pager at startup:

- read-ahead: an 8 MiB run, converted to each pager's pages (four at 2 MiB,
  2,048 at 4 KiB);
- write-ahead: the same 8 MiB, or one page where that pager's dirty budget
  holds fewer than 64 runs;
- concurrent page I/O: four permits per processor, between 16 and 256, and
  never more read-ahead runs than the pager's arena has room for;
- one session serves two faults per processor, between 8 and 64;
- a VMM's mappings are admitted against half of the node's
  `/proc/sys/vm/max_map_count`, with the budget disabled below 128.

A node should allow 1,048,576 mappings (`vm.max_map_count`), which current
distributions set. A RAM pager maps a scattered store as its own mapping until
that budget refuses one, then copies instead, so at the kernel's 65,530 every
guest writing scattered pages pays in copies. A node tuned low enough loses the
budget entirely, which the host logs. docs/vm-memory.md has the reasoning.

Credentials are not in the environment: the pod reaches GCS through the VM's
metadata server, whose service account has `roles/storage.objectAdmin` on that
one bucket. The demo has no key file.

Hosts do not authenticate one another: `sproutfs-host` uses the default plain
TCP [transport](../docs/hosting.md#transport). A peer server serves any peer
that reaches its port, and a destination dials the source's peer-server address
from the handoff. The NetworkPolicy restricts that port to the host pods.

### `sproutfs-orchestrator`

| Variable | Source | Value | Meaning |
| -------- | ------ | ----- | ------- |
| `SPROUTFS_BUCKET` | ConfigMap `sproutfs-demo` key `bucket` | `sproutfs-demo-<project>` | where it lists control records to find VMs |
| `SPROUTFS_PREFIX` | ConfigMap `sproutfs-demo` key `prefix` | `demo` | |
| `SPROUTFS_API_PORT` | literal | `8080` | port for the orchestrator API |
| `SPROUTFS_API_TOKEN` | Secret `sproutfs-api-token` key `token` | generated per deployment | the shared bearer token: required by its API and presented to the hosts |
| `SPROUTFS_NAMESPACE` | downward API `metadata.namespace` | `sproutfs` | the namespace it lists pods in |
| `SPROUTFS_HOST_SELECTOR` | literal | `app.kubernetes.io/name=sproutfs-host` | label selector that finds host pods |
| `SPROUTFS_HOST_API_PORT` | literal | `8080` | port it calls on a host pod |
| `SPROUTFS_HOST_PAGE_SERVER_PORT` | literal | `8081` | port it names when it tells one host to migrate to another |
| `SPROUTFS_TABLE_PATH` | literal | `/var/lib/sproutfs/orchestrator.db` | the SQLite VM table, on a `hostPath` under `/opt/sproutfs-demo/orchestrator`, so a restarted pod remembers a migration in flight. It is rebuilt from a survey at startup; losing it loses only a VM's RAM after a resizing cold start and the pull mark of a VM nothing runs. The Deployment uses `strategy: Recreate` and one replica, so one process writes it |
| `SPROUTFS_CACHE_CODE` | unset | `4+2` | the code of the hosts' disk caches, `k+m`, set for the size the cluster usually runs at. It never follows the number of hosts. The orchestrator writes it in [the membership](../docs/hosting.md#the-membership) |
| `SPROUTFS_CACHE_EARLIER_CODES` | unset | | the codes used before `SPROUTFS_CACHE_CODE`, newest first, comma-separated, at most three. Written in the membership; hosts still read a window stored under one until it ages out. See [the code](../docs/hosting.md#the-code) |
| `SPROUTFS_SHARDS` | ConfigMap `sproutfs-demo` key `shards`, optional | unset | `gce` has the orchestrator list the shards from their claims, assign them over the hosts in the membership, and attach and detach them through Compute Engine's API. See [Shards](#shards) |
| `SPROUTFS_SHARD_CLAIMS` | literal | `app.kubernetes.io/component=sproutfs-shard` | the label selector of the shards' claims in the namespace |
| `SPROUTFS_GCS_ENDPOINT` | unset | | a GCS emulator to use instead of the ambient Google credentials |
| `SPROUTFS_OBJECT_STORE` | unset | `gcs` | the object store provider, `gcs` or `s3` |
| `SPROUTFS_S3_ENDPOINT` | unset | | an S3-compatible server to use instead of S3 |

It authenticates to the Kubernetes API with its mounted ServiceAccount token.
Its Role allows `get`, `list`, `watch` and `delete` on pods in `sproutfs`, and
`get` and `list` on its claims; a ClusterRole allows `get` on
PersistentVolumes, which name a claim's disk.

## Shards

A deployment may keep the cluster's cache on shards: a fixed set of network
disks the membership moves between hosts as compute scales, so adding and
removing machines moves no window
([hosting](../docs/hosting.md#shards-on-network-disks)). Kubernetes provisions
the disks and never attaches them. The orchestrator attaches each to the node
of the host the membership assigns it to, through Compute Engine's API, and
the host opens the device through the node's `/dev`.

On GKE, with its default Compute Engine persistent disk CSI driver:

```
kubectl apply -f deploy/
kubectl apply -f deploy/shards/00-storageclass.yaml -f deploy/shards/10-claims.yaml
kubectl -n sproutfs patch configmap sproutfs-demo --type merge -p '{"data":{"shards":"gce"}}'
kubectl -n sproutfs rollout restart deployment/sproutfs-orchestrator deployment/sproutfs-host
```

The StorageClass names the zone the hosts' node pool runs in, since a zonal
disk attaches only to an instance of its zone; edit `allowedTopologies` for
another. The orchestrator needs `compute.disks.get` and `compute.disks.use` on
the shards, and `compute.instances.get`, `compute.instances.attachDisk`,
`compute.instances.detachDisk` and `compute.zoneOperations.get` on the nodes.
On GKE, bind its ServiceAccount to a Google service account holding them
through Workload Identity. The hosts need nothing new: they are privileged and
mount the node's `/dev` at `/host/dev`.

On k3s on a Compute Engine instance, either run the CSI driver and apply the
same two files, or make the disks by hand and bind the claims with
`deploy/shards/k3s-volumes.yaml`, whose comments give the commands. The
orchestrator then uses the instance's service account, which needs the
permissions above and the `cloud-platform` or `compute-rw` scope.

The shard count is fixed when the cache is sized; the hosts scale with the
guests. With fewer hosts than shards, each host serves several shards within
one serving budget, `SPROUTFS_CACHE_SERVE_BYTES_PER_SECOND`.

### Sizing

| Setting | Rule | The manifests |
| --- | --- | --- |
| Shard count | Fixed, at least k+m, so every window's stripes are on as many disks as the code is wide; about the number of hosts at the cluster's smallest, so each host serves one or two. Changing it moves windows | 6, under 4+2 |
| Disk size | The cache holds shard count × size × k/(k+m) of windows, less a 64 MiB header region and a free region per shard. All shards the same size: a shard's weight is its size in 16 GiB steps | 256 GiB each: 1 TiB of windows under 4+2 |
| Provisioned throughput | Its host's serving budget, 500 MiB/s, shared by the shards that host serves, plus its fills. A slower disk is the slowest hop of a read | 500 MiB/s, Hyperdisk Balanced |
| Provisioned IOPS | A 2 MiB page under 4+2 is a 512 KiB stripe per disk; a 4 KiB page is about 1 KiB, read with its window's other pages; a fault reads an 8 MiB run. Throughput binds first for 2 MiB pages, IOPS for 4 KiB pages read one at a time | 6000 |
| Machine | An instance's network disk throughput depends on its machine type and vCPUs and caps all its shards together. A C3 of 4 vCPUs read 400 MiB/s from Hyperdisk Balanced provisioned for 500; 8 vCPUs allow 800. Hyperdisk Balanced attaches to C3, C4 and N4; the API refused it on N2 | c3, 8 vCPUs |

`docs/measurements/gce-shards-2026-10-04.md` measures shards on C3. A
dependent read of a 2 MiB page took 8.6 ms a hop from Hyperdisk Balanced,
7.3 ms from pd-balanced or pd-ssd and 5.5 ms from local NVMe, against 40 ms
from the store; of a 4 KiB page, 1.4, 1.0 and 0.56 ms, against 29 ms. A
persistent disk's IOPS and throughput follow its size, and on C3 it reads at
most 240 MiB/s. A shard moved off a removed host in 13 to 15 s with a
controller passing every second, about 10 s of it Compute Engine's detach and
attach calls.

## Durable flush

Durable flush is optional and off by default. On, a guest's flush is answered
once the blocks it changed are on a journal disk, not at the next checkpoint
([durable flush](../plans/fsync-journal-2026-10-06.md)). Each node a host runs
on has one journal disk. The orchestrator makes the disks through Compute
Engine's API, labelled `sproutfs-journal` with the namespace, attaches each to
its node, and deletes one that has been unused for an hour. A flush that cannot
be journaled fails with an I/O error in the guest, and the orchestrator places
no VM on a host until its journal disk is served. See
[journal disks](../docs/hosting.md#journal-disks).

```
kubectl -n sproutfs patch configmap sproutfs-demo --type merge -p '{"data":{"durable_flush":"gce"}}'
kubectl -n sproutfs rollout restart deployment/sproutfs-orchestrator deployment/sproutfs-host
```

A journal disk is 32 GiB unless `SPROUTFS_JOURNAL_BYTES` on the orchestrator
says otherwise. Besides the shards' permissions above, the orchestrator needs
`compute.disks.list`, `compute.disks.create`, `compute.disks.setLabels` and
`compute.disks.delete` in the nodes' zone. The hosts need nothing new.

## Resources

| | host (each of two pods) | orchestrator |
| --- | --- | --- |
| `hugepages-2Mi` | 6Gi request = limit | none |
| `memory` | 8Gi request = limit | 256Mi / 512Mi |
| `cpu` | 2 request, 3 limit | 200m / 1 |
| `emptyDir` | on the node disk, no size limit: the disk limiter's goals bound it | none |
| privileged | yes, plus a `hostPath` `CharDevice` on `/dev/kvm`, the node's read-only guest-image directory, and the node's cache directory | no, one `hostPath` directory for its table |

Huge pages are not counted against the container's `memory` limit, so a host
pod's ceiling on the node is 6 GiB of pages plus 8 GiB of ordinary memory. The
pages hold both arenas, 5 GiB, and the ephemeral arena's 256 MiB, with room
over; the 8 GiB holds the Go heap, the page cache and one Firecracker process
per VM. The two pods together request the node's whole 12 GiB pool and half of
its 32 GiB of memory, so there is no room for a third: the host
Deployment rolls at `maxSurge: 0` and `maxUnavailable: 1`, stopping one pod
before starting its replacement.

A host's publications also hold their fills in memory: a queue of 64 MiB
(`CacheConfig.FillQueueBytes`) and the parts waiting behind it. The host sends
their keeps at 192 MiB/s at most (`CacheConfig.FillBytesPerSecond`). On GCE
the bench's publisher of an 8 GiB guest peaked at 1.8 GiB with them. A 1 GiB
queue took it to 4.3 GiB and published no faster
([the measurement](../docs/measurements/gce-fill-defaults-2026-10-06.md)).

The preStop drain needs the other pod as a destination. Deleting both pods at
once (a `Recreate` strategy, or an eviction of both) makes every rollout a host
loss: each VM rewound to its last checkpoint and reopened by a recovery. The
PodDisruptionBudget, `maxUnavailable: 1`, says the same to a node drain or a
cluster upgrade. `scripts/lib/demo-fixes.sh` tests the rollout: it writes a
witness into a guest after its last checkpoint, restarts the hosts, and
requires the guest to still hold it.

The hosts are a Deployment because nothing durable is named after a pod. A
guest image is imported into a template, an ordinary VM whose checkpoint holds
the image, under `template-<sha256 of the image file>-<pages>`, the pages being
its RAM volume's and its root's (`2m-2m`, `2m-4k`). The first pod to want it
imports it; every other pod of the same pages finds it published. A changed
image, or a pod configured with other pages, is a new template. No pod deletes a template, since another may be forking from it, so
templates no longer used are left for a collector like other pinned
checkpoints.

A template's record with no pin is an unfinished import. A pod that finds one
waits, because opening it would take the epoch from the importing pod; after
the wait it takes the record over and imports again, so a pod that died
mid-import leaves nothing stuck.
