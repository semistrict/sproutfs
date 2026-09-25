#!/usr/bin/env bash
# Runs one Go test binary as root in the Lima instance, which is how the
# vmmachine guards are exercised from a Mac:
#
#   GOOS=linux GOARCH=arm64 CGO_ENABLED=0 python3 scripts/mutate-simulation.py \
#     --go-test-exec scripts/mutation/lima-go-test-exec.sh \
#     --output "$HOME/.cache/vmmachine-mutations" --mutant vmmachine-skip-owner
#
# The binary must be under a directory the instance mounts, such as $HOME. The
# mutation runner clears every other SPROUTFS_* setting, so only the guard it
# enables is passed on.
set -euo pipefail
exec limactl shell default sudo -n env SPROUTFS_SIM_BUG="${SPROUTFS_SIM_BUG-}" "$@"
