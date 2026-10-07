#!/usr/bin/env bash
# Run on a benchmark host: what one journal-sized network disk takes to write
# and sync, one write in flight, as the host's journal writes a batch
# (scripts/bench-fsync-journal-gce.sh). The disk is named as a journal disk
# is attached: a Compute Engine disk by its name, its block device
# /dev/disk/by-id/google-<name>.
#
# Two jobs for every write size from 4 KiB to 64 KiB, each for fifteen
# seconds after two of warming up, over blocks already written once:
#
#   sync-<size>    a buffered write and an fsync of the device, the journal's
#                  own way (platform.File.Sync on the device it opened). fio
#                  times the fsync apart from the write.
#   dsync-<size>   a write with O_DIRECT and O_DSYNC, which the kernel sends
#                  as one write that the device makes durable before it
#                  answers: the device's own cost of a durable write.
#
# Each writes its fio JSON to the output directory, with the device's queue
# settings beside them.
#
# Usage: fsync-fio.sh <disk name> <output directory>
set -euo pipefail
name=${1:?the disk name}
out=${2:?the output directory}
seconds=${FSYNC_FIO_SECONDS:-15}
[[ $name =~ ^[a-z0-9-]+$ ]] || { echo "A disk name is lower-case letters, digits and dashes." >&2; exit 2; }
[[ $seconds =~ ^[0-9]+$ ]] || { echo "FSYNC_FIO_SECONDS is a number." >&2; exit 2; }
device=/dev/disk/by-id/google-$name
[[ -b $device ]] || { echo "$device is not a block device." >&2; exit 1; }
if ! command -v fio > /dev/null; then
    sudo DEBIAN_FRONTEND=noninteractive apt-get -qq update
    sudo DEBIAN_FRONTEND=noninteractive apt-get -qq install -y fio > /dev/null
fi
mkdir -p "$out"
fio --version > "$out/fio-version.txt"
block=$(basename "$(readlink -f "$device")")
{
    echo "device $(readlink -f "$device")"
    for setting in write_cache fua max_sectors_kb nr_requests scheduler rotational; do
        echo "$setting $(cat "/sys/block/$block/queue/$setting" 2> /dev/null || echo none)"
    done
    lsblk -o NAME,SIZE,MODEL "/dev/$block"
} > "$out/device.txt"

# job name, then fio's own options. The journal writes forwards through its
# ring, so every job writes forwards through the disk's first 8 GiB.
job() {
    local job=$1
    shift
    sudo fio --name="$job" --filename="$device" --rw=write --iodepth=1 --size=8G \
        --time_based --runtime="$seconds" --ramp_time=2 --percentile_list=50:90:99:99.9:99.99 \
        --output-format=json --output="$out/$job.json" "$@"
}

# The first write of a block a disk has never written can cost more than a
# rewrite. A journal disk writes its whole ring once and then rewrites it, so
# one job measures 4 KiB on blocks never written, past the span the others
# use, and the span is written whole before the others run.
job fresh-sync-4k --offset=16G --bs=4k --ioengine=sync --direct=0 --fsync=1
sudo fio --name=prefill --filename="$device" --rw=write --bs=1M --iodepth=8 --ioengine=libaio --direct=1 \
    --size=8G --output-format=json --output="$out/prefill.json"
for size in 4k 8k 16k 32k 64k; do
    job "sync-$size" --bs="$size" --ioengine=sync --direct=0 --fsync=1
    job "dsync-$size" --bs="$size" --ioengine=psync --direct=1 --sync=dsync
done
