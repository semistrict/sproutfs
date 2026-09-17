# Demo manifests

Plain YAML for the one-node demo of
[the demo plan](../plans/demo-gce-2026-09-13.md). No kustomize, no Helm: the
whole deployment is a namespace, two Deployments, two Services, two
NetworkPolicies, a PodDisruptionBudget, and the ServiceAccount, Role and
RoleBinding the orchestrator reads the Kubernetes API with.

```
kubectl apply -f deploy/
```

`scripts/demo-gce.sh create` applies these on the demo VM. Files apply in name
order, so the namespace exists before anything in it. `deploy/testdata` holds a
probe pod the script runs by name; `kubectl apply -f` does not recurse, so it
never applies with the rest.

## What the manifests assume

Both containers come from one image, `sproutfs:demo`, with
`imagePullPolicy: Never`: the image is imported into the node's containerd
(`k3s ctr -n k8s.io images import`) and never pulled from a registry. Until
that image exists the pods stay in `ErrImageNeverPull`, which is the expected
state of a freshly created demo VM.

The node must have a 2 MiB HugeTLB pool of 12 GiB and `/dev/kvm`;
`scripts/lib/gce-demo-startup.sh` sets both up before k3s starts, so kubelet
advertises `hugepages-2Mi: 12Gi` when it first registers the node. The pool is
what it is because of the workload template: a 2 GiB guest and a few forks of it
need an arena of about 5 GiB per host to stay resident.

## What the image brings, and what the node brings

`deploy/Dockerfile` puts three fixed paths in the image, which the host binary
can rely on without configuration:

| Path | What |
| ---- | ---- |
| `/usr/local/bin/firecracker` | the static x86_64 VMM |
| `/usr/share/sproutfs/seccomp.bpf` | its compiled x86_64 seccomp policy |
| `/usr/share/sproutfs/vmlinux` | the pinned guest kernel |
| `/usr/local/bin/sproutfs-guest-agent` | the agent that runs inside a guest, carried here to be copied into the guest image rather than run from this one |
| `/usr/local/bin/sproutfs-guest-witness` | the witness that says whether a guest's memory and disk are what it wrote, carried here for the same reason and copied into both guest images |

The guest root images are not in the container image: they are built on the node
by `scripts/lib/demo-image.sh` at `/opt/sproutfs-demo/guest/guest.ext4` and
`/opt/sproutfs-demo/guest/workload.ext4`, and the host pods mount that directory
read-only at `/usr/share/sproutfs/guest` (named by `SPROUTFS_TEMPLATES`). The
mount is `DirectoryOrCreate`, so a pod still starts on a node where the build has
not run — it just finds no image there.

Two templates are registered. `alpine` is the minirootfs with an interactive
shell, which every flow of `demo-gce.sh run` uses. `workload` is that plus git,
ripgrep, Node, pnpm and three MIT-licensed TypeScript repositories whose
dependencies are in a pnpm store inside the image, which is what
`demo-gce.sh workload` measures the checkpoint model against; its VMs get 2 GiB
of RAM rather than the deployment's 512 MiB default.

## Generated configuration

`deploy/` carries the topology, which is the same everywhere. The one thing
that is not is which bucket the demo writes to, so `scripts/demo-gce.sh`
generates a ConfigMap next to these files in the copy it applies on the VM:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: sproutfs-demo
  namespace: sproutfs
data:
  bucket: sproutfs-demo-<project>
  prefix: demo
```

Applying `deploy/` on its own leaves the pods in `CreateContainerConfigError`
until that ConfigMap exists. Create it by hand for a cluster the script did not
provision.

## Ports

| Port | Name   | Workload     | What listens                                              |
| ---- | ------ | ------------ | --------------------------------------------------------- |
| 8080 | `api`  | host         | the host's HTTP API: create, open, fork, capture, console, exec, migrate-out, migrate-in, release, drain, stop, delete, status |
| 8081 | `page` | host         | the page server: memory of every VM this host has handed to another |
| 8080 | `api`  | orchestrator | the orchestrator's HTTP API, behind the ClusterIP Service |

Both container ports are named, and both Services target the names rather than
the numbers, so a port change needs an edit in one place.

## Required HTTP endpoints

The manifests depend on these; a binary that does not serve them will not run
here.

| Method | Path       | Workload | Called by | Contract |
| ------ | ---------- | -------- | --------- | -------- |
| GET    | `/healthz` | both     | readiness probe | 2xx once the process can take work. A host answers it once every configured guest image has a published template, whoever imported it, so nothing places a VM on a host that would read a whole image inside the request; until then it is not an endpoint of the headless Service. Served without the token: the kubelet reaches a pod before anything has given it one. |
| GET    | `/livez`   | both     | liveness probe | 2xx while the process can still do the work it exists for, 503 once it cannot — a host whose supervisor has closed, or whose own context has been cancelled, has released its pager and its VMM processes and can serve nothing, and a pod that answered out of the mux would be left running for ever. It is a separate endpoint from readiness so that a host doing an honest import is not restarted for failing it. Served without the token. |
| GET    | `/version` | both     | an operator | the build's `git describe`, stamped at link time. A cluster is rolled by replacing one image, so asking the pod is the only way to tell what it runs. |
| GET    | `/metrics` | host     | a scraper | the pager's occupancy, the page server's counters, the memory and cache allotments and the object-store counters per operation, in Prometheus text format. It carries the token: what a host holds is what its VMs are doing, so a scraper is configured with it like any other client and nothing in this namespace scrapes these pods on its own — the demo reads them with `kubectl exec` or a port-forward. |
| POST   | `/drain`   | host     | preStop hook | Migrate every VM this host runs to another host and return only when none are left to hand over (`Status().Serving` empty). The drain bounds itself: four at a time, 60 s per VM, 80 s overall, and the preStop command waiting on it gives up at 90 s. Kubelet waits for the hook within `terminationGracePeriodSeconds`, 150 s, which is that 90 s plus the 60 s shutdown behind it — 30 s to stop the API and 30 s for the supervisor's close, which publishes a final checkpoint of every VM that did not move. |

Draining is a POST carrying the token, not a GET: a GET is what a proxy, a link
checker or a curious operator does to every URL it is given, and this one
migrates every VM off the host. The `preStop` hook is therefore an `exec` of
`sproutfs-host drain`, which reaches this container's own API over the loopback
with the environment the server was started with, rather than an `httpGet` hook,
which can carry neither a method nor a header.

## Authentication

Every request to either API but `/healthz` and `/livez` carries the deployment's
shared bearer token, `Authorization: Bearer <token>`, and is refused with 401 without
it. One token admits the whole control plane: the orchestrator to the hosts, a
draining host to the orchestrator, and `sproutfsctl` to either. It is not
authority over any VM — a VM's authority is the epoch in its control record —
and it carries no identity: it says only that the caller is part of this
deployment.

`scripts/demo-gce.sh` generates it per deployment into the Secret
`sproutfs-api-token`, which every pod reads as `SPROUTFS_API_TOKEN`. Every side trims it, so a Secret written with a trailing newline is the same token as one without: one process that trimmed and one that did not refused every request between them with a 401 that said nothing about whitespace. A pod
without the Secret does not start, rather than serving an API that admits
anyone. `sproutfsctl` run through `kubectl exec` into the orchestrator pod
inherits it from that container's environment. A process started by hand with no
token serves an API that admits anyone and says so at startup.

`30-networkpolicy.yaml` is the other half: ingress to the host API, the page
server and the orchestrator API is restricted to pods carrying this
deployment's `app.kubernetes.io/part-of: sproutfs` label. k3s enforces these —
it ships a network-policy controller of its own alongside flannel and applies
NetworkPolicy objects by default — so on the demo cluster the token is the
second lock rather than the only one.

## The host API

Everything the host serves on port 8080. Requests and replies are JSON; a
failure is `{"op":"...","error":"..."}` with a status saying whose problem it
is — 400 for the request, 404 for a VM this host does not run, 409 for one it
already runs or cannot hand over, 503 while it is closing.

Every shape here is declared in `internal/api/host`, which is wire types and
nothing else: a handoff crossing this API is plain data the control plane
carries unread, so `sproutfsctl` and the orchestrator speak it without linking a
pager or a checkpoint store. `internal/host` converts between it and what the
host actually runs, at its own boundary.

| Method | Path | What it does |
| ------ | ---- | ------------ |
| GET | `/healthz` | readiness |
| GET | `/status` | the VMs this host runs, the handovers it still holds frames for — the VMs it migrated away and the children of every fork instant it took, wherever they landed, the pager's residency and sharing, what the page server has answered, its RAM allotment and page-cache cap, and the object-store counters — calls, failures and bytes per operation — since the process started |
| POST | `/vms` | `{"id","template"}`: fork the template's root checkpoint, boot the VM, and publish its own first checkpoint between the two. The template is the guest image in a published checkpoint, named by the image's own bytes and imported once by whichever pod of the deployment wanted it first; every create inherits it. The VM's own root is what makes it something the rest of the deployment can act on: until it is published the VM is a fork that runs here and nowhere else, so nothing could recover it and nothing could fork it. Nothing has run when it is taken, so it seals no pages and uploads none |
| POST | `/vms/{id}/open` | open a VM from the checkpoint its control record selects and resume it, which is what a host loss is recovered by. `{"cold":true}` instead discards every page of its memory and the VMM state with it, in one checkpoint, and boots the kernel from the root volume — which is exactly what the last checkpoint published, so the guest's filesystem sees a power cut after it and its journal recovers what a journal recovers. `{"memory","disk"}` give the VM its shape from there: any memory the host admits, up or down, and a root volume that may only grow, whose new pages read as zeroes for the guest's own `witness grow` to take. Both are refused without `cold`, which is the one moment nothing in memory describes the VM's shape, and a cold start is refused outright by a host with no kernel configured — before anything is discarded |
| POST | `/vms/{id}/fork` | `{"ids": [child...], "destination"?}`: seal the running VM once and hand every child of that one instant over. The reply is a handoff per child for the control plane to give its destination's `/vms/receive`, and this host holds the instant for each of them until told to release it. With a destination it serves that child's unpublished pages; without one the child comes back here, over the sealed frames themselves, and nothing of it is served. Nothing is published either way; the parent keeps running |
| POST | `/vms/{id}/capture` | take a checkpoint now; returns the pause and how long its frames took to become durable |
| GET | `/vms/{id}/console?since=N` | the serial console from byte N, out of the newest 1 MiB the host retains in memory. A read from before that starts at the oldest byte retained, which `offset` reports and `dropped` says |
| POST | `/vms/{id}/console` | `{"data":"..."}`: type into the serial console |
| POST | `/vms/{id}/exec` | `{"cmd","timeout"}`: run a shell command in the guest over the VM's vsock and return `{"exit","stdout","stderr","seconds"}`. 503 while the guest's agent is not answering, which is a VM that is still booting |
| POST | `/vms/{id}/migrate` | `{"destination":"host:port"}`: stop the guest and hand the VM over; returns the handoff the control plane carries to the destination |
| POST | `/vms/receive` | a handoff: open the VM, resume it from the captured state and stream the source's pages in. Returns once the source may release them |
| POST | `/vms/{id}/released` | stop serving a migrated VM's pages and close the process that held them. 409 while the destination has not fetched every page this host holds that no checkpoint has: those bytes exist nowhere else, and the host keeps serving them until it has answered for each one |
| POST | `/vms/{id}/abandoned` | give one handover up rather than handing it over: the control plane says this VM will never be received — a fork's child whose destination refused it, one a fan-out never offered anywhere — so whatever is still held for it goes and a fork's parent takes its sealed frames back. It refuses nothing, which is the whole difference from `released`: those pages are going either way, and a refusal would only leave the parent sealed until this host's own deadline retired the hold |
| POST | `/drain` | migrate every VM away and return when none are left to hand over. A POST rather than a GET: a GET is what a proxy or a link checker does to every URL it is given, and this one moves every VM off the host |
| POST | `/vms/{id}/stop` | publish everything the guest still holds, then close the VMM process, give the frames back and release the handle. The control record and the objects stay, so any host can open the VM again at exactly the bytes this published — which is the whole difference between a stop and losing the host, where the writes since the last checkpoint go with it. Returns the checkpoint it published, which is the instant the VM comes back at and which nothing else records. The checkpoint comes first and nothing is given up until it lands, so a publication the store refused leaves the VM running and checkpointing. 409 for a VM a fork instant holds sealed, exactly as a delete of one is refused: a child elsewhere is reading the pages no checkpoint holds out of the frames this would release |
| DELETE | `/vms/{id}` | close the VM and delete its control record. A VM this host does not run is only a record and its objects here, and those are in the bucket, so it is deleted all the same; one anything still holds sealed is refused, because a fork instant reads the frames a close would detach |

## The orchestrator API

Everything the orchestrator serves on port 8080, and what `sproutfsctl` drives.
Hosts come from the Kubernetes API and VMs from the control records in the
bucket — one key each, under `control/`, so a listing costs a page of keys and
not a walk of every checkpoint object — and from the hosts themselves. One
survey asks every host at once and waits seconds, not minutes, for each, so a
host that accepts the connection and never answers is reported with its failure
rather than holding up the deployment; a survey answers the read requests that
arrive within a second of it, which is what spares a console being polled a
fan-out per frame. It keeps one SQLite table on the node
saying which host each VM is on and what it was last asked to do with it —
creating, running, migrating between a named pair, stopped or recovering — which
is what makes a migration or a drain visible while it is happening and what a
request for one VM is routed by. There is no table of hosts: where the hosts
are is the Kubernetes API's answer and what they run is their own, both read
fresh for every request that needs them.

Nothing in the table is authority: it is rebuilt from a survey and the bucket
when the process starts and every thirty seconds after that, and a row that
disagrees with the host that answers loses. Only an operation that moves a VM
writes it — that is the one that makes it wrong — and the reconciler, which is
also what releases a handover nothing is waiting on and forgets a VM another
orchestrator deleted. A read writes nothing: a listing that rewrote the table
put this process's only SQLite writer behind every console poll.

A row that says an operation is in flight is taken at its word for two minutes
and no longer. Everything that reads the table defers to such a row — the
source's frames are left served, the reconciler leaves it where it is, and a
recovery is refused — so an operation that died with the process driving it
would otherwise hold all three open for as long as the deployment ran. Two
minutes is inside the four checkpoint intervals a host gives one handover of its
own, so a stale row stops pinning a source before that host gives the frames up
by itself.

| Method | Path | What it does |
| ------ | ---- | ------------ |
| GET | `/healthz` | readiness |
| GET | `/hosts` | every host pod, what it runs, what it still serves, its pager's residency and sharing, and why it did not answer if it did not |
| GET | `/vms` | every VM, with the host running it or none when its host is gone. A VM exists exactly while its control record does, so a deleted VM is simply absent |
| POST | `/vms` | `{"template"}`: allocate a ULID and create the VM on the host whose guests have promised the least of its arena, refusing with 503 when the template's RAM fits on none |
| POST | `/vms/{id}/fork` | `{"count", "to"?}`: fork the running VM. Every child comes from one instant, so the parent pauses once however many are asked for, and every child is handed over and received like a migration. The default is its own host, which takes its children in over the frames the seal froze; `to` places them on another host, which pulls the pages no checkpoint holds out of the parent. Either way the fan-out is admitted against the host taking it — each child is a guest with RAM of its own — before the parent is paused, and each child is released once it holds every page it inherited. Returns the children and what the fork cost |
| POST | `/vms/{id}/capture` | take a checkpoint on the host running it |
| POST | `/vms/{id}/migrate` | `{"to"}`: migrate out, receive, and only then release. An empty `to` picks the other host with the most memory free, which is what a draining host asks for; a named destination without room for the guest is refused |
| POST | `/vms/{id}/recover` | `{"force"?}`: reopen a VM on a live host that does not run it. Reopening takes the control record's epoch, which fences whatever held it, so it needs evidence the loss is real: every pod the API lists answered, and none runs the VM. Refused while a live host reports running it, while any host still serves its unpublished pages, and while the table says an operation on it is in flight — the last two are a VM between hosts rather than a lost one, and `force` does not get past them either — and refused, without `force`, while any listed pod is quiet, because a host that missed one request is not a host that is gone |
| POST | `/vms/{id}/stop` | ask the host running the VM to publish what its guest holds and close it. The VM is then exactly its control record and its objects — still listed, on no host — and the table records it `stopped`. Returns the checkpoint it published. 404 for a VM no host runs, and 409 for one two hosts claim |
| POST | `/vms/{id}/start` | `{"to"?, "cold"?, "memory"?, "disk"?}`: open a stopped VM on a host again, on the one named or on the ready host whose guests have promised the least of its arena, refusing a named one without room for the guest. It is `recover` without the evidence of a loss: a recovery has to prove the VM's host is gone, because opening takes the record's epoch and would fence a guest that is fine, and a stopped VM has no such host, so a host that did not answer this survey does not refuse it. Everything that says the VM is between hosts rather than stopped still does — a host that runs it, two hosts that claim it, a host still serving the pages no checkpoint has, and an operation still in flight in the table. `cold` brings the VM back without its memory, and `memory` and `disk` give it a new shape, both of which the host does; from a resize on, the VM's committed RAM is its own rather than its template's, and that is what every later placement of it admits against |
| GET | `/check` | run the deployment check over the whole object namespace and report every violation: the object key, what is wrong with it, and the class of host loss that could excuse it. A deployment that disagrees with itself is a 200 with a body of violations rather than a failed request — the request worked, and what is wrong is the answer — and a store this could not read is a 500. The leftovers a live deployment always has are allowed by name: a publication in flight, the lineage a deleted VM's descendants pin, a VM's own root and a template import's intermediate checkpoints, and the superseded epoch every takeover of a VM leaves behind it — a template whose unfinished import a later one recovered among them |
| POST | `/hosts/{name}/kill` | delete a host pod with no grace period, which skips the preStop drain: the demo's host loss |
| DELETE | `/vms/{id}` | close and delete the VM on its host, or on any ready host when no host runs it — a VM's record and objects are in the bucket, so removing them is work any host can do, and a VM whose host is gone would otherwise be undeletable. The record goes, which frees the identity, and then the VM's checkpoint objects — except the checkpoints it was forked at, whose objects a descendant may still read and which a collector reclaims |
| GET | `/vms/{id}/console?since=N` | the VM's console, proxied from its host |
| POST | `/vms/{id}/console` | type into the VM's console |
| POST | `/vms/{id}/exec` | `{"cmd","timeout"}`: run a shell command in the VM's guest, through the host the table names; returns `{"host","vm","result"}` |
| POST | `/drains` | `{"host","vm","phase","error"}`: a draining host reporting that it has begun handing one VM over, or finished. It changes nothing the orchestrator does — it drives the migration itself — and makes the moment visible in the table |

## The CLI

`sproutfsctl` is in the same image and speaks only that API. It reads
`SPROUTFS_ORCHESTRATOR`, `http://localhost:8080` by default, which is where a
port-forward puts the orchestrator:

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
```

`exec` stops parsing at the `--`, so the guest's own flags and quoting reach it
untouched. What the command printed goes to this process's stdout and what it
complained about to stderr, and a command that exited non-zero makes
`sproutfsctl` exit non-zero too. It reaches the guest over the VM's vsock: no
guest network exists, and a fork or a migration carries the channel without
anything being allocated or moved.

A console session ends with its input, which is what an interactive one wants
and what a scripted one does not: a command piped in is exhausted long before
the guest has answered it. `--for` keeps the session reading for a fixed time
whatever the input does, which is how `scripts/lib/demo-run.sh` asks a guest a
question and reads the answer:

```
echo 'uname -a' | sproutfsctl console vm-01j... --for 8s
```

`sproutfsctl hosts` reports each host's resident and shared frame counts and the
pages its page server has handed to another host, beside what it runs, so a fork
on the parent's own host and a fork placed elsewhere are both visible without
reading `/status`. `sproutfsctl store` reports each host's object-store
counters, one row per host and operation, so two readings subtracted are what a
stretch of work cost in object traffic. What one checkpoint of one VM cost is a
line the host logs when its publication ends, which carries the dirty pages the
pause sealed, the bytes uploaded, the objects written, and the pause and upload
durations.

## Environment

### `sproutfs-host`

| Variable | Source | Value | Meaning |
| -------- | ------ | ----- | ------- |
| `SPROUTFS_BUCKET` | ConfigMap `sproutfs-demo` key `bucket` | `sproutfs-demo-<project>` | the GCS bucket holding control records, checkpoints and pages |
| `SPROUTFS_PREFIX` | ConfigMap `sproutfs-demo` key `prefix` | `demo` | the deployment's prefix inside that bucket |
| `SPROUTFS_API_PORT` | literal | `8080` | port for the host API |
| `SPROUTFS_API_TOKEN` | Secret `sproutfs-api-token` key `token` | generated per deployment | the shared bearer token every request to either API carries. Without it the process serves an API that admits anyone and says so at startup |
| `SPROUTFS_PAGE_SERVER_PORT` | literal | `8081` | port for the page server |
| `SPROUTFS_HUGEPAGE_DIR` | literal | `/hugepages-2Mi` | the pod's hugetlbfs mount. The arena is a `MFD_HUGETLB` memfd rather than a file in it, but the mount is what the kubelet grants the pod its HugeTLB allotment through, so the host refuses to start without it |
| `SPROUTFS_SCRATCH_DIR` | literal | `/var/lib/sproutfs` | node-disk `emptyDir` for the spill file and the VMM scratch. A starting host wipes it: a restart is a host loss |
| `SPROUTFS_ARENA_BYTES` | literal | `5368709120` | the pager's resident frame store, a whole number of 2 MiB pages out of the pod's 6 GiB HugeTLB allotment |
| `SPROUTFS_MEMORY_BYTES` | literal | `8589934592` | the RAM allotment the pager takes its frames from. Defaults to the arena plus 1 GiB |
| `SPROUTFS_CACHE_BYTES` | literal | `1073741824` | the page cache's own cap, which nothing else draws on |
| `SPROUTFS_SPILL_BYTES` | literal | `17179869184` | the pager's spill file, out of the 20 GiB `emptyDir`. It is what bounds the dirty pages |
| `SPROUTFS_LOGICAL_PAGES` | unset | arena pages × 32 | bounds the pager's per-region metadata, including never-faulted pages, and so bounds the VMs a host will start at all — see the arithmetic below |
| `SPROUTFS_DIRTY_PAGES` | literal | `6144` | bounds volatile private state on RAM and spill together; at most `SPROUTFS_LOGICAL_PAGES`, and at most what `SPROUTFS_SPILL_BYTES` holds. It defaults to the smaller of the arena's pages and `SPROUTFS_SPILL_BYTES`, which a workload guest writing hundreds of megabytes between checkpoints outruns |
| `SPROUTFS_TEMPLATES` | literal | `alpine=…/guest.ext4,workload=…/workload.ext4:2147483648` | the guest images a VM can be created from, as `name=path` pairs. A pair may name the RAM its VMs get after a colon, `name=path:bytes`, which is what an image needing more than the default uses. A create request that names none takes the only one |
| `SPROUTFS_VM_MEMORY_BYTES` | literal | `536870912` | the RAM of a VM whose template names no size of its own, a whole number of 2 MiB pages. It is read when a guest image is imported, which happens once for the whole deployment, so changing it gives no new memory to a VM created from an image already imported — a cold start with `--memory` is what changes one VM's shape |
| `GOMEMLIMIT` | literal | `6GiB` | Go's own ceiling, under the container's, so the collector runs before the cgroup kills the process |
| `SPROUTFS_VM_VCPUS` | literal | `1` | one VM's processors |
| `SPROUTFS_CHECKPOINT_INTERVAL` | literal | `60s` | how often every VM this host runs is checkpointed, which bounds what killing this pod rewinds a guest by. At least 1 s — a checkpoint pauses every VM and uploads its dirty set, so anything shorter is a host spending its guests' time on the object store — or a negative value to disable the loop |
| `SPROUTFS_BOOT_ARGS` | unset | `console=ttyS0 reboot=k panic=1 init=/init rootfstype=ext4 rootflags=dax=always` | the guest kernel command line of a cold boot. The root device is the PMEM device declared root, so the line names only the filesystem and DAX |
| `SPROUTFS_FIRECRACKER` | unset | `/usr/local/bin/firecracker` | the feature-enabled VMM in the image |
| `SPROUTFS_SECCOMP` | unset | `/usr/share/sproutfs/seccomp.bpf` | its compiled policy, including the memory-worker filter |
| `SPROUTFS_KERNEL` | unset | `/usr/share/sproutfs/vmlinux` | the guest kernel a cold boot uses |
| `SPROUTFS_ORCHESTRATOR_URL` | literal | `http://sproutfs-orchestrator.sproutfs.svc:8080` | the orchestrator's Service: where a drain asks for somewhere to put its VMs and reports what it is doing with each of them |
| `SPROUTFS_ORCHESTRATOR` | unset | the same, from `SPROUTFS_NAMESPACE` | the older name, for a host started by hand. `SPROUTFS_ORCHESTRATOR_URL` wins |
| `SPROUTFS_GCS_ENDPOINT` | unset | | a GCS emulator to use instead of the ambient Google credentials, which is how the store is exercised outside GCE |
| `SPROUTFS_POD_IP` | downward API `status.podIP` | | the address the host advertises for its API and page server |
| `SPROUTFS_POD_NAME` | downward API `metadata.name` | | the host's name to the orchestrator and to an operator. Nothing durable is named after it, which is why the hosts are a Deployment |
| `SPROUTFS_NAMESPACE` | downward API `metadata.namespace` | `sproutfs` | |

#### How many VMs the logical cap admits

The logical cap is charged one region at a time, at attachment, and every
region of every VM on a host is charged against it, so it is what decides which
VMs a host will start. It reserves nothing — a page never touched has no
metadata — and a host that runs out of it would otherwise admit a VM's RAM,
start its VMM and then be refused its root, killing the guest part way through
a restore. The host refuses such a VM before starting anything, and
`GET /status` reports what the cap has left as `logical_pages_free`.

The arithmetic for this deployment, in the pager's 2 MiB pages:

| | pages |
| --- | --- |
| arena, 5 GiB | 2,560 |
| a workload VM's RAM, 2 GiB | 1,024 |
| its root, the 5 GiB image | 2,560 |
| **one workload VM** | **3,584** |
| the workload run's VMs, all on one host because every fork lands on its parent's: the base, its two forks and their two | 5 |
| **what that run charges** | **17,920** |
| arena × 8 | 20,480 |
| arena × 32 | 81,920 |

Five VMs leave 2,560 pages under eight arenas, which is one root region and no
RAM to go with it. The sixth VM on that host — a wider run, or one left over
from `demo-gce.sh run` — had its RAM admitted and its root refused, and the
guest died part way through its restore. Thirty-two arenas is twenty-two
workload VMs on one host, well past what the arena and the placement allow. A
deployment with larger guests sets `SPROUTFS_LOGICAL_PAGES` from the same
arithmetic: the VMs one host holds, times each one's RAM plus root in 2 MiB
pages.

The pager's remaining bounds are not environment variables: the host chooses
them from the arena above and the node it lands on, and logs every one of them
when it assembles. Read-ahead is four 2 MiB pages; write-ahead is the same four,
or one where `SPROUTFS_DIRTY_PAGES` holds fewer than 64 such runs; concurrent
page I/O is four permits per processor, held between 16 and 256 and never more
read-ahead runs than the arena has room for; one session serves two faults per
processor, between 8 and 64; and a VMM's mappings are admitted against half of
the node's `/proc/sys/vm/max_map_count`, with the budget disabled below 128.
A node whose `vm.max_map_count` is the kernel default is fine; one tuned down
far enough loses the budget rather than gaining a tighter one, which the host
says in its log. docs/vm-memory.md carries the reasoning.

Credentials are not in the environment: the pod reaches GCS through the VM's
metadata server, whose service account has `roles/storage.objectAdmin` on that
one bucket. No key file exists anywhere in the demo.

Hosts do not authenticate one another. A page server serves any peer that
reaches its port, and a destination dials the source's page-server address
straight out of the handoff the orchestrator carried. Keeping that port to the
host pods is the cluster's NetworkPolicy, not the process's.

Guest images are not in the container image. `scripts/lib/demo-image.sh` builds
one on the node under `/opt/sproutfs-demo/guest`, which the host pod mounts
read-only at `/usr/share/sproutfs/guest`.

### `sproutfs-orchestrator`

| Variable | Source | Value | Meaning |
| -------- | ------ | ----- | ------- |
| `SPROUTFS_BUCKET` | ConfigMap `sproutfs-demo` key `bucket` | `sproutfs-demo-<project>` | where it lists control records to find VMs |
| `SPROUTFS_PREFIX` | ConfigMap `sproutfs-demo` key `prefix` | `demo` | |
| `SPROUTFS_API_PORT` | literal | `8080` | port for the orchestrator API |
| `SPROUTFS_API_TOKEN` | Secret `sproutfs-api-token` key `token` | generated per deployment | the same shared bearer token the hosts carry: it is what the orchestrator's own API demands and what it presents when it calls a host |
| `SPROUTFS_NAMESPACE` | downward API `metadata.namespace` | `sproutfs` | the namespace it lists pods in |
| `SPROUTFS_HOST_SELECTOR` | literal | `app.kubernetes.io/name=sproutfs-host` | label selector that finds host pods |
| `SPROUTFS_HOST_API_PORT` | literal | `8080` | port it calls on a host pod |
| `SPROUTFS_HOST_PAGE_SERVER_PORT` | literal | `8081` | port it names when it tells one host to migrate to another |
| `SPROUTFS_TABLE_PATH` | literal | `/var/lib/sproutfs/orchestrator.db` | the SQLite file holding the VM table, on a `hostPath` under `/opt/sproutfs-demo/orchestrator` so that a restarted pod does not forget a migration that was in flight. It is rebuilt from a survey at startup, so losing it costs nothing. The orchestrator Deployment uses `strategy: Recreate` and one replica: one process writes this file |
| `SPROUTFS_GCS_ENDPOINT` | unset | | a GCS emulator to use instead of the ambient Google credentials |

It authenticates to the Kubernetes API with its ServiceAccount token, mounted
as usual. Its Role allows `get`, `list`, `watch` and `delete` on pods in
`sproutfs` and nothing else — enough to place VMs and to run the kill-host
flow, and not enough to touch anything outside the namespace.

## Resources

| | host (each of two pods) | orchestrator |
| --- | --- | --- |
| `hugepages-2Mi` | 6Gi request = limit | none |
| `memory` | 8Gi request = limit | 256Mi / 512Mi |
| `cpu` | 2 request, 3 limit | 200m / 1 |
| `emptyDir` | 20Gi on the node disk | none |
| privileged | yes, plus a `hostPath` `CharDevice` on `/dev/kvm` and the node's read-only guest-image directory | no, one `hostPath` directory for its table |

Huge pages are not counted against the container's `memory` limit, so a host
pod's ceiling on the node is 6 GiB of pages plus 8 GiB of ordinary memory. The
two pods together request the node's whole 12 GiB pool, so there is no room for
a third: the host Deployment rolls at `maxSurge: 0` and `maxUnavailable: 1`,
taking a pod down before it starts its replacement and leaving the other pod up
while it does.

That other pod is what the preStop drain needs: a destination to migrate its VMs
to. Deleting both pods at once — a `Recreate` strategy, or an eviction that took
them together — leaves the drain nowhere to put anything and makes every rollout
a host loss, each VM rewound to its last checkpoint and reopened by a recovery,
which is exactly what the drain exists to avoid. The PodDisruptionBudget,
`maxUnavailable: 1`, is what says the same thing to a node drain or a cluster
upgrade. `scripts/lib/demo-fixes.sh` exercises the rollout end to end: it writes
a witness into a guest after its last checkpoint, restarts the hosts, and
requires the guest to still hold it.

The hosts are a Deployment, and their pod names are the Deployment's to choose,
because nothing durable is named after one. A guest image is imported into a
template — an ordinary VM whose checkpoint holds the image, so that creating a
VM is a fork of it — under `template-<sha256 of the image file>`. That is one
name for one image across the whole deployment: the first pod to want it imports
it, every other pod and every later pod finds it published and opens nothing,
and an image that changed under its name is a template of its own rather than
the same one holding other bytes. A pod deletes no template — another pod may be
forking from it — so the templates of images nothing creates from any more are a
collector's, like every other pinned lineage.

A template's record with no pin is an import that has not finished. A pod that
finds one waits for it, because opening it would take the epoch out from under
the pod doing the importing; past that wait it takes the record over and imports
again under it, which is how a pod that died mid-import leaves nothing wedged.
`SPROUTFS_POD_NAME` is still how a host reports itself to the orchestrator and
how an operator addresses it, and it is no longer in anything the bucket holds.
