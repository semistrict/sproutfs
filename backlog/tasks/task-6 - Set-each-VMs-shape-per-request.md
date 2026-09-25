---
id: TASK-6
title: Set each VM's shape per request
status: To Do
assignee: []
created_date: '2026-09-25 18:17'
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
- [ ] #1 A create request sets vCPUs, RAM and disk size
- [ ] #2 A VM booted cold after its host is lost gets its own vCPU count, and a fork inherits it
- [ ] #3 The host-wide settings are only defaults
<!-- AC:END -->
