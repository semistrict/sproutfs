#!/usr/bin/env python3
"""Tables of a hot-tier walk (scripts/bench-hot-tier-aws.sh and -gce.sh).

Usage: hot-tier-summary.py <results directory>

Reads walk.json, and fio-<host>/*.json where fio ran, and prints Markdown:
each source's reads, the median over rounds of each round's figure; each
round; the cold hot tier; the publications; and each disk raw.
"""
import json
import statistics
import sys
from pathlib import Path

def page(case):
    return "2 MiB" if case["page_bytes"] == 2 << 20 else "4 KiB"


def ms(value):
    if value >= 100:
        return f"{value:,.0f}"
    if value >= 10:
        return f"{value:.1f}"
    if value >= 1:
        return f"{value:.2f}"
    return f"{value:.3f}"


def hops(value):
    return f"{value:,.0f}" if value >= 100 else f"{value:.1f}"


def walks(result):
    by = {}
    for case in result["cases"]:
        by.setdefault((page(case), case["source"]), []).append(case)
    order = [(p, s) for p in ("2 MiB", "4 KiB") for s in ("regional", "hot", "cluster")]
    print("| Source | page | p50 | p90 | p99 | max | hops/s | store GETs | hot GETs | hits | misses |")
    print("| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |")
    for key in order:
        cases = by.get(key)
        if not cases:
            continue
        median = lambda name: statistics.median(c["latency_ms"][name] for c in cases)
        total = lambda field: sum(c[field] for c in cases)
        hot = lambda field: sum(c["hot"][field] for c in cases)
        print(f"| {key[1]} | {key[0]} | {ms(median('p50'))} | {ms(median('p90'))} | {ms(median('p99'))} | "
              f"{ms(median('max'))} | {hops(statistics.median(c['hops_per_second'] for c in cases))} | "
              f"{total('store_gets')} | {total('hot_gets')} | {hot('Hits')} | {hot('Misses')} |")
    print()
    print("| Source | page | p50 by round | p99 by round | max by round | hops/s by round |")
    print("| --- | --- | --- | --- | --- | --- |")
    for key in order:
        cases = sorted(by.get(key, []), key=lambda c: c["round"])
        if not cases:
            continue
        column = lambda name: ", ".join(ms(c["latency_ms"][name]) for c in cases)
        print(f"| {key[1]} | {key[0]} | {column('p50')} | {column('p99')} | {column('max')} | "
              f"{', '.join(hops(c['hops_per_second']) for c in cases)} |")
    print()
    print("Wrong pages:", sum(c["wrong"] for c in result["cases"] + result["cold"]))
    print()


def cold(result):
    print("| page | round | hops/s | p50 | p99 | hits | misses | failed (error, slow, corrupt) | fills sent | "
          "MiB sent | present | dropped (queue, rate, read, write) | store GETs | hot GETs | hot PUTs |")
    print("| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |")
    for case in result["cold"]:
        hot = case["hot"]
        print(f"| {page(case)} | {case['round']} | {hops(case['hops_per_second'])} | "
              f"{ms(case['latency_ms']['p50'])} | {ms(case['latency_ms']['p99'])} | {hot['Hits']} | {hot['Misses']} | "
              f"{', '.join(str(n) for n in hot['Failed'])} | {hot['Sent']} | {hot['SentBytes'] / (1 << 20):,.0f} | "
              f"{hot['Present']} | {', '.join(str(n) for n in hot['Dropped'][:4])} | {case['store_gets']} | "
              f"{case['hot_gets']} | {case['hot_puts']} |")
    print()


def publications(result):
    print("| guest | commit s | settled s |")
    print("| --- | --- | --- |")
    names = ["guest (cluster)", "guest-hot (hot tier)", "small (cluster)", "small-hot (hot tier)"]
    for name, published in zip(names, result["publish"]):
        print(f"| {name} | {published['seconds']:.1f} | {published['settled_seconds']:.1f} |")
    print()


def fio(directory):
    hosts = sorted(directory.glob("fio-*/"))
    jobs = {}
    for host in hosts:
        for path in sorted(host.glob("*.json")):
            try:
                job = json.loads(path.read_text())["jobs"][0]
            except (ValueError, KeyError, IndexError):
                continue
            side = job["read"] if job["read"]["io_bytes"] else job["write"]
            percentiles = side["clat_ns"].get("percentile", {})
            jobs.setdefault(path.stem, []).append((side["bw_bytes"] / (1 << 20), side["iops"],
                                                   percentiles.get("50.000000", 0) / 1e3,
                                                   percentiles.get("99.000000", 0) / 1e3))
    if not jobs:
        return
    print(f"fio, the median over {len(hosts)} hosts:")
    print()
    print("| Job | MiB/s | IOPS | p50 µs | p99 µs |")
    print("| --- | --- | --- | --- | --- |")
    for name, runs in jobs.items():
        median = [statistics.median(run[i] for run in runs) for i in range(4)]
        print(f"| {name} | {hops(median[0])} | {median[1]:,.0f} | {median[2]:,.0f} | {median[3]:,.0f} |")
    print()


def main():
    directory = Path(sys.argv[1])
    result = json.loads((directory / "walk.json").read_text())
    walks(result)
    cold(result)
    publications(result)
    fio(directory)


if __name__ == "__main__":
    main()
