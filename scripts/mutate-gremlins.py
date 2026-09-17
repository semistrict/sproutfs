#!/usr/bin/env python3
"""Run Gremlins on a source snapshot, for one package or the whole Go project."""

import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--gremlins", default="gremlins", help="Gremlins executable (qualified with v0.6.0)")
    scope = parser.add_mutually_exclusive_group()
    scope.add_argument("--package", help="package whose production code is mutated (default: internal/vmmigrate)")
    scope.add_argument("--all", action="store_true", help="mutate all first-party Go packages")
    parser.add_argument("--suite", choices=("scheduled", "full"),
                        help="default: full with --all, otherwise scheduled")
    parser.add_argument("--integration", action="store_true",
                        help="run all packages' tests for each mutation (always enabled for scheduled)")
    parser.add_argument("--go-test-exec", type=Path,
                        help="explicit Go test launcher, for example a prepared Linux VM environment")
    parser.add_argument("--timeout-coefficient", type=int,
                        help="Gremlins baseline time multiplier (default: 3 full, 10 scheduled)")
    parser.add_argument("--seeds", type=int, default=3)
    parser.add_argument("--workers", type=int, default=4)
    parser.add_argument("--dry-run", action="store_true")
    parser.add_argument("--output", type=Path, help="new directory for source and results")
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[1]
    package = Path("." if args.all else args.package or "internal/vmmigrate")
    if package.is_absolute() or ".." in package.parts or not (root / package).is_dir():
        parser.error("package must name a directory inside this repository")
    suite = args.suite or ("full" if args.all else "scheduled")
    integration = args.integration or suite == "scheduled"
    coefficient = args.timeout_coefficient if args.timeout_coefficient is not None else (10 if suite == "scheduled" else 3)
    if args.seeds < 1 or args.workers < 1 or coefficient < 1:
        parser.error("seeds, workers and timeout coefficient must be positive")
    launcher = args.go_test_exec.resolve() if args.go_test_exec else None
    if launcher and (not launcher.is_file() or not os.access(launcher, os.X_OK)
                     or any(char.isspace() for char in str(launcher))):
        parser.error("go-test-exec must name an executable file whose path has no whitespace")
    tool = shutil.which(args.gremlins)
    if tool is None:
        parser.error("install Gremlins or pass --gremlins /absolute/path/to/gremlins")
    tool = str(Path(tool).resolve())
    output = args.output.resolve() if args.output else Path(tempfile.mkdtemp(prefix="gremlins-simulation-"))
    if args.output:
        output.mkdir(parents=True, exist_ok=False)
    source = output / "source"
    source.mkdir()
    # Reuse only the source-copy helper. Gremlins generates and executes every
    # mutation in this campaign; the curated catalogue is not involved.
    sys.dont_write_bytecode = True
    spec = importlib.util.spec_from_file_location("source_snapshot", root / "scripts/mutate-simulation.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    hashes = module.snapshot(root, source)
    (output / "source-sha256.json").write_text(json.dumps(hashes, indent=2) + "\n")
    env = {k: v for k, v in os.environ.items() if not k.startswith(("SPROUTFS_", "GREMLINS_"))}
    flags = "-count=1 -timeout=2m -json"
    if launcher:
        flags += " -exec=" + str(launcher)
    if suite == "scheduled":
        flags += " -run=^TestScheduled.*Reproduces$"
    env.update(GOFLAGS=flags, GOMAXPROCS="4",
               GOWORK="off", SPROUTFS_OVERLAP_TRACE_SEEDS=str(args.seeds))
    real_go = shutil.which("go")
    if real_go is None:
        parser.error("Go must be available on PATH")
    audit_directory, audit_bin = output / "audit", output / "audit-bin"
    audit_directory.mkdir()
    audit_bin.mkdir()
    audit_script = root / "scripts/mutation/gremlins-go-audit.py"
    shutil.copyfile(audit_script, audit_bin / "go")
    (audit_bin / "go").chmod(0o755)
    env.update(PATH=str(audit_bin) + os.pathsep + env.get("PATH", ""),
               GREMLINS_AUDIT_GO=str(Path(real_go).resolve()),
               GREMLINS_AUDIT_SOURCE=str(source), GREMLINS_AUDIT_DIR=str(audit_directory),
               GREMLINS_AUDIT_MANIFEST=str(output / "source-sha256.json"))
    # Gremlins takes a directory, not a Go package pattern: ./... silently
    # produces no mutations. The module root recursively includes all packages.
    command = [tool, "unleash", str(package), "--exclude-files=internal/gen/",
               f"--workers={args.workers}", f"--timeout-coefficient={coefficient}",
               "--output=" + str(output / "results.json")]
    if integration:
        command.extend(["--integration", "--coverpkg=" + ("./..." if args.all else "./" + str(package))])
    if args.dry_run:
        command.append("--dry-run")
    metadata = {"command": command, "suite": suite, "integration": integration,
                "go": subprocess.check_output(["go", "version"], text=True).strip(),
                "gremlins": subprocess.check_output([tool, "--version"], text=True).strip(),
                "audit_helper_sha256": hashlib.sha256((audit_bin / "go").read_bytes()).hexdigest(),
                "environment": {k: env[k] for k in ("GOFLAGS", "GOMAXPROCS", "GOWORK", "SPROUTFS_OVERLAP_TRACE_SEEDS")}}
    if launcher:
        shutil.copyfile(launcher, output / "go-test-exec")
        (output / "go-test-exec").chmod(0o755)
        metadata["go_test_exec"] = {"path": str(launcher),
                                    "sha256": hashlib.sha256(launcher.read_bytes()).hexdigest()}
    (output / "run.json").write_text(json.dumps(metadata, indent=2) + "\n")
    with (output / "packages.json").open("w") as inventory:
        subprocess.run(["go", "list", "-json", "./..."], cwd=source, env=env,
                       stdout=inventory, check=True)
    print(f"Evidence: {output}", flush=True)
    with (output / "run.log").open("w") as log:
        run = subprocess.run(command, cwd=source, env=env, stdout=log, stderr=subprocess.STDOUT)
    metadata["exit_code"] = run.returncode
    metadata["source_unchanged"] = all(
        hashlib.sha256((source / name).read_bytes()).hexdigest() == digest for name, digest in hashes.items())
    (output / "run.json").write_text(json.dumps(metadata, indent=2) + "\n")
    print((output / "run.log").read_text(), end="")
    if not metadata["source_unchanged"]:
        raise SystemExit("Gremlins changed the source snapshot; inspect it before reuse")
    if run.returncode == 0:
        results_path = output / "results.json"
        if not results_path.exists() or not json.loads(results_path.read_text()).get("files"):
            raise SystemExit("Gremlins produced no mutation results; inspect the scope and coverage")
        spec = importlib.util.spec_from_file_location("go_audit", audit_script)
        audit = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(audit)
        summary = audit.summarize(audit_directory, json.loads(results_path.read_text()))
        (output / "audit-summary.json").write_text(json.dumps(summary, indent=2) + "\n")
        print("Audited outcomes:", json.dumps(summary["audited_outcomes"], sort_keys=True))
        if not summary["reconciled"]:
            raise SystemExit("Go audit does not reconcile with Gremlins; inspect incomplete invocations")
    raise SystemExit(run.returncode)


if __name__ == "__main__":
    main()
