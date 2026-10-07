#!/usr/bin/env python3
"""Tables of a run of scripts/bench-fsync-journal-gce.sh, as Markdown.

The argument is the run's results directory:

- raw/fio-<disk>/*.json: fio's writes with a sync, one in flight, on a
  journal-sized disk of each kind (scripts/lib/fsync-fio.sh);
- sproutfs-fsync/<label>/flush.jsonl and the host's /metrics around each
  case: the guest's flushes and writes (scripts/lib/fsync-journal-run.sh), for
  each label: off, hdb, pds;
- sproutfs-fsync/<label>/interval.*: one thread flushing for minutes, with the
  journal's live bytes sampled each second.

The /metrics around a short case also count what the witness wrote before its
clock started, each thread's file whole, so the journal's bytes per guest
byte are the long run's alone.

A sync job's latency is the write's and the fsync's added at each
percentile, which bounds the pair from above; fio times them apart. A dsync
job's is the one write that the device makes durable before it answers.
"""

import json
import sys
from pathlib import Path

SIZES = ["4k", "8k", "16k", "32k", "64k"]
DISKS = [("hdb", "Hyperdisk Balanced"), ("pds", "pd-ssd")]
LABELS = [("off", "off"), ("hdb", "on, Hyperdisk Balanced"), ("pds", "on, pd-ssd")]
CASES = ["sync-1", "sync-8", "sync-64", "write-1", "write-8"]
PERCENTILES = ["50.000000", "99.000000", "99.900000"]


def us(ns):
    return f"{ns / 1000:,.0f}"


def percentiles(section):
    """The percentiles of one of fio's latency sections, in ns."""
    lat = section.get("clat_ns") or section.get("lat_ns") or {}
    found = lat.get("percentile", {})
    return [found.get(p, 0) for p in PERCENTILES]


def fio_table(root):
    lines = ["| disk | write | write + fsync p50 / p99 / p99.9 µs | O_DSYNC write p50 / p99 / p99.9 µs |"
             " writes a second, fsync / O_DSYNC |",
             "| --- | --- | --- | --- | --- |"]
    for short, name in DISKS:
        directory = root / "raw" / f"fio-{short}"
        if not directory.exists():
            continue
        fresh = directory / "fresh-sync-4k.json"
        if fresh.exists():
            sync = json.loads(fresh.read_text())["jobs"][0]
            paired = [a + b for a, b in zip(percentiles(sync["write"]), percentiles(sync["sync"]))]
            lines.append(f"| {name} | 4 KiB, never written | {' / '.join(us(v) for v in paired)} | – | "
                         f"{sync['write']['iops']:,.0f} / – |")
        for size in SIZES:
            sync = json.loads((directory / f"sync-{size}.json").read_text())["jobs"][0]
            dsync = json.loads((directory / f"dsync-{size}.json").read_text())["jobs"][0]
            write = percentiles(sync["write"])
            flushed = percentiles(sync["sync"])
            paired = [a + b for a, b in zip(write, flushed)]
            direct = percentiles(dsync["write"])
            lines.append(f"| {name} | {size.upper().replace('K', ' KiB')} | "
                         f"{' / '.join(us(v) for v in paired)} | {' / '.join(us(v) for v in direct)} | "
                         f"{sync['write']['iops']:,.0f} / {dsync['write']['iops']:,.0f} |")
    return "\n".join(lines)


def metrics(path):
    """A /metrics scrape as a map of series to value."""
    values = {}
    for line in path.read_text().splitlines():
        if not line or line.startswith("#"):
            continue
        series, _, value = line.rpartition(" ")
        try:
            values[series] = float(value)
        except ValueError:
            continue
    return values


def delta(before, after, series):
    return after.get(series, 0) - before.get(series, 0)


def flush_table(root):
    lines = ["| durable flush | case | writes | MiB/s | p50 / p99 / p99.9 µs | journal flushes |"
             " PMEM protect traps a second | capture mean ms |",
             "| --- | --- | --- | --- | --- | --- | --- | --- |"]
    for label, name in LABELS:
        directory = root / "sproutfs-fsync" / label
        path = directory / "flush.jsonl"
        if not path.exists():
            continue
        reports = [json.loads(line) for line in path.read_text().splitlines() if line.strip()]
        for case, report in zip(CASES, reports):
            before, after = metrics(directory / f"{case}.before.prom"), metrics(directory / f"{case}.after.prom")
            flushes = delta(before, after, 'sproutfs_journal_flushes_total{outcome="succeeded"}')
            traps = delta(before, after, 'sproutfs_pager_protect_traps_total{kind="pmem"}')
            captures = delta(before, after, "sproutfs_journal_capture_seconds_count")
            capture_seconds = delta(before, after, "sproutfs_journal_capture_seconds_sum")
            capture = f"{capture_seconds / captures * 1000:.2f}" if captures else "–"
            lines.append(f"| {name} | {case} | {report['writes']:,} | {report['mib_per_second']:.2f} | "
                         f"{report['p50_us']:,.0f} / {report['p99_us']:,.0f} / {report['p999_us']:,.0f} | "
                         f"{flushes:,.0f} | {traps / report['seconds']:,.0f} | {capture} |")
    return "\n".join(lines)


def interval_table(root):
    lines = ["| durable flush | seconds | flushes | p50 / p99 / p99.9 / max µs | journal bytes per guest byte |"
             " journal MiB/s | live MiB, peak | PMEM protect traps a second | capture mean ms |",
             "| --- | --- | --- | --- | --- | --- | --- | --- | --- |"]
    for label, name in LABELS:
        directory = root / "sproutfs-fsync" / label
        if not (directory / "interval.json").exists():
            continue
        report = json.loads((directory / "interval.json").read_text())
        before, after = metrics(directory / "interval.before.prom"), metrics(directory / "interval.after.prom")
        written = delta(before, after, "sproutfs_journal_written_bytes_total")
        traps = delta(before, after, 'sproutfs_pager_protect_traps_total{kind="pmem"}')
        captures = delta(before, after, "sproutfs_journal_capture_seconds_count")
        capture_seconds = delta(before, after, "sproutfs_journal_capture_seconds_sum")
        rows = [line.split("\t") for line in (directory / "interval.tsv").read_text().splitlines()[1:]]
        live = [float(r[1]) for r in rows if len(r) == 5 and r[1]]
        lines.append(f"| {name} | {report['seconds']:.0f} | {report['writes']:,} | "
                     f"{report['p50_us']:,.0f} / {report['p99_us']:,.0f} / {report['p999_us']:,.0f} / "
                     f"{report['max_us']:,.0f} | {written / report['bytes']:.2f} | "
                     f"{written / 2**20 / report['seconds']:.2f} | {max(live) / 2**20:,.0f} | "
                     f"{traps / report['seconds']:,.0f} | {capture_seconds / captures * 1000:.2f} |")
    return "\n".join(lines)


def main(root):
    root = Path(root)
    print("## Raw disks\n")
    print(fio_table(root))
    print("\n## Flushes in a guest\n")
    print(flush_table(root))
    print("\n## One thread flushing over checkpoint intervals\n")
    print(interval_table(root))


if __name__ == "__main__":
    if len(sys.argv) != 2:
        sys.exit(__doc__)
    main(sys.argv[1])
