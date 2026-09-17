#!/usr/bin/env python3
"""Run a memory command in one isolated plain Firecracker guest on Linux."""

import argparse
import json
import os
import pathlib
import select
import subprocess
import tempfile
import time


def emit(record):
    print(json.dumps(record), flush=True)


def run(args, work):
    root = work / "root.ext4"
    subprocess.run(
        ["cp", "--reflink=auto", "--sparse=always", args.root, str(root)], check=True
    )
    config = work / "config.json"
    config.write_text(json.dumps({
        "machine-config": {
            "mem_size_mib": args.memory,
            "vcpu_count": args.vcpus,
            "huge_pages": args.huge_pages,
        },
        "boot-source": {
            "kernel_image_path": args.kernel,
            "boot_args": (
                "console=ttyS0 quiet loglevel=1 reboot=k panic=1 init=/init "
                "root=/dev/vda rw transparent_hugepage=never " + args.boot_extra
            ),
        },
        "drives": [{
            "drive_id": "rootfs",
            "path_on_host": str(root),
            "is_root_device": True,
            "is_read_only": False,
        }],
    }))
    proc = subprocess.Popen(
        [args.binary, "--api-sock", str(work / "api.sock"), "--no-seccomp",
         "--config-file", str(config)],
        stdin=subprocess.PIPE, stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT, bufsize=0,
    )
    try:
        emit({"event": "start", "pid": proc.pid, "memory": args.memory,
              "vcpus": args.vcpus, "command": args.command,
              "boot_extra": args.boot_extra, "huge_pages": args.huge_pages})
        began = time.monotonic()
        pending = b""
        sent = None
        while time.monotonic() - began < args.timeout:
            readable, _, _ = select.select([proc.stdout], [], [], 1)
            if not readable:
                if proc.poll() is not None:
                    raise RuntimeError(f"VMM exited {proc.returncode}")
                continue
            data = os.read(proc.stdout.fileno(), 65536)
            if not data:
                raise RuntimeError("VMM closed output")
            pending += data
            while b"\n" in pending:
                line, pending = pending.split(b"\n", 1)
                line = line.decode(errors="replace").strip()
                emit({"host_elapsed_s": round(time.monotonic() - began, 6), "line": line})
                if line.startswith("SPROUTFS_READY") and sent is None:
                    sent = time.monotonic()
                    proc.stdin.write(("run " + args.command + "\n").encode())
                    proc.stdin.flush()
                if line.startswith("SPROUTFS_RUN") and sent is not None:
                    if "exit=0 " not in line:
                        raise RuntimeError(line)
                    emit({"event": "complete", "host_command_s": time.monotonic() - sent})
                    return
        raise TimeoutError("probe exceeded deadline")
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait(timeout=10)
        proc.stdin.close()
        proc.stdout.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--command", required=True)
    parser.add_argument("--vcpus", type=int, default=1)
    parser.add_argument("--memory", type=int, default=2048)
    parser.add_argument("--boot-extra", default="")
    parser.add_argument("--huge-pages", choices=["None", "Transparent", "2M"], default="None")
    parser.add_argument("--timeout", type=int, default=600)
    parser.add_argument("--kernel", required=True)
    parser.add_argument("--binary", required=True)
    parser.add_argument("--root", required=True)
    args = parser.parse_args()
    with tempfile.TemporaryDirectory(prefix="sproutfs-plain-probe.") as directory:
        run(args, pathlib.Path(directory))


if __name__ == "__main__":
    main()
