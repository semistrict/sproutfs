---
id: TASK-68
title: Refuse to start a host whose VMM lacks an API it will use
status: To Do
assignee: []
created_date: '2026-09-29 23:10'
labels:
  - embedder
  - firecracker
dependencies: []
references:
  - vmmachine/process_linux.go
priority: high
type: feature
ordinal: 76000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
A Firecracker build without the `sync_snapshot_files` field booted VMs and failed only at their first capture on stop. That VM lost every write since its last checkpoint. The next change to the fork's API would fail the same way. The field is sent at vmmachine/process_linux.go:939.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A host refuses to start, with an error naming what is missing, when its VMM lacks any managed-memory API field the host will send
- [ ] #2 The check runs at host start, before any VM is created or opened
- [ ] #3 A test starts a host against a VMM that reports an older version and sees it refused
<!-- AC:END -->
