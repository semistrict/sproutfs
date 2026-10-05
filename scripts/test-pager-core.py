#!/usr/bin/env python3
"""Run the pager's suites under the zircon pager core.

The pager's page layer is being ported from Zircon's (plans/zircon-pager-port-
2026-10-05.md). Its new core runs beside the old one, selected by
SPROUTFS_PAGER_CORE, and serves everything the old one does, so the suites that
build pagers (the pager's own, the host's, migration's, the simulation's and the
machine's) run under it too, in both arena modes, as `just check` runs them
under the current core. It fails if any of them fails.

--survey runs each test of a package alone instead, under the zircon core in
both arena modes, and reports which pass, which is what finding the tests a
change to the core broke needs.
"""

import argparse
import concurrent.futures
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import time

ARENAS = ("isolated", "shared")

# The packages whose tests build pagers, each of which picks its core by
# SPROUTFS_PAGER_CORE (internal/testcore).
SUITES = ("./vmmemory/...", "./host/...", "./vmmigrate/...", "./internal/simtest/...", "./vmmachine/...")


def environment(arena):
    """The environment of a run under the zircon core in one arena mode."""
    env = {k: v for k, v in os.environ.items() if not k.startswith("SPROUTFS_")}
    env["SPROUTFS_PAGER_CORE"] = "zircon"
    env["SPROUTFS_ARENA"] = arena
    return env


def run(binary, directory, tests, arena, timeout):
    """Runs the named tests of one package in one arena mode. It reports the
    tests that did not pass, a skipped test passing, and the output."""
    pattern = "^(" + "|".join(re.escape(t) for t in tests) + ")$"
    try:
        result = subprocess.run(
            [str(binary), "-test.run=" + pattern, "-test.count=1", "-test.v",
             f"-test.timeout={timeout}s"],
            cwd=directory, env=environment(arena), stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
            timeout=timeout + 30)
    except subprocess.TimeoutExpired as expired:
        return list(tests), (expired.stdout or b"").decode(errors="replace") + "\ntimed out\n"
    output = result.stdout.decode(errors="replace")
    missing = []
    for test in tests:
        # A top-level result line, not a subtest's: a test passes when every
        # run of it passes or skips and there is at least one.
        done = re.findall(r"^--- (?:PASS|SKIP): " + re.escape(test) + r" \(", output, re.MULTILINE)
        failed = re.findall(r"^--- FAIL: " + re.escape(test) + r" \(", output, re.MULTILINE)
        if not done or failed:
            missing.append(test)
    if result.returncode != 0 and not missing:
        missing = list(tests)
    return missing, output


def build(root, package, logs, race):
    """Builds one package's test binary into logs and reports its path."""
    binary = logs / (package.replace("/", "-") + ".test")
    result = subprocess.run(["go", "test", "-c"] + (["-race"] if race else []) + ["-o", str(binary), "./" + package],
                            cwd=root, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    if result.returncode != 0:
        sys.exit(f"building {package} failed:\n{result.stdout.decode(errors='replace')}")
    return binary


def survey(root, packages, jobs, timeout, race):
    """Runs every top-level test of each package alone under the zircon core,
    in both arena modes, and prints which pass."""
    logs = Path(tempfile.mkdtemp(prefix="pager-core-survey-"))
    work = []
    for package in packages:
        binary = build(root, package, logs, race)
        listing = subprocess.run([str(binary), "-test.list", ".*"], cwd=root / package,
                                 stdout=subprocess.PIPE, stderr=subprocess.STDOUT, check=True)
        for name in listing.stdout.decode().split():
            if re.fullmatch(r"(Test|Example)\w*", name):
                work.extend((package, binary, name, arena) for arena in ARENAS)

    def one(job):
        package, binary, name, arena = job
        missing, output = run(binary, root / package, [name], arena, timeout)
        if missing:
            (logs / f"{package.replace('/', '-')}.{name}.{arena}.log").write_text(output)
        return package, name, arena, not missing

    results = {}
    with concurrent.futures.ThreadPoolExecutor(max_workers=jobs) as pool:
        for package, name, arena, passed in pool.map(one, work):
            results.setdefault((package, name), {})[arena] = passed
    for (package, name), arenas in sorted(results.items()):
        state = "pass" if all(arenas.values()) else "FAIL " + ",".join(a for a, ok in arenas.items() if not ok)
        print(f"{state:22} {package} {name}")
    failed = sum(1 for arenas in results.values() if not all(arenas.values()))
    print(f"{len(results) - failed} of {len(results)} tests pass under the zircon core; logs of the rest in {logs}")
    if failed:
        sys.exit(1)
    shutil.rmtree(logs)


def main():
    parser = argparse.ArgumentParser(description=__doc__,
                                     formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--jobs", type=int, default=max(1, (os.cpu_count() or 2) // 2))
    parser.add_argument("--timeout", type=int, default=600, help="seconds per test process")
    parser.add_argument("--race", action="store_true", help="build the test binaries with the race detector")
    parser.add_argument("--survey", action="append", metavar="PACKAGE",
                        help="run every test of PACKAGE alone under the zircon core and report which pass")
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[1]
    if args.survey:
        survey(root, args.survey, args.jobs, args.timeout, args.race)
        return
    started = time.monotonic()
    logs = Path(tempfile.mkdtemp(prefix="pager-core-"))

    def one(arena):
        log = logs / f"{arena}.log"
        with log.open("wb") as out:
            result = subprocess.run(["go", "test", "-count=1"] + (["-race"] if args.race else []) +
                                    [f"-timeout={args.timeout}s", *SUITES],
                                    cwd=root, env=environment(arena), stdout=out, stderr=subprocess.STDOUT)
        return arena, result.returncode, log

    failures = []
    with concurrent.futures.ThreadPoolExecutor(max_workers=len(ARENAS)) as pool:
        for arena, code, log in pool.map(one, ARENAS):
            print(f"{'passed' if code == 0 else 'FAILED':6}  the pager's suites under the zircon core, {arena} arena",
                  flush=True)
            if code != 0:
                failures.append(f"SPROUTFS_PAGER_CORE=zircon SPROUTFS_ARENA={arena} go test {' '.join(SUITES)}"
                                f"   (log {log})")
    print(f"the pager's suites under the zircon core in both arena modes, {time.monotonic() - started:.0f}s")
    if not failures:
        shutil.rmtree(logs)
        return
    print("\n".join(failures), file=sys.stderr)
    sys.exit(f"{len(failures)} runs did not pass; logs in {logs}")


if __name__ == "__main__":
    main()
