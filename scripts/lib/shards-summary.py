#!/usr/bin/env python3
"""Tables of a run of scripts/bench-shards-gce.sh, as Markdown.

The argument is the run's results directory: one directory per disk type,
each with the drive's results.json and fio's JSON from every host.

Each case's percentile is the median over the rounds of each round's, and
hops a second the median over the rounds of reads over seconds. The bands
sum the rounds. fio's figures are the median over the hosts, with the
lowest host's beside them.
"""

import json
import statistics
import sys
from pathlib import Path

DISKS = ["local-nvme", "hyperdisk-balanced", "pd-balanced", "pd-ssd"]
NAMES = {"local-nvme": "local NVMe", "hyperdisk-balanced": "Hyperdisk Balanced",
         "pd-balanced": "pd-balanced", "pd-ssd": "pd-ssd"}
PERCENTILES = ["p50", "p90", "p99", "p99.9", "max"]
# A histogram's bucket i holds reads of [2^(i-7), 2^(i-6)) ms, the first
# everything under 1/64 ms. Each band sums buckets from its first to its last.
BANDS = [("<0.5", 0, 5), ("0.5–1", 6, 6), ("1–2", 7, 7), ("2–4", 8, 8), ("4–8", 9, 9), ("8–16", 10, 10),
         ("16–32", 11, 11), ("32–64", 12, 12), ("64–128", 13, 13), ("128–256", 14, 14), ("256–512", 15, 15),
         ("512+", 16, 19)]
FIO = ["read-seq-1m-qd8", "read-rand-512k-qd1", "read-rand-512k-qd4", "read-rand-512k-qd16",
       "read-rand-4k-qd1", "read-rand-4k-qd16", "read-rand-4k-qd64", "write-seq-1m-qd8",
       "write-rand-512k-qd4", "write-rand-4k-qd16"]
BUDGET_MIB = 500


def number(value):
    if value >= 100:
        return f"{value:,.0f}"
    if value >= 10:
        return f"{value:.1f}"
    return f"{value:.2f}"


def disks(root):
    return [d for d in DISKS if (root / d / "results.json").exists()]


def cases(root, disk):
    return json.loads((root / disk / "results.json").read_text())


def chain_tables(root):
    rows, bands = [], []
    for disk in disks(root):
        result = cases(root, disk)
        groups = {}
        for c in result["cases"]:
            if c.get("profiled"):
                continue
            groups.setdefault((c["case"], c["source"]), []).append(c)
        for (case, source), runs in sorted(groups.items(), key=lambda kv: (kv[0][1] != "cluster", kv[0][0])):
            size, _, unit, _ = case.split("/")
            label = NAMES[disk] if source == "cluster" else "none (store)"
            cells = [number(statistics.median(r["latency_ms"][p] for r in runs)) for p in PERCENTILES]
            hops = statistics.median(r["reads"] / r["seconds"] for r in runs)
            wrong = sum(r["wrong"] + r["failed"] for r in runs)
            rows.append(f"| {label} | {size} | {unit} | {' | '.join(cells)} | {number(hops)} | {len(runs)} |"
                        + (f" {wrong} wrong or failed |" if wrong else ""))
            counts = [sum(sum(r["histogram"][lo:hi + 1]) for r in runs) for _, lo, hi in BANDS]
            bands.append(f"| {label} | {size} | {unit} | " + " | ".join(f"{n:,}" for n in counts) + " |")
    print("| Disk | Guest | Read | p50 | p90 | p99 | p99.9 | max | hops/s | rounds |")
    print("| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |")
    print("\n".join(rows))
    print()
    print("| Disk | Guest | Read | " + " | ".join(b for b, _, _ in BANDS) + " |")
    print("| --- | --- | --- | " + " | ".join("---" for _ in BANDS) + " |")
    print("\n".join(bands))
    print()


def cluster_details(root):
    """What the cluster reads did besides their latency: holders' bytes, the
    store's reads, second requests and BUSY answers, per disk and case."""
    print("| Disk | Case | store GETs | hedges won | second requests | busy | wrong stripes | timeouts |"
          " served MB/s per holder (min–max) | holders' CPU/s |")
    print("| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |")
    for disk in disks(root):
        groups = {}
        for c in cases(root, disk)["cases"]:
            if c.get("profiled") or c["source"] != "cluster":
                continue
            groups.setdefault(c["case"], []).append(c)
        for case, runs in sorted(groups.items()):
            gets = sum(r["store_gets"] for r in runs)
            won = sum(r["read"].get("StoreHedgesWon", 0) for r in runs)
            second = sum(r["read"].get("SecondRequests", 0) for r in runs)
            busy = sum(sum(r["busy"]) for r in runs)
            wrong = sum(r["read"].get("WrongStripes", 0) for r in runs)
            timeouts = sum(r["read"].get("Timeouts", 0) for r in runs)
            rates = [b / r["seconds"] / 1e6 for r in runs for i, b in enumerate(r["served_bytes"]) if i != 1]
            cpu = statistics.median(statistics.median(c / r["seconds"] for i, c in enumerate(r["cpu_seconds"])
                                                      if i != 1) for r in runs)
            print(f"| {NAMES[disk]} | {case} | {gets} | {won} | {second} | {busy} | {wrong} | {timeouts} |"
                  f" {min(rates):.0f}–{max(rates):.0f} | {cpu:.2f} |")
    print()


def publications(root):
    print("| Disk | Guest | commit s | settled s | fills dropped |")
    print("| --- | --- | --- | --- | --- |")
    for disk in disks(root):
        result = cases(root, disk)
        for vm, p in sorted(result["publish"].items()):
            dropped = sum(p["fill"].get("Dropped", []) or [])
            print(f"| {NAMES[disk]} | {vm} | {p['seconds']:.1f} | {p['settled_seconds']:.1f} | {dropped} |")
    print()


def fio_job(path):
    job = json.loads(path.read_text())["jobs"][0]
    side = job["read"] if job["read"]["io_bytes"] > 0 else job["write"]
    clat = side["clat_ns"].get("percentile", {})
    return {"mib": side["bw_bytes"] / (1 << 20), "iops": side["iops"],
            "p50": clat.get("50.000000", 0) / 1e3, "p99": clat.get("99.000000", 0) / 1e3}


def fio_table(root):
    print("| Disk | Job | MiB/s (lowest host) | IOPS | p50 µs | p99 µs | of 500 MiB/s |")
    print("| --- | --- | --- | --- | --- | --- | --- |")
    for disk in DISKS:
        hosts = sorted((root / disk).glob("fio-*/"))
        if not hosts:
            continue
        for name in FIO:
            found = [fio_job(h / f"{name}.json") for h in hosts if (h / f"{name}.json").exists()]
            if not found:
                continue
            mib = statistics.median(f["mib"] for f in found)
            low = min(f["mib"] for f in found)
            iops = statistics.median(f["iops"] for f in found)
            p50 = statistics.median(f["p50"] for f in found)
            p99 = statistics.median(f["p99"] for f in found)
            print(f"| {NAMES[disk]} | {name} | {number(mib)} ({number(low)}) | {iops:,.0f} | {number(p50)} |"
                  f" {number(p99)} | {mib / BUDGET_MIB:.0%} |")
    print()


def main():
    root = Path(sys.argv[1])
    print("## Chains\n")
    chain_tables(root)
    print("## The cluster's reads\n")
    cluster_details(root)
    print("## Publications\n")
    publications(root)
    print("## fio\n")
    fio_table(root)


if __name__ == "__main__":
    main()
