#!/usr/bin/env bash
# What scripts/bench-stripes-gce.sh runs on each host, beside the benchmark's
# binary in the home directory. Everything it writes goes to ~/results.
#
#   stripes-host.sh serve INDEX SERVERS OBJECTS
#       put the store on the host's local SSD and serve this host's stripes
#       in the background.
#   stripes-host.sh read NAME SERVER-LIST CLIENT-FLAGS...
#       run one pass of the client, writing NAME.json, NAME.txt and NAME.log.
set -euo pipefail
cd "$HOME"
mkdir -p results
bench=$HOME/sproutfs-stripebench

case ${1:-} in
    serve)
        [[ $# == 4 ]] || { echo "usage: $0 serve INDEX SERVERS OBJECTS" >&2; exit 2; }
        # The host's one local NVMe SSD, as a deployment's cache disk is.
        if ! mountpoint -q /mnt/stripes; then
            sudo mkfs.ext4 -q -F /dev/disk/by-id/google-local-nvme-ssd-0
            sudo mkdir -p /mnt/stripes
            sudo mount /dev/disk/by-id/google-local-nvme-ssd-0 /mnt/stripes
            sudo chown "$(id -u):$(id -g)" /mnt/stripes
        fi
        if pgrep -f "$bench server" > /dev/null; then
            echo "a server is already running here" >&2
            exit 1
        fi
        setsid nohup "$bench" server -index "$2" -servers "$3" -objects "$4" \
            -file /mnt/stripes/store > results/server.log 2>&1 < /dev/null &
        ;;
    read)
        [[ $# -ge 3 ]] || { echo "usage: $0 read NAME SERVER-LIST CLIENT-FLAGS..." >&2; exit 2; }
        name=$2 servers=$3
        shift 3
        timeout --signal=TERM --kill-after=30s 40m \
            "$bench" client -servers "$servers" -name "$(hostname)" -out "results/$name" "$@" \
            > /dev/null 2> "results/$name.log"
        ;;
    *)
        echo "usage: $0 serve|read ..." >&2
        exit 2
        ;;
esac
