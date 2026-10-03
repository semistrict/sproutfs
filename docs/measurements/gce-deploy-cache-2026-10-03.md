# The page cache's disk on a demo node, GCE, 2026-10-03

The first run on a real cluster of step 10's deployment half of
[the disk cache plan](../../plans/disk-cache-2026-10-02.md) (TASK-81). The
page cache's file is in a `hostPath` on the node, each host takes the first
`cache-N` no other process has locked, disk goals replace the per-concern
caps, and the demo's boot disk is 200 GB.

## What ran

`scripts/demo-gce.sh create` made one disposable `n2-standard-8` node,
`sproutfs-demo-cache1003`, in us-east4-a, with its own bucket. It ran k3s, the
two host pods and the orchestrator from `deploy/`. The source was 74ed7f27
with the `/status` and `/metrics` change below; the stop fix below was
deployed later with `redeploy`. The node, its disk and the bucket were deleted
afterwards, and the script checked that none remained. Raw output is in
`gce-deploy-cache-2026-10-03/`.

Five things were checked:

1. Both host pods start and become ready, and `/status` shows the disk
   limiter's goals, the cache's share, the reserve, the cache's identity and
   weight, and the list of caches.
2. The cache files are under `/opt/sproutfs-demo/cache/<namespace>/` on the
   node, one per host pod, each locked by its pod.
3. A VM created with `--pull` fills the cache. After its host pod is deleted,
   the replacement takes back the same file and identity, reads its regions
   back from their tables, and the VM's pages come from the disk, not the
   store.
4. The host reads the device's write counter from inside the pod.
5. How much of the disk the system, the images and other users take, and how
   much the limiter gives each cache.

## Results

### 1. Pods, status

Both host pods were ready about 30 s after the apply. Each logged its limiter
at start:

| | Value |
| --- | --- |
| goals | free 20 %, used 60,129,542,144 bytes (56 GiB) |
| filesystem | 207,071,854,592 bytes (192.9 GiB) |
| floor | 41,414,370,918 bytes (38.6 GiB) |
| reserve | 1 GiB |
| band | 4 GiB |
| promises | 15,032,397,824 bytes: RAM spill 9 GiB, PMEM spill 3 GiB, ephemeral spill 2 GiB |
| cache's share | 45,097,144,320 bytes (42 GiB), bound by the used goal |
| write budget | 1 TiB a day, burst 45,812,984,490 bytes |

Each pod's `/status` had a `cache` (identity, weight 3, its page address) and
a `caches` list of both caches under 1+1, read every ten seconds (66 reads,
1 failure while the orchestrator was starting). The weight is 3 because the
share the host is given at start, 42 GiB, is 2.6 steps of 16 GiB.

### 2. The files on the node

```
/opt/sproutfs-demo/cache/sproutfs/cache-0  inode 762154  FLOCK WRITE pid 39416 sproutfs-host  pod 0069a887…
/opt/sproutfs-demo/cache/sproutfs/cache-1  inode 762157  FLOCK WRITE pid 39426 sproutfs-host  pod da2c7351…
```

The pid's cgroup names each pod. Inside both pods `/var/cache/sproutfs` is a
bind of `/opt/sproutfs-demo/cache/sproutfs`, and `/var/lib/sproutfs` a bind of
the pod's `emptyDir`, both on device 8:1. The kubelet made the `sproutfs`
directory from `subPathExpr: $(SPROUTFS_NAMESPACE)`.

### 3. A pulled VM across the deletion of its host pod

An `alpine` VM was created with `--pull`, wrote 64 MiB of random bytes into
its tmpfs, and was stopped with `--suspend`. Its host pod was deleted with
`kubectl delete pod`, and the VM was started again on the replacement with
`start --to`.

The first run found a bug. The stop's checkpoint, 79 MB uploaded, was not kept
on the disk: the cache's admitted bytes did not move across the stop. A stop
ends the VM's machine before it publishes, and the machine's end closed the
pull, so the pull kept nothing of the stop's publication. The VM started on
the replacement read those 79 MB from the store: 57 get calls and 100,572,013
bytes, against 49 disk hits.

The fix ends a pull's copying with the machine and its keeping with the VM's
handle, after its last publication. Run again with the fix deployed:

| Step | Disk entries | Admitted bytes | Disk hits | Store gets |
| --- | ---: | ---: | ---: | ---: |
| pull done on the first pod (`cache-0`) | 130 | 12,149,042 | 47 | 42 calls |
| after `stop --suspend` | 212 | 91,108,096 | 47 | 44 calls |
| replacement pod, before the start | 212 | 0 | 0 | 4 calls, 3,106 bytes |
| VM started and every page read | 212 | 0 | 82 | 10 calls, 9,058 bytes |

The replacement logged:

```
host: the page cache's disk was claimed  file=cache-0
checkpoint: the page cache's disk was read back  identity=46ee965edb495c012dda6ee2c7b32d0a
    regions=5 from_tables=5 scanned=0 given_back=0 items=212
```

It took back `cache-0`, with the identity the first pod drew when it made the
file. All five regions came back from their tables: an orderly shutdown closes
the open region. The VM resumed from the stop's checkpoint and its witness
hashed as it did before the stop. The guest's pages, 91 MB as the pull counts
them, came from the disk; the store served only control records and indexes,
9 KB in all. The pull on the replacement found every page on the disk and
wrote nothing.

The demo's rolling restart for the redeploy replaced both pods one at a time.
The new pods took back `cache-0` and `cache-1` with their identities, from
3 and 2 tables.

### 4. The device's write counter

The pod reads `/sys/dev/block/8:1/stat`, the boot disk's root partition, the
same file as the node. `sproutfs_disk_device_written_bytes_total` was
1,544,343,552 one minute after start, mostly the two pods' spill files. The
write budget sees the device.

### 5. Disk accounting

The boot disk is 200 GiB, since GCE sizes disks in GiB: 214,748,364,800
bytes. The root filesystem is 192.9 GiB, with no reserved blocks. After the
first deploy it held 49.4 GiB:

| User | GiB |
| --- | ---: |
| two hosts' spill files, allocated whole | 28.0 |
| Docker's store under `/var/lib/containerd`: the build's images | 9.9 |
| guest images, `guest.ext4` and `workload.ext4` | 7.0 |
| `/usr` | 2.2 |
| k3s and its containerd | 0.9 |
| snapd, caches, logs and the rest | 1.6 |

Everything but the hosts held 21.4 GiB. One redeploy added about 2 GB to Docker's
store: the old image stays behind.

The limiter gives each host a share of 42 GiB, the used goal less 14 GiB of
promises. The used goal binds while the filesystem keeps free its floor, the
reserve and the band above what the hosts hold. With both hosts at their goal,
everything else may hold 192.9 − 38.6 − 112 − 1 − 4 = 37.3 GiB before the
caches start to shrink. The manifest's estimate of "about 45 GiB" was high;
the manifest and `deploy/README.md` now give 37 GiB. The node had 16 GiB of
that to spare.

## What changed

- **A stop's checkpoint is kept on a pulled VM's disk.**
  `checkpoint.Pull.StopFetching` stops the copy and leaves the keeping on.
  The host calls it when the machine ends, and `volume.VM.Close` closes the
  pull after the last publication. A second `Pull` closes the one it
  replaces. Test: `TestAPulledVMKeepsItsStopsCheckpointOnTheDisk`. It stores
  into two pages, stops the VM, opens it again on the same host and reads
  every page with no request of the store. Guard
  `host-pull-closed-with-the-machine` puts the old order back, and the test
  then fails with 2 requests.
- **`/status` and `/metrics` report the page cache's disk.** `cache_disk` names
  the claimed file, the identity, the regions and entries, the hits, the lost
  copies, the regions given back, and what the host read back at start.
  `sproutfs_cache_disk_*` carries the same counters. Before this change no
  counter showed a disk hit or a read-back. Tests: the restart test checks the
  report, and a metrics test checks the exposition.
- The host test harness runs its hosts under the simulation's runtime, so a
  guard read from the host's own context can be turned on.
- The disk estimate in `deploy/10-host.yaml` and `deploy/README.md` is
  corrected, and `docs/hosting.md` says when a pull's copying and keeping
  end.

## Not checked

- A host loss (`kill-host`, no grace period) over a cache file. The open
  region is then scanned or given back; the simulation covers that, and the
  node did not.
- A cache that fills to its share, and the two caches pressing on each other
  through the reserve and the band. The run held 335 MB.
- Docker's store growing with each redeploy. Nothing removes old images;
  across many redeploys that eats the 16 GiB of slack.
- The repeated SSH resets while a host pod started (three times). The
  commands run on the node through `nohup` were not affected. The cause was
  not found.
