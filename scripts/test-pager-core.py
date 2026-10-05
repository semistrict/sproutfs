#!/usr/bin/env python3
"""Run the tests the zircon pager core serves, under that core.

The pager's page layer is being ported from Zircon's (plans/zircon-pager-port-
2026-10-05.md). Its new core runs beside the old one, selected by
SPROUTFS_PAGER_CORE, and serves a growing part of what the old one does.
scripts/pager-core-zircon.json names, per package, the tests that already pass
under it. This builds each package's test binary once and runs those tests with
SPROUTFS_PAGER_CORE=zircon in both arena modes. It fails if a listed test fails,
or does not exist: a name that matches nothing would pass silently, and a list
that says a test runs under the new core must mean it.
"""

import argparse
import concurrent.futures
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import time

ARENAS = ("isolated", "shared")


def run(binary, directory, tests, arena, timeout):
    """Runs the listed tests of one package in one arena mode. It reports the
    tests that did not pass, and the output."""
    env = {k: v for k, v in os.environ.items() if not k.startswith("SPROUTFS_")}
    env["SPROUTFS_PAGER_CORE"] = "zircon"
    env["SPROUTFS_ARENA"] = arena
    pattern = "^(" + "|".join(re.escape(t) for t in tests) + ")$"
    try:
        result = subprocess.run(
            [str(binary), "-test.run=" + pattern, "-test.count=1", "-test.v",
             f"-test.timeout={timeout}s"],
            cwd=directory, env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
            timeout=timeout + 30)
    except subprocess.TimeoutExpired as expired:
        return list(tests), (expired.stdout or b"").decode(errors="replace") + "\ntimed out\n"
    output = result.stdout.decode(errors="replace")
    missing = []
    for test in tests:
        # A top-level result line, not a subtest's: a test passes when every
        # run of it passes and there is at least one.
        passed = re.findall(r"^--- PASS: " + re.escape(test) + r" \(", output, re.MULTILINE)
        failed = re.findall(r"^--- FAIL: " + re.escape(test) + r" \(", output, re.MULTILINE)
        if not passed or failed:
            missing.append(test)
    if result.returncode != 0 and not missing:
        missing = list(tests)
    return missing, output


def main():
    parser = argparse.ArgumentParser(description=__doc__,
                                     formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--jobs", type=int, default=max(1, (os.cpu_count() or 2) // 2))
    parser.add_argument("--timeout", type=int, default=600, help="seconds per test process")
    parser.add_argument("--race", action="store_true", help="build the test binaries with the race detector")
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[1]
    listed = json.loads((root / "scripts/pager-core-zircon.json").read_text())
    packages = {}
    for entry in listed:
        tests = packages.setdefault(entry["package"], [])
        for test in entry["tests"]:
            if test in tests:
                sys.exit(f"{test} of {entry['package']} is listed twice")
            tests.append(test)
    if not packages:
        print("no test is listed for the zircon core")
        return
    started = time.monotonic()
    logs = Path(tempfile.mkdtemp(prefix="pager-core-"))
    binaries = {}
    for package in sorted(packages):
        binary = logs / (package.replace("/", "-") + ".test")
        build = subprocess.run(["go", "test", "-c"] + (["-race"] if args.race else []) + ["-o", str(binary), "./" + package],
                               cwd=root, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        if build.returncode != 0:
            sys.exit(f"building {package} failed:\n{build.stdout.decode(errors='replace')}")
        binaries[package] = binary

    def one(job):
        package, arena = job
        missing, output = run(binaries[package], root / package, packages[package], arena, args.timeout)
        log = logs / f"{package.replace('/', '-')}.{arena}.log"
        log.write_text(output)
        return package, arena, missing, log

    jobs = [(package, arena) for package in sorted(packages) for arena in ARENAS]
    failures = []
    with concurrent.futures.ThreadPoolExecutor(max_workers=args.jobs) as pool:
        for package, arena, missing, log in pool.map(one, jobs):
            count = len(packages[package])
            print(f"{count - len(missing):4} of {count:4} passed  {package}, {arena} arena", flush=True)
            for test in missing:
                failures.append(f"SPROUTFS_PAGER_CORE=zircon SPROUTFS_ARENA={arena} "
                                f"go test ./{package} -run '^{test}$' -count=1   (log {log})")
    total = sum(len(t) for t in packages.values())
    print(f"{total} listed tests in {len(packages)} packages under the zircon core, "
          f"{time.monotonic() - started:.0f}s")
    if not failures:
        shutil.rmtree(logs)
        return
    print("\n".join(failures), file=sys.stderr)
    sys.exit(f"{len(failures)} listed test runs did not pass; logs in {logs}")


if __name__ == "__main__":
    main()
