#!/usr/bin/env bash
# Run on a benchmark host: what one disk can do, read and written raw, as a
# shard's disk is. The disk is named as the node's -device names it: a
# Compute Engine disk by its name, its block device
# /dev/disk/by-id/google-<name>, or an EBS volume by its ID, vol-<hex>, its
# block device /dev/disk/by-id/nvme-Amazon_Elastic_Block_Store_vol<hex>. Every
# job uses O_DIRECT, so the page cache plays no part, and each writes its fio
# JSON to the output directory.
#
# The first job writes the span the others use, in order, so no read is of a
# block the disk has never written: a network disk answers those without
# reading anything.
#
# Usage: shard-fio.sh <disk name> <output directory> [span, 8G by default]
set -euo pipefail
name=${1:?the disk name}
out=${2:?the output directory}
span=${3:-8G}
[[ $name =~ ^[a-z0-9-]+$ ]] || { echo "A disk name is lower-case letters, digits and dashes." >&2; exit 2; }
[[ $span =~ ^[0-9]+[GM]$ ]] || { echo "A span is a size such as 8G." >&2; exit 2; }
if [[ $name =~ ^vol-([0-9a-f]+)$ ]]; then
    device=/dev/disk/by-id/nvme-Amazon_Elastic_Block_Store_vol${BASH_REMATCH[1]}
else
    device=/dev/disk/by-id/google-$name
fi
[[ -b $device ]] || { echo "$device is not a block device." >&2; exit 1; }
if ! command -v fio > /dev/null; then
    if command -v dnf > /dev/null; then
        sudo dnf install -y -q fio
    else
        sudo DEBIAN_FRONTEND=noninteractive apt-get -qq update
        sudo DEBIAN_FRONTEND=noninteractive apt-get -qq install -y fio > /dev/null
    fi
fi
mkdir -p "$out"
fio --version > "$out/fio-version.txt"

# job name, then fio's own options.
job() {
    local job=$1
    shift
    sudo fio --name="$job" --filename="$device" --direct=1 --ioengine=libaio --size="$span" \
        --output-format=json --output="$out/$job.json" "$@"
}

job write-seq-1m-qd8 --rw=write --bs=1M --iodepth=8
job read-seq-1m-qd8 --rw=read --bs=1M --iodepth=8 --time_based --runtime=20 --ramp_time=2
for depth in 1 4 16; do
    job "read-rand-512k-qd$depth" --rw=randread --bs=512k --iodepth="$depth" --time_based --runtime=20 --ramp_time=2
done
for depth in 1 16 64; do
    job "read-rand-4k-qd$depth" --rw=randread --bs=4k --iodepth="$depth" --time_based --runtime=20 --ramp_time=2
done
job write-rand-4k-qd16 --rw=randwrite --bs=4k --iodepth=16 --time_based --runtime=20 --ramp_time=2
job write-rand-512k-qd4 --rw=randwrite --bs=512k --iodepth=4 --time_based --runtime=20 --ramp_time=2
