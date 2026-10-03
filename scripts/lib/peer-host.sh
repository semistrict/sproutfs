#!/usr/bin/env bash
# One GCE host's half of scripts/bench-peer-gce.sh. It runs in the home
# directory the script copied the binaries to.
#
#   peer-host.sh serve after|before    start a server in the background
#   peer-host.sh stop                  stop it
#   peer-host.sh read after|before CASE SERVER DURATION FAULT_EVERY
#                                      run one case and append its record
#   peer-host.sh cpu                   print the server's CPU time in clock ticks
#   peer-host.sh linux-tests           run the Linux-only transport tests
set -euo pipefail
cd "$HOME"
touch /var/lib/sproutfs-bench/lease 2>/dev/null || sudo touch /var/lib/sproutfs-bench/lease
mkdir -p results

case "$1" in
    serve)
        : > "results/server-$2.log"
        if [[ $2 == after ]]; then
            nohup ./sproutfs-peerbench server -listen :7500 -dir "$HOME" > results/server-after.log 2>&1 &
        else
            PEERBENCH_MODE=server PEERBENCH_LISTEN=:7500 nohup ./peerbench-before -test.run '^TestPeerBench$' \
                -test.v -test.timeout 0 > results/server-before.log 2>&1 &
        fi
        echo $! > server.pid
        for _ in {1..120}; do
            if [[ $(< "results/server-$2.log") == *serving* ]]; then
                exit 0
            fi
            sleep 0.5
        done
        echo "the $2 server did not start" >&2
        cat "results/server-$2.log" >&2
        exit 1
        ;;
    stop)
        if [[ -e server.pid ]]; then
            kill "$(< server.pid)" 2> /dev/null || true
            while kill -0 "$(< server.pid)" 2> /dev/null; do sleep 0.2; done
            rm -f server.pid
        fi
        ;;
    read)
        build=$2 name=$3 server=$4 duration=$5 every=$6
        if [[ $build == after ]]; then
            ./sproutfs-peerbench client -server "$server" -case "$name" -duration "$duration" \
                -fault-every "$every" -out "results/$build.jsonl"
        else
            PEERBENCH_MODE=client PEERBENCH_SERVER=$server PEERBENCH_CASE=$name PEERBENCH_DURATION=$duration \
                PEERBENCH_FAULT_EVERY=$every PEERBENCH_OUT=results/$build.jsonl \
                ./peerbench-before -test.run '^TestPeerBench$' -test.timeout 0
        fi
        ;;
    cpu)
        read -ra stat < "/proc/$(< server.pid)/stat"
        echo $((stat[13] + stat[14]))
        ;;
    linux-tests)
        ./framer.test -test.v -test.count=1 > results/framer-tests.log 2>&1 || { cat results/framer-tests.log; exit 1; }
        ./real.test -test.v -test.count=1 > results/real-tests.log 2>&1 || { cat results/real-tests.log; exit 1; }
        ./peer.test -test.count=1 > results/peer-tests.log 2>&1 || { cat results/peer-tests.log; exit 1; }
        ;;
    *)
        echo "usage: $0 serve|stop|read|linux-tests" >&2
        exit 2
        ;;
esac
