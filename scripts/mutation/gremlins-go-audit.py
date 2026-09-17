#!/usr/bin/env python3
"""Record Gremlins' real Go invocations without changing their arguments/results."""

import collections
import difflib
import json
import os
from pathlib import Path
import subprocess
import sys
import time


def classify(log, code):
    failures, ran, passed, skipped, build_error = [], 0, 0, 0, False
    for line in log.splitlines():
        try:
            event = json.loads(line)
        except ValueError:
            continue
        ran += event.get("Action") == "run" and "Test" in event
        passed += event.get("Action") == "pass" and "Test" in event
        skipped += event.get("Action") == "skip" and "Test" in event
        if event.get("Action") == "fail" and "Test" in event:
            failures.append(event.get("Package", "") + ":" + event["Test"])
        build_error |= event.get("Action") == "build-fail"
    if "panic: test timed out" in log or "testrepro: child deadline exceeded:" in log:
        status = "timeout"
    elif "testrepro: child exited on a signal:" in log:
        status = "process-error"
    elif build_error or "[build failed]" in log:
        status = "build-error"
    elif code == 0:
        status = "passed-with-tests" if passed else "no-tests"
    elif failures or "panic:" in log or "fatal error:" in log:
        status = "test-failure"
    else:
        status = "process-error"
    return {"status": status, "tests_run": ran, "tests_passed": passed,
            "tests_skipped": skipped, "failures": failures}


def save(path, record):
    temporary = path.with_suffix(".pending")
    temporary.write_text(json.dumps(record, indent=2) + "\n")
    temporary.replace(path)


def summarize(directory, native):
    records = [json.loads(path.read_text()) for path in sorted(directory.glob("*.json"))]
    mutants = [record for record in records if record["changes"]]
    statuses = collections.Counter(
        mutation["status"] for file in native.get("files", []) for mutation in file["mutations"])
    executed = sum(statuses[status] for status in ("KILLED", "LIVED", "TIMED OUT", "NOT VIABLE"))
    interrupted = sum("exit_code" not in record for record in mutants)
    # Gremlins' watchdog kills the shim before it can record an exit. Only
    # identify these as timeouts when both independent counts reconcile.
    reconciled = len(mutants) == executed and interrupted == statuses["TIMED OUT"]
    counts = collections.Counter(record.get("status", "timeout" if reconciled else "interrupted") for record in mutants)
    return {"native_statuses": dict(statuses), "audited_outcomes": dict(counts),
            "executed_mutations": executed, "audited_mutations": len(mutants),
            "reconciled": reconciled,
            "baseline": [{key: record[key] for key in ("command", "status", "tests_run") if key in record}
                         for record in records if not record["changes"]]}


def main():
    real_go = os.environ["GREMLINS_AUDIT_GO"]
    if len(sys.argv) < 2 or sys.argv[1] != "test":
        os.execv(real_go, [real_go, *sys.argv[1:]])
    cwd = Path.cwd()
    module = next(path for path in (cwd, *cwd.parents) if (path / "go.mod").is_file())
    original = Path(os.environ["GREMLINS_AUDIT_SOURCE"])
    names = json.loads(Path(os.environ["GREMLINS_AUDIT_MANIFEST"]).read_text())
    record = {"command": [real_go, *sys.argv[1:]], "cwd": str(cwd), "changes": []}
    for name in names:
        if not name.endswith(".go") or name.endswith("_test.go") or "internal/gen/" in name:
            continue
        before, after = (original / name).read_text(), (module / name).read_text()
        if before != after:
            record["changes"].append({"path": name, "source": after,
                "diff": "".join(difflib.unified_diff(before.splitlines(True), after.splitlines(True),
                                                   fromfile=name, tofile=name))})
    stem = f"{time.time_ns()}-{os.getpid()}"
    directory = Path(os.environ["GREMLINS_AUDIT_DIR"])
    path, log_path = directory / (stem + ".json"), directory / (stem + ".log")
    record["log"] = log_path.name
    save(path, record)
    with log_path.open("wb", buffering=0) as log:
        process = subprocess.Popen(record["command"], stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        for line in process.stdout:
            log.write(line)
            sys.stdout.buffer.write(line)
            sys.stdout.buffer.flush()
        code = process.wait()
    record.update(exit_code=code, **classify(log_path.read_text(errors="replace"), code))
    save(path, record)
    sys.exit(code)


if __name__ == "__main__":
    main()
