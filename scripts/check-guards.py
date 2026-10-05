#!/usr/bin/env python3
"""Check that every in-tree bug guard is still killed by the tests it names.

scripts/mutation/guards.json names, for each sim.Bug guard, the package and the
tests that must fail with SPROUTFS_SIM_BUG set to that guard. A guard whose
tests pass protects nothing any more: the code moved, the tests stopped
reaching the guard, or the condition around it became impossible. This runs
each entry against a test binary built once per package and fails if any
entry's tests pass, so a guard cannot stop being proven without a run saying
so.

Each entry runs once with no guard on first, so a test that fails on its own
does not count as a kill. --repeat runs the guarded tests that many times, and
every run must fail. An entry with a "goos" runs only on that system, and one
with "root" only as root: its tests skip themselves anywhere else.

An entry runs under each pager core, current and zircon, while the zircon core
runs beside the current one, and must be killed under each; one whose "cores"
names fewer runs under those alone.
"""

import argparse
import concurrent.futures
import json
import os
from pathlib import Path
import platform
import shutil
import subprocess
import sys
import tempfile
import time


def run(binary, directory, pattern, bug, core, timeout):
    """Runs one test binary under one pager core and reports passed, failed,
    no-tests, timeout or process-error."""
    env = {k: v for k, v in os.environ.items() if not k.startswith("SPROUTFS_")}
    env["SPROUTFS_PAGER_CORE"] = core
    if bug:
        env["SPROUTFS_SIM_BUG"] = bug
    started = time.monotonic()
    try:
        result = subprocess.run(
            [str(binary), "-test.run=" + pattern, "-test.count=1", "-test.v",
             f"-test.timeout={timeout}s"],
            cwd=directory, env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
            timeout=timeout + 30)
    except subprocess.TimeoutExpired as expired:
        return "timeout", time.monotonic() - started, (expired.stdout or b"").decode(errors="replace")
    output = result.stdout.decode(errors="replace")
    if "panic: test timed out" in output:
        status = "timeout"
    elif result.returncode == 0:
        status = "passed" if "--- PASS:" in output else "no-tests"
    else:
        status = "failed" if ("--- FAIL:" in output or "panic:" in output) else "process-error"
    return status, time.monotonic() - started, output


def check(guards, root, logs, repeat, jobs, timeout):
    """Builds each package's test binary once, then runs every entry. It
    prints each entry's outcome as it lands and returns the entries that were
    not killed."""
    binaries = {}
    for package in sorted({g["package"] for g in guards}):
        binary = logs / (package.replace("/", "-") + ".test")
        build = subprocess.run(["go", "test", "-c", "-o", str(binary), "./" + package],
                               cwd=root, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        if build.returncode != 0:
            sys.exit(f"building {package} failed:\n{build.stdout.decode(errors='replace')}")
        binaries[package] = binary

    def one(job):
        guard, core = job
        binary, directory = binaries[guard["package"]], root / guard["package"]
        name = f"{guard['id']}.{core}"
        status, seconds, output = run(binary, directory, guard["run"], None, core, timeout)
        if status != "passed":
            (logs / f"{name}.clean.log").write_text(output)
            return guard, core, f"clean-{status}", seconds
        for attempt in range(1, repeat + 1):
            status, took, output = run(binary, directory, guard["run"], guard["id"], core, timeout)
            seconds += took
            (logs / f"{name}.{attempt}.log").write_text(output)
            if status != "failed":
                return guard, core, {"passed": "SURVIVED"}.get(status, status), seconds
        return guard, core, "killed", seconds

    failures = []
    jobs_list = [(guard, core) for guard in guards for core in cores(guard)]
    with concurrent.futures.ThreadPoolExecutor(max_workers=jobs) as pool:
        for guard, core, outcome, seconds in pool.map(one, jobs_list):
            print(f"{outcome:>16} {seconds:7.1f}s  {guard['id']} ({core} core)", flush=True)
            if outcome != "killed":
                failures.append((guard, core, outcome))
    return failures, len(jobs_list)


def cores(guard):
    """The pager cores a guard is checked under."""
    return guard.get("cores", ["current", "zircon"])


def main():
    parser = argparse.ArgumentParser(description=__doc__,
                                     formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--guard", action="append", help="check only this ID; repeat to check several")
    parser.add_argument("--repeat", type=int, default=1, help="guarded runs per entry, each of which must fail")
    parser.add_argument("--jobs", type=int, default=max(1, (os.cpu_count() or 2) // 2))
    parser.add_argument("--timeout", type=int, default=300, help="seconds per test process")
    parser.add_argument("--logs", type=Path,
                        help="directory for binaries and logs; default a temporary one, kept only if a check fails")
    args = parser.parse_args()
    if args.repeat < 1 or args.jobs < 1 or args.timeout < 1:
        parser.error("repeat, jobs and timeout must be positive")
    root = Path(__file__).resolve().parents[1]
    guards = json.loads((root / "scripts/mutation/guards.json").read_text())
    ids = [g["id"] for g in guards]
    duplicates = sorted({i for i in ids if ids.count(i) > 1})
    if duplicates:
        parser.error(f"duplicate guard IDs: {duplicates}")
    unknown_cores = sorted({c for g in guards for c in cores(g)} - {"current", "zircon"})
    if unknown_cores:
        parser.error(f"unknown pager cores: {unknown_cores}")
    if args.guard:
        unknown = set(args.guard) - set(ids)
        if unknown:
            parser.error(f"unknown guard IDs: {sorted(unknown)}")
        guards = [g for g in guards if g["id"] in args.guard]
    system, as_root = platform.system().lower(), os.geteuid() == 0

    def runnable(guard):
        return guard.get("goos", system) == system and (as_root or not guard.get("root", False))

    skipped = [g for g in guards if not runnable(g)]
    guards = [g for g in guards if runnable(g)]
    logs = args.logs or Path(tempfile.mkdtemp(prefix="guards-"))
    logs.mkdir(parents=True, exist_ok=True)
    started = time.monotonic()

    failures, runs = check(guards, root, logs, args.repeat, args.jobs, args.timeout)
    for guard in skipped:
        needs = " as root" if guard.get("root") else ""
        print(f"{'skipped':>16} {'':8}  {guard['id']} (runs on {guard.get('goos', system)}{needs} only)")
    repeated = f" in each of {args.repeat} runs" if args.repeat > 1 else ""
    print(f"{runs - len(failures)} of {runs} guard runs killed{repeated} "
          f"({len(guards)} guards), "
          f"{len(skipped)} skipped, {time.monotonic() - started:.0f}s")
    if not failures:
        if args.logs is None:
            shutil.rmtree(logs)
        return
    for guard, core, outcome in failures:
        print(f"{outcome}: SPROUTFS_PAGER_CORE={core} SPROUTFS_SIM_BUG={guard['id']} "
              f"go test ./{guard['package']} -run '{guard['run']}' -count=1", file=sys.stderr)
    sys.exit(f"logs in {logs}")


if __name__ == "__main__":
    main()
