---
id: TASK-73
title: >-
  Export the metrics an operator needs to explain a stall, a slow fault and a
  failed checkpoint
status: In Progress
assignee:
  - '@claude'
created_date: '2026-09-30 08:29'
updated_date: '2026-09-30 08:38'
labels:
  - embedder
  - observability
dependencies: []
references:
  - api/host/metrics.go
  - vmmemory/stats.go
  - host/interval.go
  - docs/measurements/gce-backlog-2026-09-30.md
priority: medium
type: enhancement
ordinal: 80000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
`/metrics` (hostapi.Metrics) exports pager gauges and counters, the widest loss window, page-server counts, budgets and object-store calls, failures and bytes. It cannot say why a guest is stalled, how slow faults are, or why checkpoints fail.

Missing, by value:
1. Stall causes the pager already counts but does not export: DirtyWaits, DirtyStalls, CheckpointRequests, WindowWaits, WindowStalls, RepeatedFaults, PacedFaults, RefusedMappings (vmmemory.Stats).
2. Latency histograms the pager already keeps (Fault, Load, Seal, Mapping, Revoke) as Prometheus histograms. The hand-written exposition has no histogram type yet. The 2026-09-30 GCE fan-out showed an isolated-arena fault costs 60% more than a shared one; this is where that would show.
3. Interval checkpoint outcomes: attempts, failures by reason, pause and upload duration, bytes uploaded. None of this is in Status today.
4. Lifecycle counters: migrations, forks and receives with outcome and pause, VMM deaths, fenced VMs, pager-ordered stops.
5. Object-store latency per operation, and S3 conditional conflicts (409) apart from other failures.
6. A build-info gauge: version, VMM API revision, arena mode. VM intervals aggregated only, never a series per VM, to keep tenant identities out of the label space.
7. Template imports: count, duration, bytes read.

Prometheus pulls. A metric set just before a process exits is never scraped, and a restarted host starts its counters at zero. Anything that happens at or near exit (shutdown, drain, the final checkpoint, a fatal error) must be recorded somewhere that outlives the process: a structured log line, the report a drain already sends the orchestrator, or durable state. Metrics are for what a scrape will see.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 The stall counters in 1 and the build-info gauge in 6 are in the exposition, tested through hostapi.Metrics
- [x] #2 Fault, load and seal latencies are exported as Prometheus histograms with _bucket, _sum and _count series
- [x] #3 Status carries interval checkpoint attempts, failures by reason, pause and upload durations, and they are exported
- [ ] #4 Migrations, forks, receives, VMM deaths, fenced VMs and pager-ordered stops are counted and exported, per outcome
- [ ] #5 Object-store operations are exported with a latency histogram per operation
- [ ] #6 Template imports are exported: count, duration and bytes read
- [ ] #7 Every event that can happen at or near process exit is also recorded where it outlives the process, and docs/hosting.md says which record to read for which event
- [x] #8 No metric carries a VM or tenant identity as a label
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. Pager stall counters, fault/load/seal histograms, build info (Status + exposition). Histogram series written by hand: cumulative _bucket in seconds, +Inf, _sum, _count.
2. Interval checkpoint outcomes in Status: attempts, failures by reason (capture, publish, fenced), pause and upload histograms, bytes uploaded.
3. Lifecycle counters by outcome: migrations, forks, receives (with pause histograms), VMM deaths, fenced VMs, pager-ordered stops.
4. Object-store latency histogram per operation in the metered store.
5. Template imports: count, duration histogram, bytes read.
6. Near-exit events: make sure each has a structured log line or an orchestrator report; document which record to read in docs/hosting.md.
Every metric aggregated per host; no VM or tenant labels (tested).
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Step 1 done: stall counters, fault/load/seal histograms and sproutfs_build_info. TestMetricsNameNoVMOrTenant guards AC 8.

Step 2 done: host.Activity counts the interval loop's attempts and outcomes (published, capture_failed, publish_failed, fenced), pause and upload histograms and uploaded bytes; Status.Checkpoints and sproutfs_checkpoint* export them. TestAHostCountsItsIntervalCheckpoints drives a store outage and recovery on a simulated clock.
<!-- SECTION:NOTES:END -->
