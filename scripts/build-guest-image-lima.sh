#!/usr/bin/env bash
# Build the workload guest image for the storage benchmark inside a Lima
# instance and cache it there. The build itself is scripts/lib/bench-image.sh,
# which a GCE host runs too; the instance mounts this repository at its own
# path, so the script is run from where it is.
#
# Prints the guest-side path of the image on stdout; progress goes to stderr.
set -euo pipefail
repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
instance=${SPROUTFS_LIMA_INSTANCE:-default}
if (($# != 0)); then echo "Usage: $0" >&2; exit 2; fi
key=$(cat "$repo/scripts/lib/bench-image.sh" "$repo/internal/vmmachine/testdata/guest.c" | shasum -a 256 | cut -c1-32)
limactl shell "$instance" sudo -n true
limactl shell "$instance" bash "$repo/scripts/lib/bench-image.sh" "$repo" "$key"
