#!/usr/bin/env python3
"""Compare protobuf traces from independent processes running the prototype.

Full traces retain goroutine arrival order. Execution traces omit only submitted
and ready events, then renumber; events are never sorted to manufacture a match.
Exits nonzero for any full-trace mismatch, even when execution traces match.
"""

import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--runs", type=int, default=10, help="processes per GOMAXPROCS setting")
    parser.add_argument("--scenario", choices=["batch", "dynamic", "world"], default="dynamic")
    parser.add_argument("--seeds", type=int, default=32)
    parser.add_argument("--maxprocs", type=int, nargs="+", default=[1, 2, 4, 8])
    parser.add_argument("--output", type=Path, help="new directory to retain traces and report")
    args = parser.parse_args()
    if args.runs < 1 or args.seeds < 1 or any(n < 1 for n in args.maxprocs) or len(set(args.maxprocs)) != len(args.maxprocs):
        parser.error("runs and seeds must be positive; maxprocs must contain distinct positive values")
    output = args.output.resolve() if args.output else Path(tempfile.mkdtemp(prefix="overlap-traces-"))
    if args.output:
        output.mkdir(parents=True, exist_ok=False)
    root = Path(__file__).resolve().parents[1]
    report = {"processes": 0, "full_comparisons": 0, "full_mismatches": 0,
              "execution_comparisons": 0, "execution_mismatches": 0,
              "adapter_comparisons": 0, "adapter_mismatches": 0,
              "scenario": args.scenario, "seeds": args.seeds, "first_full_difference": None, "runs": []}
    test_name = {"batch": "TestOverlapPrototypeTraceFiles", "dynamic": "TestDynamicOverlapTraceFiles",
                 "world": "TestScheduledWorldReproduces"}[args.scenario]
    package = {"world": "./internal/simtest"}.get(args.scenario, "./platform/sim")
    baseline = None
    print(f"Traces: {output}", flush=True)
    with tempfile.TemporaryDirectory(prefix="overlap-test-bin-") as build:
        binary = str(Path(build) / "sim.test")
        subprocess.run(["go", "test", "-c", "-o", binary, package], cwd=root, check=True)
        for procs in args.maxprocs:
            for repetition in range(args.runs):
                run_dir = output / f"procs-{procs}-run-{repetition:02d}"
                env = os.environ | {"GOMAXPROCS": str(procs), "SPROUTFS_OVERLAP_TRACE_DIR": str(run_dir),
                                       "SPROUTFS_OVERLAP_TRACE_SEEDS": str(args.seeds)}
                result = subprocess.run([binary, f"-test.run=^{test_name}$",
                                         "-test.count=1", "-test.timeout=30s"],
                                        cwd=root, env=env, text=True, capture_output=True, timeout=45)
                run_dir.mkdir(parents=True, exist_ok=True)
                (run_dir / "test.log").write_text(result.stdout + result.stderr)
                if result.returncode:
                    raise SystemExit(f"Test failed; inspect {run_dir / 'test.log'}")
                files = sorted(run_dir.glob("*.pb"))
                # Seeds x 2 creation orders x trace formats.
                expected_files = args.seeds * 2 * (2 if args.scenario == "batch" else 3)
                if len(files) != expected_files:
                    raise SystemExit(f"Incomplete trace set in {run_dir}: {len(files)} files")
                hashes = {p.name: hashlib.sha256(p.read_bytes()).hexdigest() for p in files}
                report["runs"].append({"directory": run_dir.name, "gomaxprocs": procs, "sha256": hashes})
                report["processes"] += 1
                if baseline is None:
                    baseline = run_dir
                    continue
                for path in files:
                    kind = ("execution" if path.name.endswith(".execution.pb") else
                            "adapter" if path.name.endswith(".adapter.pb") else "full")
                    report[f"{kind}_comparisons"] += 1
                    # Compare exact stored bytes, not hashes or sorted events.
                    if path.read_bytes() == (baseline / path.name).read_bytes():
                        continue
                    report[f"{kind}_mismatches"] += 1
                    if kind == "full" and report["first_full_difference"] is None:
                        before = (baseline / path.with_suffix(".txt").name).read_text().splitlines()
                        after = path.with_suffix(".txt").read_text().splitlines()
                        index = next((i for i, pair in enumerate(zip(before, after)) if pair[0] != pair[1]),
                                     min(len(before), len(after)))
                        report["first_full_difference"] = {
                            "baseline": str(baseline / path.name), "actual": str(path),
                            "event": index + 1,
                            "before": before[index] if index < len(before) else "<end>",
                            "after": after[index] if index < len(after) else "<end>"}
            print(f"Completed {args.runs} processes with GOMAXPROCS={procs}", flush=True)
    (output / "report.json").write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps({k: v for k, v in report.items() if k != "runs"}, indent=2))
    if report["execution_mismatches"] or report["full_mismatches"] or report["adapter_mismatches"]:
        raise SystemExit(1)


if __name__ == "__main__":
    main()
