---
id: TASK-92.6
title: 'Zircon port step 6: the spill as references in the page list'
status: To Do
assignee: []
created_date: '2026-10-05 05:09'
updated_date: '2026-10-05 07:06'
labels:
  - pager
  - zircon-port
dependencies:
  - TASK-92.5
references:
  - plans/zircon-pager-port-2026-10-05.md
parent_task_id: TASK-92
priority: high
type: task
ordinal: 104000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Step 6 of the plan. Zircon records a compressed page as a reference in its page list slot and keeps the bytes in compression storage (zircon/kernel/vm/compression.cc, vm/slot_page_storage.cc). A spilled page is the same thing with the spill file as the storage. Two departures apply: a page a pager backs may be spilled when dirty (D2), and a dirty page takes its spill slot before it is dirty, so a spill never needs space (D5). LZ4 is not ported; the storage keeps the page as it is with its CRC32C.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A spilled page is a reference in the page list; the storage follows VmCompression's interface over the spill file, with the file allocated at start, truncated at start and checked on read as today
- [ ] #2 compression_smoke_test, compression_zero_test, compression_fail_test and compression_move_reference_test run as Go tests
- [ ] #3 vmmemory/reservations_internal_test.go is rewritten against the new storage and keeps every property it tests
- [ ] #4 The guards spill-sparse and pager-forget-spill sit in the new storage under the same names and are still killed by the tests guards.json names
- [ ] #5 Every test in vmmemory, host, vmmigrate and internal/simtest passes unchanged in both arena modes
<!-- AC:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
TASK-92.9 ported compressor.cc and compression.cc's reference bookkeeping into vmmemory/internal/zirconvm/compression.go ahead of this step, over storage and strategy interfaces, without D5 or the four tests. This step adds the reference storage, D5 and the reservations.
<!-- SECTION:NOTES:END -->
