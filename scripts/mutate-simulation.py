#!/usr/bin/env python3
"""Run the fault catalogue against the simulations, in a source copy.

Most of the catalogue is in the tree as sim.Bug guards, and those entries cost
one test invocation each with SPROUTFS_SIM_BUG naming the guard: no patching, no
rebuild, and no entry that silently stops matching when the code around it moves.
Only mutations no guard can express — a wrong behaviour in the simulated
dependencies themselves — are still applied as source edits.

Build failures and timeouts are reported separately, never counted as kills.
Survivors are also tested against the full affected package suites. This is a
fault catalogue, not an exhaustive mutation score or a coverage percentage.
"""

import argparse
import collections
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import time


SCHEDULED = {
    "internal/simtest": "TestScheduledWorldReproduces",
    "platform/sim": "TestDynamicOverlapTraceFiles",
}


def command(args, cwd, env, log, timeout):
    started = time.monotonic()
    with log.open("w") as output:
        try:
            result = subprocess.run(args, cwd=cwd, env=env, stdout=output,
                                    stderr=subprocess.STDOUT, timeout=timeout)
            status = "passed" if result.returncode == 0 else "failed"
        except subprocess.TimeoutExpired:
            status = "timeout"
    return {"status": status, "seconds": round(time.monotonic() - started, 3),
            "command": args, "log": str(log)}


def snapshot(root, target):
    names = subprocess.check_output(
        ["git", "ls-files", "-z", "--cached", "--others", "--exclude-standard"],
        cwd=root).decode().split("\0")
    hashes = {}
    for name in sorted(set(names)):
        path = Path(name)
        if not name or path.parts[0] == "third_party":
            continue
        if path.suffix != ".go" and path.name not in ("go.mod", "go.sum") and "testdata" not in path.parts:
            continue
        source = root / path
        if not source.is_file():
            continue
        data = source.read_bytes()
        destination = target / path
        destination.parent.mkdir(parents=True, exist_ok=True)
        destination.write_bytes(data)
        hashes[name] = hashlib.sha256(data).hexdigest()
    return hashes


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, help="new directory for source copy, binaries and evidence")
    parser.add_argument("--seeds", type=int, default=3)
    parser.add_argument("--mutant", action="append", help="select an ID; repeat to select several")
    parser.add_argument("--timeout", type=int, default=180, help="seconds per test process")
    args = parser.parse_args()
    if args.seeds < 1 or args.timeout < 1:
        parser.error("seeds and timeout must be positive")
    root = Path(__file__).resolve().parents[1]
    catalogue = json.loads((root / "scripts/mutation/simulation.json").read_text())
    guards = json.loads((root / "scripts/mutation/guards.json").read_text())
    if args.mutant:
        unknown = set(args.mutant) - {m["id"] for m in catalogue} - {g["id"] for g in guards}
        if unknown:
            parser.error(f"unknown mutant IDs: {sorted(unknown)}")
        catalogue = [m for m in catalogue if m["id"] in args.mutant]
        guards = [g for g in guards if g["id"] in args.mutant]
    output = args.output.resolve() if args.output else Path(tempfile.mkdtemp(prefix="simulation-mutations-"))
    if args.output:
        output.mkdir(parents=True, exist_ok=False)
    source = output / "source"
    source.mkdir()
    hashes = snapshot(root, source)
    (output / "source-sha256.json").write_text(json.dumps(hashes, indent=2) + "\n")
    (output / "catalogue.json").write_text(json.dumps(catalogue, indent=2) + "\n")
    (output / "guards.json").write_text(json.dumps(guards, indent=2) + "\n")
    # Do not inherit opt-in cloud/KVM paths, soak settings or trace destinations.
    env = {k: v for k, v in os.environ.items() if not k.startswith("SPROUTFS_")}
    env.update(SPROUTFS_OVERLAP_TRACE_SEEDS=str(args.seeds), GOMAXPROCS="4", GOWORK="off")
    packages = sorted({p for m in catalogue for p in m["packages"]} | {g["package"] for g in guards})
    report = {"seeds": args.seeds, "gomaxprocs": 4, "guards": [],
              "go": subprocess.check_output(["go", "version"], text=True).strip(),
              "revision": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=root, text=True).strip(),
              "source_manifest": "source-sha256.json", "baseline": [], "mutants": []}

    def save():
        report["summary"] = dict(collections.Counter(
            m["outcome"] for m in report["mutants"] + report["guards"]))
        (output / "report.json").write_text(json.dumps(report, indent=2) + "\n")

    def build(package, directory):
        binary = directory / (package.replace("/", "-") + ".test")
        result = command(["go", "test", "-c", "-o", str(binary), "./" + package],
                         source, env, binary.with_suffix(".build.log"), 180)
        return binary, result

    def run(binary, package, phase, directory, pattern=None, bug=None):
        if pattern is None:
            pattern = "^" + SCHEDULED[package] + "$" if phase == "scheduled" else "."
        environment = env if bug is None else {**env, "SPROUTFS_SIM_BUG": bug}
        name = f"{package.replace('/', '-')}.{phase}" + (f".{bug}" if bug else "")
        result = command([str(binary), "-test.run=" + pattern, "-test.count=1",
                          "-test.v", "-test.failfast", f"-test.timeout={args.timeout}s"],
                         source / package, environment, directory / f"{name}.log",
                         args.timeout + 5)
        log = Path(result["log"]).read_text(errors="replace")
        if "panic: test timed out" in log:
            result["status"] = "timeout"
        if result["status"] == "passed" and "=== RUN" not in log:
            result["status"] = "no-tests"
        if result["status"] == "failed" and "--- FAIL:" not in log and "panic:" not in log:
            result["status"] = "process-error"
        result["evidence"] = [line.strip() for line in log.splitlines()
                              if "--- FAIL:" in line or line.startswith("panic:")][:8]
        result.update(package=package, phase=phase)
        if bug:
            result["bug"] = bug
        return result

    print(f"Evidence: {output}", flush=True)
    baseline = output / "baseline"
    baseline.mkdir()
    for package in packages:
        binary, built = build(package, baseline)
        report["baseline"].append(built)
        if built["status"] != "passed":
            save()
            raise SystemExit(f"Baseline build failed: {built['log']}")
        phases = ["scheduled", "full"] if package in SCHEDULED else ["full"]
        for phase in phases:
            result = run(binary, package, phase, baseline)
            report["baseline"].append(result)
            if result["status"] != "passed":
                save()
                raise SystemExit(f"Baseline failed: {result['log']}")
    save()
    # The guards need no patched source and no build of their own: the baseline
    # binary already contains them, switched off, and the environment is what
    # turns exactly one of them on.
    guarded = output / "guards"
    guarded.mkdir()
    for guard in guards:
        package = guard["package"]
        binary, built = build(package, guarded)
        record = {"id": guard["id"], "purpose": guard["purpose"], "runs": [built], "outcome": "build-error"}
        report["guards"].append(record)
        if built["status"] == "passed":
            result = run(binary, package, "guard", guarded, pattern=guard["run"], bug=guard["id"])
            record["runs"].append(result)
            record["outcome"] = {"failed": "killed-guard", "passed": "survived"}.get(
                result["status"], result["status"] + "-guard")
        save()
        print(f"{guard['id']}: {record['outcome']}", flush=True)
    for mutant in catalogue:
        directory = output / mutant["id"]
        directory.mkdir()
        target = source / mutant["path"]
        original = target.read_text()
        record = {"id": mutant["id"], "purpose": mutant["purpose"], "runs": [], "outcome": "invalid"}
        report["mutants"].append(record)
        if original.count(mutant["before"]) != 1:
            record["reason"] = "mutation must match exactly once; source has changed"
            save()
            continue
        binaries = {}
        try:
            target.write_text(original.replace(mutant["before"], mutant["after"], 1))
            shutil.copyfile(target, directory / target.name)
            record["outcome"] = "survived"
            for phase in ("scheduled", "full"):
                for package in mutant["packages"]:
                    if phase == "scheduled" and package not in SCHEDULED:
                        continue
                    if package not in binaries:
                        binary, built = build(package, directory)
                        record["runs"].append(built)
                        if built["status"] != "passed":
                            record["outcome"] = "build-error"
                            break
                        binaries[package] = binary
                    result = run(binaries[package], package, phase, directory)
                    record["runs"].append(result)
                    if result["status"] != "passed":
                        record["outcome"] = (f"killed-{phase}" if result["status"] == "failed"
                                             else result["status"] + "-" + phase)
                        break
                if record["outcome"] != "survived":
                    break
        finally:
            target.write_text(original)
            save()
        print(f"{mutant['id']}: {record['outcome']}", flush=True)
    # Recheck the clean scheduled suites from freshly built binaries.
    report["restored"] = []
    restored = output / "restored"
    restored.mkdir()
    for package in packages:
        if package not in SCHEDULED:
            continue
        binary, built = build(package, restored)
        report["restored"].append(built)
        if built["status"] == "passed":
            report["restored"].append(run(binary, package, "scheduled", restored))
    save()
    print(json.dumps(report["summary"], indent=2))
    if any(r["status"] != "passed" for r in report["restored"]):
        raise SystemExit("Restored baseline failed")
    if any(m["outcome"] != "killed-guard" for m in report["guards"]):
        raise SystemExit(1)
    if any(m["outcome"] != "killed-scheduled" for m in report["mutants"]):
        raise SystemExit(1)


if __name__ == "__main__":
    main()
