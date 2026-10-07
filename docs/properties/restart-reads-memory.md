---
title: Resident pages come from memory
summary: A restarted VM gets a page still resident on its host from that memory.
---

**Given** a VM stopped hours ago whose page is still resident on the host,
**when** the VM starts again on that host,
**then** that page comes from memory, with no disk or network read.

**Status, 2026-10-03.** Not shown. Placing a restarted VM on the host that
still holds its pages in memory is left to a later plan.
