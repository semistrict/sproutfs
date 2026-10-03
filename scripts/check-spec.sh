#!/usr/bin/env bash
# Model-checks every TLA+ spec under spec/.
#
#   scripts/check-spec.sh        the MC*.cfg configurations, and the mutants
#   scripts/check-spec.sh deep   the configurations under deep/, which take
#                                minutes each
#
# Each spec directory holds one module. Every configuration must pass. A
# mutant puts a defect back into the module through its Bugs constant and must
# fail: its "\* expect: <name>" line names the invariant or property TLC has to
# report violated, or "deadlock". That is how a check that has stopped catching
# anything is caught itself. Each line reports how long TLC took.
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
mode=${1:-quick}
log=$(mktemp)
trap 'rm -f "$log"' EXIT
failed=0

# caught <config> <expect>: whether the log shows the failure the mutant wants.
# TLC names a violated invariant or safety property, but not a violated
# liveness property, so a mutant that expects one names it as its only
# PROPERTY.
caught() {
    if [ "$2" = deadlock ]; then
        grep -q "Deadlock reached" "$log"
    elif grep -q "$2 is violated" "$log"; then
        true
    else
        grep -q "Temporal properties were violated" "$log" &&
            [ "$(sed -n 's/^PROPERTY //p' "$1")" = "$2" ]
    fi
}

# tlc <module> <config>: runs TLC into the log, and sets took to its seconds.
tlc() {
    local start=$SECONDS status=0
    "$root/scripts/tlc.sh" "$1" "$2" >"$log" 2>&1 || status=$?
    took=$((SECONDS - start))s
    return "$status"
}

for directory in "$root"/spec/*/; do
    modules=("$directory"*.tla)
    module=${modules[0]}
    if [ "$mode" = deep ]; then
        configs=("$directory"deep/*.cfg)
    else
        configs=("$directory"MC*.cfg)
    fi
    for config in "${configs[@]}"; do
        [ -e "$config" ] || continue
        name=${config#"$root"/}
        if tlc "$module" "$config"; then
            states=$(sed -n 's/^[0-9]* states generated, \([0-9]*\) distinct states found, 0 states left on queue\.$/\1/p' "$log" | tail -1)
            echo "ok    $name: $states states, $took"
        else
            echo "FAIL  $name, $took"
            cat "$log"
            failed=1
        fi
    done
    if [ "$mode" = deep ]; then
        continue
    fi
    for config in "$directory"mutants/*.cfg; do
        [ -e "$config" ] || continue
        name=${config#"$root"/}
        expect=$(sed -n 's/^\\\* expect: //p' "$config")
        if tlc "$module" "$config"; then
            echo "FAIL  $name: the defect went unnoticed, want $expect, $took"
            failed=1
        elif caught "$config" "$expect"; then
            echo "ok    $name: caught by $expect, $took"
        else
            echo "FAIL  $name: want $expect, $took"
            cat "$log"
            failed=1
        fi
    done
done
exit "$failed"
