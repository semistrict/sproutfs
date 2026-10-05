#!/usr/bin/env python3
"""Record every file under zircon/kernel at the revision the pager's Zircon port
is taken from, with the copyright lines of its header.

    python3 scripts/zircon-sources.py ~/src/fuchsia

The files are read from the checkout's objects at that revision, so its working
tree may be sparse or at another commit. The list goes to
vmmemory/internal/zirconvm/testdata/zircon-kernel-files.txt. The package's
header test reads it: a ported file may name only a file on the list, and must
carry that file's copyright lines. The test runs where no fuchsia checkout
exists, so it reads this list instead of the tree.
"""

import os
from pathlib import Path
import re
import subprocess
import sys

REVISION = "90e54e090540aaac37a81494884f4b8a68bc269c"
ROOT = Path(__file__).resolve().parent.parent
OUT = ROOT / "vmmemory/internal/zirconvm/testdata/zircon-kernel-files.txt"

# A copyright line in a header, after its comment marker: C and C++ (//, /*,
# *), GN, Python and shell (#), and assembly (;).
COPYRIGHT = re.compile(r"^\s*(?://|/\*|\*|#|;)?\s*(Copyright\b.*?)\s*(?:\*/)?\s*$")


def main() -> int:
    if len(sys.argv) != 2:
        print("usage: scripts/zircon-sources.py <fuchsia checkout>", file=sys.stderr)
        return 2
    repo = ["git", "-C", str(Path(sys.argv[1]).expanduser())]
    # Each entry is "<mode> <kind> <object>\t<path>".
    entries = subprocess.run(
        [*repo, "ls-tree", "-r", "-z", REVISION, "zircon/kernel/"],
        check=True, capture_output=True,
    ).stdout.split(b"\0")
    blobs = {}
    for entry in filter(None, entries):
        meta, path = entry.split(b"\t", 1)
        _, kind, obj = meta.split()
        if kind == b"blob":
            blobs[path.decode().removeprefix("zircon/kernel/")] = obj.decode()
    # One cat-file reads every blob, in the order they are asked for. Each
    # comes back as "<object> blob <size>\n<content>\n". A partial clone may
    # not hold a blob; it comes back as "<object> missing\n" and is listed
    # with no copyright lines rather than fetched.
    order = sorted(blobs)
    batch = subprocess.run(
        [*repo, "cat-file", "--batch"],
        input="".join(blobs[p] + "\n" for p in order).encode(),
        check=True, capture_output=True,
        env={**os.environ, "GIT_NO_LAZY_FETCH": "1"},
    ).stdout
    lines = [
        f"# Every file under zircon/kernel at fuchsia {REVISION},",
        "# with the copyright lines of its header, tab-separated. Written by",
        "# scripts/zircon-sources.py; do not edit.",
    ]
    at = 0
    missing = 0
    for path in order:
        newline = batch.index(b"\n", at)
        reply = batch[at:newline].split()
        if reply[1] == b"missing":
            at = newline + 1
            missing += 1
            lines.append(path)
            continue
        size = int(reply[2])
        content = batch[newline + 1 : newline + 1 + size]
        at = newline + 1 + size + 1
        header = content.decode("utf-8", errors="replace").splitlines()[:20]
        copyrights = [m.group(1) for m in map(COPYRIGHT.match, header) if m]
        lines.append("\t".join([path, *copyrights]))
    OUT.write_text("\n".join(lines) + "\n")
    print(f"wrote {len(order)} files to {OUT.relative_to(ROOT)}, "
          f"{missing} of them not in the clone and listed without copyright lines")
    return 0


if __name__ == "__main__":
    sys.exit(main())
