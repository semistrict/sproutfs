---
title: A hot page spreads its load
summary: No one host serves all of a page that many hosts read at once.
---

**Given** a page that many hosts read at about the same time,
**when** they read it,
**then** each host that serves it sends only part of it, so no one host sends
the whole page to every reader. This holds from the first read, without
detecting that the page is hot.

**Status, 2026-10-03.** Holds. `TestAHotPageSpreadsItsLoadOverEveryHolder`
has six hosts read one page at once under 4+2: every holder is asked, each reader asks four besides
itself, and each holder sends each reader one stripe, a quarter of the
page. On GCE, one reader's 4,096 windows spread within 2.5 % over the five
other hosts ([measurement](../measurements/gce-cluster-reads-2026-10-03.md)).
