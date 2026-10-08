---
id: TASK-119
title: 'Give the orchestrator a fault campaign over its hosts, pods and records'
status: Done
assignee:
  - '@claude'
created_date: '2026-10-08 15:23'
updated_date: '2026-10-08 16:36'
labels:
  - testing
dependencies: []
priority: high
ordinal: 154000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
The orchestrator (cmd/sproutfs-orchestrator) calls each host through hostClient (the host API), the Kubernetes API through pods, and the bucket through records. Its tests fake all three, and the fakes fail only where a test switches a failure on (down, wedged, refuse, receives). That is the pattern that hid the mapping refusal which killed an embedder's VM (docs/testing.md, "Every boundary error is simulated"). Nothing lists these interfaces under scripts/faults, and no campaign meets a host that acted and lost its reply.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 scripts/faults/orchestrator.json lists hostClient, pods and records, with an entry for each error each method can return
- [x] #2 The fakes return each at random at a sim.Buggify site, including a reply lost after the host acted
- [x] #3 A seeded campaign drives creates, migrations, forks, drains, stops and recoveries through them and requires, once the faults stop, every VM to run on exactly one host or be stopped, and no VM to be lost
- [x] #4 python3 scripts/check-faults.py --file scripts/faults/orchestrator.json passes
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. Give the fakes in cmd/sproutfs-orchestrator/orchestrator_test.go a sim.Buggify site for each error the real clients return: host API unreachable, refused, and reply lost after acting; Kubernetes API unavailable and delete reply lost; bucket unavailable. Let the fake hosts write the bucket as real ones do.
2. Write TestTheOrchestratorUnderBoundaryFaults: a synctest campaign over many seeds driving creates, forks, migrations, drains, stops, starts, recoveries, kills, restarts, deletes, partitions and the reconcile timer; once faults stop and the deployment settles, every VM with a record runs on exactly one host or starts, none made is lost, no host runs a VM without a record or holds a handover; every site fires.
3. Write scripts/faults/orchestrator.json and make check-faults pass.
4. Fix each bug found, with a regression test and a sim.Bug guard in scripts/mutation/guards.json.
5. Update docs/testing.md.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Bugs found and fixed, each with a regression test and guard:
- Start opened a VM on a second host while the host running it was quiet (TestStartIsRefusedWhileTheHostRunningTheVMIsQuiet, orchestrator-start-past-a-quiet-host). Found by the campaign.
- Delete removed the record of a VM a quiet host ran (TestADeleteWaitsForTheQuietHostThatMayRunTheVM, orchestrator-delete-past-a-quiet-host). Found by the campaign.
- Start and Delete now need every host to answer, or a table row saying the VM stopped (seenStopped). Rows said stopped without a host's word in three places: an open whose answer was lost (orchestrator-take-a-lost-open-for-a-refusal), a reconcile while the VM's host was quiet (orchestrator-table-stops-a-quiet-hosts-vm), and a handover that ended while its destination was quiet after a lost receive answer (errUnsettled, orchestrator-stop-an-unsettled-handover). A reconcile also kept a deleted VM's row when its host was gone (orchestrator-keep-a-deleted-vms-row).
- The fake's receive refusals are now error statuses, as a real host's are.
Validation: go test ./cmd/sproutfs-orchestrator passes (128 seeds, every one of 57 sites fires in at least 7 seeds); SPROUTFS_ORCHESTRATOR_SEEDS=500 passes; a 6-seed -race run found no race; check-faults --file scripts/faults/orchestrator.json passes; check-guards kills all five new guards. Seeds are not replayed exactly: concurrent surveys and the reconcile timer draw faults in arrival order.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
The orchestrator's fakes fail every method of hostClient, pods and records at random (unreachable, refused, reply lost after acting), and TestTheOrchestratorUnderBoundaryFaults drives every operation through them on 128 seeds, with partitions, restarts and the reconcile timer, then requires every VM on exactly one host or startable, none lost. scripts/faults/orchestrator.json lists 57 faults. The campaign found starts and deletes going past a quiet host; they now need every host to answer or a row saying stopped, and three evidence-free writers of stopped are fixed. Verified: just check passes, check-faults on the manifest passes, 500 seeds pass, five new guards killed.
<!-- SECTION:FINAL_SUMMARY:END -->
