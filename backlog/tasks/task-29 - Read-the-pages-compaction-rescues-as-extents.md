---
id: TASK-29
title: Read the pages compaction rescues as extents
status: To Do
assignee: []
created_date: '2026-09-25 18:18'
labels:
  - performance
dependencies: []
priority: low
type: enhancement
ordinal: 29000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
**Compaction reads the pages it rescues one at a time.** A read of a range of
a volume now fetches a run of members as one ranged read per extent. But
compaction walks a segment's pages and calls `Store.loadPage` for each page it
moves. So rewriting a mostly dead checkpoint of 4 KiB pages costs one request
per page, up to `compactionBudget`. That budget is 64 MiB, which is 16,384
requests (`checkpoint/publication.go`, `compact`). The pages it moves
are consecutive within a segment, and their members are adjacent in the part
they came from, so the same grouping would apply. It was left as it is
because compaction runs after the guest has resumed and off the fault path.
<!-- SECTION:DESCRIPTION:END -->
