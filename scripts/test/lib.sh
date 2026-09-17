#!/usr/bin/env bash
# What the shell tests under scripts/test share: finding a bash the demo flows
# can run under, and saying what an assertion wanted.
#
# It is sourced, never run.

# repo is the checkout these tests are part of, and output is where the run a
# test is asserting about left what it printed. Both are read by the files that
# source this one.
# shellcheck disable=SC2034 # read by the test files, not here
repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
output=
failures=0
checks=0

# find_shell prints a bash the demo flows can run under. demo-soak.sh keeps its
# per-VM expectation in an associative array, which is bash 4 and up, and macOS
# ships 3.2; SPROUTFS_TEST_BASH names one, and otherwise the usual places are
# looked in. Nothing here is skipped when none is found: a test that quietly
# does not run is a test that does not exist.
find_shell() {
    local candidate major
    for candidate in "${SPROUTFS_TEST_BASH:-}" /opt/homebrew/bin/bash /usr/local/bin/bash \
        /bin/bash /usr/bin/bash; do
        [[ -n $candidate && -x $candidate ]] || continue
        # shellcheck disable=SC2016 # BASH_VERSINFO is the candidate's, not ours
        major=$("$candidate" -c 'echo ${BASH_VERSINFO[0]}' 2> /dev/null) || continue
        [[ $major =~ ^[0-9]+$ ]] || continue
        ((major >= 4)) || continue
        printf '%s\n' "$candidate"
        return 0
    done
    echo "No bash 4 or newer here, which the demo flows need." >&2
    echo "Install one (brew install bash) or name it in SPROUTFS_TEST_BASH." >&2
    return 1
}

# --- saying what a test wanted ------------------------------------------------

ok() { checks=$((checks + 1)); }

bad() {
    checks=$((checks + 1))
    failures=$((failures + 1))
    printf 'FAIL: %s\n' "$1" >&2
    shift
    local line
    for line in "$@"; do printf '      %s\n' "$line" >&2; done
}

# want reports one condition that has already been worked out, which is what a
# test with arithmetic in it wants: `want "$((a > b))" ...` reads worse than
# saying the comparison here.
want() {
    if (($1)); then ok; else
        shift
        bad "$@"
    fi
}

want_status() {
    local got=$1 wanted=$2 what=$3
    if [[ $got == "$wanted" ]]; then ok; else
        bad "$what: exited $got, want $wanted" "$(tail -n 20 "$output")"
    fi
}

want_file_has() {
    local path=$1 pattern=$2 what=$3
    if [[ ! -f $path ]]; then
        bad "$what: there is no $path"
    elif grep -qE -- "$pattern" "$path"; then ok; else
        bad "$what: $(basename "$path") has no line matching $pattern" "$(head -n 40 "$path")"
    fi
}

want_file_lacks() {
    local path=$1 pattern=$2 what=$3
    if [[ ! -f $path ]]; then
        bad "$what: there is no $path"
    elif grep -qE -- "$pattern" "$path"; then
        bad "$what: $(basename "$path") has a line matching $pattern" "$(grep -E -- "$pattern" "$path")"
    else ok; fi
}

want_equal() {
    local got=$1 wanted=$2 what=$3
    if [[ $got == "$wanted" ]]; then ok; else
        bad "$what: it is '$got', want '$wanted'"
    fi
}

# report prints what a test file found and reports it as its exit status.
report() {
    if ((failures > 0)); then
        printf '%s: %s of %s assertions failed\n' "$1" "$failures" "$checks" >&2
        return 1
    fi
    printf '%s: %s assertions held\n' "$1" "$checks"
}
