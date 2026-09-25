---
id: TASK-6
title: Set each VM's shape per request
status: Done
assignee:
  - '@claude'
created_date: '2026-09-25 18:17'
updated_date: '2026-09-25 22:13'
labels:
  - embedder
dependencies: []
priority: high
type: feature
ordinal: 6000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
An embedding program replaces JuiceFS with sproutfs in its sandbox host. This is one of the changes it needs. vCPUs, RAM and disk size must be set per create, not by the host-wide SPROUTFS_VM_VCPUS and SPROUTFS_VM_MEMORY_BYTES.

**A guest image's RAM is fixed when the image is first imported.** A host reads `SPROUTFS_VM_MEMORY_BYTES`, and a template's own `name=path:bytes`, when it imports the image into a template. That import now happens once for the whole deployment, because the template is named by the image's bytes. So raising either value and rolling out gives no new memory to a VM created from an image the deployment already holds. The create forks the template, and a fork inherits the template's memory. A cold start with `--memory` changes one VM's shape, and `scripts/lib/demo-bigguest.sh` uses that. But the only way to make every VM of an image larger from now on is to change the image. Either a create should publish its root at the shape the host's configuration gives that image, or the configured memory should be part of the template's name.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 A create request sets vCPUs, RAM and disk size
- [x] #2 A VM booted cold after its host is lost gets its own vCPU count, and a fork inherits it
- [x] #3 The host-wide settings are only defaults
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. checkpoint: root field vcpus (zero = none recorded); Index.VCPUs; Publication.SetVCPUs; a root inherits its parent's.
2. volume: DiscardMemory takes a Shape (sizes and vCPUs); VM.VCPUs.
3. host: Create forks the template and publishes its root at the requested shape through DiscardMemory; ColdShape carries vCPUs; the Starter gets the VM's vCPUs through Launch.VCPUs.
4. api/host and api/orch: create takes vcpus, memory and disk; open cold takes vcpus; VM reports vcpus.
5. orchestrator: places a create by its requested memory and records it; sproutfsctl create flags.
6. Prove: unit tests per layer, simtest create at a shape and cold boot after host loss keeps vCPUs, Lima boot of a created VM at 2 vCPUs.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Lima: TestAVMBootsWithItsOwnProcessorCount passes (guest sees 0-1 with the Starter default at 1). Mac: go test ./... passes.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Committed. The checkpoint root records a processor count that forks and later checkpoints keep; a create publishes its first checkpoint at the requested RAM, disk and processors through Host.Reshape; the Starter boots with Launch.VCPUs. Verified on Lima and by unit tests at every layer.
<!-- SECTION:FINAL_SUMMARY:END -->
