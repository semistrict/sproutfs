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
# report violated. That is how a check that has stopped catching anything is
# caught itself.
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
mode=${1:-quick}
log=$(mktemp)
trap 'rm -f "$log"' EXIT
failed=0

for directory in "$root"/spec/*/; do
    modules=("$directory"*.tla)
    module=${modules[0]}
    if [ "$mode" = deep ]; then
        configs=("$directory"deep/*.cfg)
    else
        configs=("$directory"MC*.cfg)
    fi
    for config in "${configs[@]}"; do
        name=${config#"$root"/}
        if "$root/scripts/tlc.sh" "$module" "$config" >"$log" 2>&1; then
            echo "ok    $name: $(grep -o '[0-9]* distinct states found, 0' "$log" | cut -d' ' -f1) states"
        else
            echo "FAIL  $name"
            cat "$log"
            failed=1
        fi
    done
    if [ "$mode" = deep ]; then
        continue
    fi
    for config in "$directory"mutants/*.cfg; do
        name=${config#"$root"/}
        expect=$(sed -n 's/^\\\* expect: //p' "$config")
        if "$root/scripts/tlc.sh" "$module" "$config" >"$log" 2>&1; then
            echo "FAIL  $name: the defect went unnoticed, want $expect violated"
            failed=1
        elif grep -q "$expect is violated" "$log"; then
            echo "ok    $name: caught by $expect"
        else
            echo "FAIL  $name: want $expect violated"
            cat "$log"
            failed=1
        fi
    done
done
exit "$failed"
