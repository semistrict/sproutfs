---
title: Serving a peer copies nothing into memory
summary: A host serves a page from its disk cache to another host without reading the page into its own memory.
---

**Given** a host whose disk cache holds a page another host asks for,
**when** it serves the page,
**then** the bytes go from its disk to the network as they are stored, with no
decode, no encode and no copy through the host process's memory. A transport
that cannot do this, such as TLS in user space, uses a bounded buffer instead.

**Status, 2026-10-03.** Not yet. A host reads the stripes it serves into its own memory and sends them
from there. Serving by `sendfile` is step 8 of the disk cache plan, once the
cost of the copy is measured.
