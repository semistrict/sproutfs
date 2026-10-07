# Disk pages of 2 MiB against 4 KiB under a build — 2026-10-07

**Question.** How much would storing disk pages at 4 KiB save over 2 MiB? The
journal plan for durable fsync (TASK-104) finds changed 4 KiB blocks inside
2 MiB pages, which raised whether checkpoints should store 4 KiB too.

**Answer for this workload: little.** At 4 KiB the disks' dirty bytes were
about a fifth smaller, and the bytes uploaded were the same.

## Setup

- One GCE demo VM (`scripts/demo-gce.sh`, n2-standard-8, us-east4-a), two host
  pods, main at 2fa9708a with cc70685a's fix to the workload script.
- `scripts/demo-gce.sh workload` with `FORKS_BASE=0 FORKS_PER_REPO=0`: one
  workload guest (2 GiB, Alpine with git, ripgrep, Node, pnpm and three
  repositories pinned to h3 `aa50e96`, unstorage `7f773be`, ofetch `1dbc37f`)
  runs ripgrep, git status and log, an offline `pnpm install` in each
  repository, `pnpm build` in h3, then 75 s idle. Each phase ends with a capture.
- Run twice: once at the default 2 MiB disk page, once with
  `SPROUTFS_DEMO_PMEM_PAGE_BYTES=4096`. RAM is 2 MiB in both.
- The forks of the default workload no longer fit: placement admits each
  fork's whole RAM, and the deployed RAM arena (3.75 GiB) holds one 2 GiB guest.

## Results

The capture at the end of each phase publishes RAM and disks together, and the
checkpoint log does not split them. In the 4 KiB run the two page sizes differ,
so each checkpoint's dirty bytes and dirty pages give the RAM part (2 MiB
pages) and the disk part (4 KiB pages) exactly. The 2 MiB run's disk part is
its total less the 4 KiB run's RAM for the same phase. That assumes the guest
dirtied the same RAM in both runs, so a single phase's figure is rough; the
total is less so.

| phase | disk dirty at 2 MiB (estimate) | disk dirty at 4 KiB |
| --- | --- | --- |
| boot | 2 MiB | 8.0 MiB |
| search | 0 | 0 |
| history | 8 MiB | 16.0 MiB |
| install | 168 MiB | 132.3 MiB |
| build | 30 MiB | 8.5 MiB |
| idle (interval checkpoints) | 42 MiB | 35.3 MiB |
| **total** | **248 MiB** | **200 MiB** |

Bytes uploaded over the run: 232.3 MiB at 2 MiB, 232.5 MiB at 4 KiB, in 23 and
27 PUTs. RAM is most of them, and the disks' unchanged bytes inside a dirty
2 MiB page compress, as the 2026-09-14 run found.

## What it shows

- **A build writes whole files.** `pnpm install` and `pnpm build` write new
  files from start to end, so a dirty 2 MiB disk page is mostly changed bytes.
  4 KiB pages saved 1.2 times the dirty bytes over the run.
- **Compression already takes the rest.** The uploads did not differ.
- **Where 4 KiB would pay is not measured.** A database that rewrites 4 KiB
  pages scattered over a large file dirties a 2 MiB page per write. None of the
  guest images holds a database. Adding one (SQLite or PostgreSQL from
  Alpine's packages) is the next measurement before any change to the storage
  format.

All instances, disks and the bucket were deleted, and the deletion verified.
