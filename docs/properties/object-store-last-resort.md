---
title: The object store is the last resort
summary: A restarted VM reads a page from the object store only when no host holds it.
---

**Given** a VM stopped hours ago,
**when** it starts again,
**then** it reads a page from the object store only when no host in the
cluster holds that page in memory or on disk.
