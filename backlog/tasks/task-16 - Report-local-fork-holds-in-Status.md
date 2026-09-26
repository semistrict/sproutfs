---
id: TASK-16
title: Report local fork holds in Status
status: In Progress
assignee:
  - '@claude'
created_date: '2026-09-25 18:17'
updated_date: '2026-09-26 15:11'
labels:
  - embedder
dependencies: []
priority: medium
type: feature
ordinal: 16000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
An embedding program replaces JuiceFS with sproutfs in its sandbox host. This is one of the changes it needs. The control plane can then release them.

**A child forked onto its parent's own host holds the fork point without the orchestrator seeing the hold.** Such a child is served no pages, so it is not in the parent host's `Status().Serving`. The orchestrator's survey of stale handovers therefore cannot see the hold. Only the host's own four-interval deadline ends a hold whose child never publishes its root (`host/fork.go`, `host/migrate.go`). If hosts reported local holds alongside served ones, the survey could release them the same way.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 Status lists local fork holds, and the orchestrator's survey releases stale ones
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
Finding: host.Status().Serving already listed local fork holds (host/host.go serving()), and the survey already asked to release them. What was missing is that a local hold was not the same shape as a served one: its release was never refused and Outstanding reported 0 before the child was taken in, so a release before the take-in was accepted on the table's word alone. The orchestrator's fake host already assumed the refusal.
1. host: a local hold records when Receive binds its child to the point. ReleaseMigrated refuses a local hold whose child is not taken in, with vmmigrate.ErrOutstanding, as the page server refuses one with pages outstanding. Status.Outstanding counts the point's pages for such a hold until then.
2. orchestrator: the survey gives up a handover whose VM does not exist and that nothing is creating: no host runs it, nothing is in flight, and no row names it or (on a reconcile, which lists the bucket) it has no control record.
3. Tests in host, orchestrator and simtest. simtest: Settle runs the orchestrator's survey; a forgotten-releases fault skips the release after a receive; after every step every sealed VM must have a hold for one of its children in its host's Serving, and a hold whose child runs on that host must owe nothing.
4. docs: migration.md, hosting.md, testing.md, deploy/README.md.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Host: forkHold.taken set in Receive after vmmigrate.Receive binds a local child; release() refuses an untaken local hold with ErrOutstanding before touching anything; outstanding() reports hold.owed() for local holds.
Orchestrator: release() takes the bucket listing from Reconcile; forsaken() = no host runs it and (no row or no record). A stale local hold whose child was never taken in is now given up on the first reconcile after its row ages out of flight (2 min), instead of being released on the table's word.
Sim: VerifyHandovers after every step; negative check: hiding local holds from serving() makes TestASurveyEndsTheHoldsOfAFanOutOntoItselfThatNothingReleased fail with 'vm-1 is sealed and host-0 reports no hold for a child of it'. The three campaign seeds draw forgotten-releases but no fork lands in its window, so the dedicated test is what exercises it.
Validation: go test ./... and just check pass (macOS). Linux-only host code unchanged; no Lima run.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Local fork holds were already in Status().Serving; this made them the same shape as served holds and made the survey end stale ones on evidence. The host now refuses to release a local hold until Receive has bound its child to the fork point (ErrOutstanding, 409) and reports the point's pages as outstanding until then. The orchestrator's reconcile gives up a hold whose child has no control record, no host runs it and nothing is in flight. Verified by host TestAHostReportsTheHoldsOfAFanOutOntoItself, orchestrator localfork_test.go (released when taken in, given up when the child does not exist, left alone in flight), and simtest TestASurveyEndsTheHoldsOfAFanOutOntoItselfThatNothingReleased plus a per-step VerifyHandovers invariant and a forgotten-releases fault; go test ./... and just check pass.
<!-- SECTION:FINAL_SUMMARY:END -->
