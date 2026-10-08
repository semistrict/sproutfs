#!/usr/bin/env python3
"""Check that every error a boundary interface can return is simulated.

A deterministic simulation finds only the faults it injects. On 2026-10-08 an
embedder's VM died of a mapping refusal the pager mishandled, although the
campaigns had run for weeks: the test client refused only where one test
switched it on, so no campaign ever took the paths a refusal leads down. The
first campaign run with a random refusal found the bug.

So each interface the system calls across a process or I/O boundary is listed
in a manifest under scripts/faults/, one file per area, and every method of it
has entries: each error the method can return, with the sim.Buggify site that
returns it at random in the simulated implementation and the campaign that must
fire it, or "none" with the reason the method cannot fail. An entry may be
"deferred" to a backlog task, which names why its site is not fired yet: it is
checked against the source and listed, and its campaign is not run. A deferred
entry whose task is to write the site names no site and no campaign. This
fails where:

  - a listed interface has a method with no entry, or an entry names a method
    the interface no longer has;
  - a site is not a string literal in any Buggify call in the tree;
  - a campaign, run with SPROUTFS_FIRED_SITES set, never fires a site its
    entries name (platform/sim appends every site a run fires to that file).

--static skips running the campaigns. --file checks one manifest.
"""

import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile

ROOT = Path(__file__).resolve().parent.parent
BUGGIFY = re.compile(r'(?:Buggify|BuggifyDelay|buggifyHere)\(\s*[^,]*?,?\s*"([^"]+)"')
# A site whose id is a literal prefix and a name the caller passes, as
# sim.Buggify(ctx, "client-command-lost/"+command, p), names the prefix here,
# and each of its sites is the prefix and a string literal of the same file.
BUGGIFY_PREFIX = re.compile(r'(?:Buggify|BuggifyDelay|buggifyHere)\(\s*[^,]*?,?\s*"([^"]+/)"\s*\+')
# A site named by a constant, as sim.Buggify(ctx, buggifyShardOpenFails, p) or
# r.buggifyHere(SiteRandomClose, p), names the constant's value: a string
# constant of the same package.
BUGGIFY_NAMED = re.compile(r'(?:Buggify|BuggifyDelay|buggifyHere)\(\s*(?:[^,()"]*,\s*)?([A-Za-z_]\w*)\s*,')
CONSTANT = re.compile(r'^\s*([A-Za-z_]\w*)\s*=\s*"([^"]+)"', re.M)


def interface_methods(package, name):
    """The methods of interface name in package, from its source: one per line
    of the form `\tMethod(`. An embedded interface is reported as itself. A
    concrete type, a client the system calls across the boundary with no
    interface in front of it, is reported with its exported methods."""
    pattern = re.compile(r"^type " + re.escape(name) + r" interface \{\s*$")
    sources = [path for path in sorted((ROOT / package).glob("*.go")) if not path.name.endswith("_test.go")]
    for path in sources:
        lines = path.read_text().splitlines()
        for index, line in enumerate(lines):
            if not pattern.match(line):
                continue
            methods = []
            for body in lines[index + 1:]:
                if body.startswith("}"):
                    return methods
                found = re.match(r"^\t([A-Z]\w*)(\(|$)", body)
                if found:
                    methods.append(found.group(1))
            return methods
    method = re.compile(r"^func \(\w+ \*?" + re.escape(name) + r"\) ([A-Z]\w*)\(", re.MULTILINE)
    methods = sorted({m for path in sources for m in method.findall(path.read_text())})
    return methods or None


def sites_in_tree():
    """Every site id that is a string literal in a Buggify call, a literal
    prefix of one followed by a string literal of the same file, or the value
    of a string constant of the same package the call names."""
    sites = set()
    constants = {}
    named = []
    for path in ROOT.rglob("*.go"):
        parts = path.relative_to(ROOT).parts
        if "third_party" in parts or ".claude" in parts:
            continue
        text = path.read_text(errors="replace")
        sites.update(BUGGIFY.findall(text))
        literals = set(re.findall(r'"([A-Za-z0-9_./-]+)"', text))
        for prefix in BUGGIFY_PREFIX.findall(text):
            sites.update(prefix + name for name in literals)
        if not path.name.endswith("_test.go"):
            constants.setdefault(path.parent, {}).update(CONSTANT.findall(text))
            named.extend((path.parent, name) for name in BUGGIFY_NAMED.findall(text))
    for package, name in named:
        if name in constants.get(package, {}):
            sites.add(constants[package][name])
    return sites


def load(files):
    manifests = []
    for path in files:
        manifest = json.loads(path.read_text())
        manifest["path"] = path
        manifests.append(manifest)
    return manifests


def check_static(manifests):
    problems = []
    sites = sites_in_tree()
    for manifest in manifests:
        where = manifest["path"].relative_to(ROOT)
        entries = manifest.get("faults", [])
        for interface in manifest.get("interfaces", []):
            key = f'{interface["package"]}.{interface["name"]}'
            methods = interface_methods(interface["package"], interface["name"])
            if methods is None:
                problems.append(f"{where}: no interface {key} in {interface['package']}")
                continue
            covered = {e["method"] for e in entries if e["interface"] == key}
            for method in methods:
                if method not in covered:
                    problems.append(f"{where}: {key}.{method} has no entry: list each error it can return, "
                                    f'or "none" with why it cannot fail')
            for method in sorted(covered - set(methods)):
                problems.append(f"{where}: {key} has no method {method} any more")
        declared = {f'{i["package"]}.{i["name"]}' for i in manifest.get("interfaces", [])}
        for entry in entries:
            if entry["interface"] not in declared:
                problems.append(f'{where}: an entry names {entry["interface"]}, which this file does not list')
            if "none" in entry:
                continue
            if "deferred" in entry and not re.match(r"TASK-\d+(\.\d+)*: ", entry["deferred"]):
                problems.append(f'{where}: {entry["interface"]}.{entry["method"]} is deferred without '
                                f'"TASK-n: why"')
            # A deferred entry whose task is to write the site names none yet.
            fields = ("error", "purpose") if "deferred" in entry and "site" not in entry else \
                ("error", "site", "package", "run", "purpose")
            for field in fields:
                if not entry.get(field):
                    problems.append(f'{where}: {entry["interface"]}.{entry["method"]} lacks "{field}"')
            if entry.get("site") and entry["site"] not in sites:
                problems.append(f'{where}: site {entry["site"]} ({entry["interface"]}.{entry["method"]} '
                                f'{entry.get("error")}) is in no Buggify call')
    return problems


def check_fired(manifests):
    """Runs each entry's campaign once per (package, run, env) with the fired
    sites recorded, and reports the sites it never fired."""
    problems = []
    runs = {}
    for manifest in manifests:
        for entry in manifest.get("faults", []):
            if "none" in entry or "deferred" in entry:
                continue
            key = (entry["package"], entry["run"], json.dumps(entry.get("env", {}), sort_keys=True))
            runs.setdefault(key, []).append(entry)
    with tempfile.TemporaryDirectory(prefix="check-faults-") as scratch:
        for index, ((package, pattern, env_json), entries) in enumerate(sorted(runs.items())):
            fired_path = Path(scratch) / f"fired-{index}"
            env = {k: v for k, v in os.environ.items() if not k.startswith("SPROUTFS_")}
            env.update(json.loads(env_json))
            env["SPROUTFS_FIRED_SITES"] = str(fired_path)
            command = ["go", "test", "./" + package, "-run", pattern, "-count=1"]
            print(f"  {package} -run {pattern} {env_json if env_json != '{}' else ''}".rstrip(), flush=True)
            result = subprocess.run(command, cwd=ROOT, env=env, stdout=subprocess.PIPE,
                                    stderr=subprocess.STDOUT)
            if result.returncode != 0:
                problems.append(f"{package} -run {pattern} failed:\n{result.stdout.decode(errors='replace')}")
                continue
            fired = set(fired_path.read_text().split()) if fired_path.exists() else set()
            for entry in entries:
                if entry["site"] not in fired:
                    problems.append(f'{entry["site"]} ({entry["interface"]}.{entry["method"]} '
                                    f'{entry["error"]}) never fired in {package} -run {pattern}')
    return problems


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--static", action="store_true", help="check the manifests against the source only")
    parser.add_argument("--file", action="append", help="check only this manifest (repeatable)")
    args = parser.parse_args()
    files = [Path(f).resolve() for f in args.file] if args.file else sorted((ROOT / "scripts/faults").glob("*.json"))
    manifests = load(files)
    problems = check_static(manifests)
    if not problems and not args.static:
        problems = check_fired(manifests)
    for problem in problems:
        print(problem, file=sys.stderr)
    deferred = [e for m in manifests for e in m.get("faults", []) if "deferred" in e]
    for entry in deferred:
        print(f'deferred: {entry["interface"]}.{entry["method"]} {entry["error"]}: {entry["deferred"]}')
    faults = sum(1 for m in manifests for e in m.get("faults", []) if "none" not in e and "deferred" not in e)
    interfaces = sum(len(m.get("interfaces", [])) for m in manifests)
    if problems:
        sys.exit(f"{len(problems)} problems in {len(files)} manifests")
    print(f"{faults} faults of {interfaces} interfaces simulated and fired, {len(deferred)} deferred "
          f"({len(files)} manifests)")


if __name__ == "__main__":
    main()
