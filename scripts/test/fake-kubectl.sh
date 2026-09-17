#!/usr/bin/env bash
# The kubectl the demo flows reach their deployment through, answering out of
# the model in scripts/test/fake-sproutfsctl.sh. It is here so that the flow
# under test runs its own `ctl` wrapper — `kubectl exec -n NS -i
# deploy/sproutfs-orchestrator -- sproutfsctl ...` — rather than a stand-in for
# it, which is where a wrong namespace or a missing `--` would hide.
set -euo pipefail

# workload is the one thing a rollout may name. The hosts are a Deployment: a
# template is named by its guest image's bytes, so nothing depends on a pod
# coming back as itself and there is nothing for a StatefulSet's stable names to
# be for. A flow that still asked for one would find no such workload on the
# node, and finds none here.
workload=deployment/sproutfs-host

case ${1:-} in
exec)
    shift
    while (($# > 0)) && [[ $1 != -- ]]; do shift; done
    (($# > 0)) || { echo "fake kubectl: an exec with no -- and no command" >&2; exit 2; }
    shift
    [[ ${1:-} == sproutfsctl ]] || { echo "fake kubectl: exec ran ${1:-nothing}, not sproutfsctl" >&2; exit 2; }
    shift
    # -i forwards this process's standard input to the far side, and kubectl
    # reads it whether the command on the far side wants it or not. That is what
    # makes a `ctl` called from the body of a loop reading a list on standard
    # input eat the rest of that list, and the loop end after one line. It is
    # done here every time rather than when the race happens to go that way: a
    # flow is either taking its standard input from somewhere it chose or it is
    # relying on a race, and the second is not a thing to ship.
    cat > /dev/null
    exec "${SPROUTFS_FAKE_CTL:?"SPROUTFS_FAKE_CTL names the model CLI"}" "$@" < /dev/null
    ;;
rollout)
    shift
    action=${1:-}
    shift || true
    # The workload is the argument of the form kind/name, which is how every
    # rollout the flows do names one.
    named=''
    for argument in "$@"; do
        case $argument in */*) named=$argument ;; esac
    done
    [[ $named == "$workload" ]] || {
        echo "fake kubectl: there is no ${named:-nameless workload} in this deployment, only $workload" >&2
        exit 1
    }
    case $action in
    restart)
        # The model's rollout is one step: every pod is drained and replaced
        # before this returns, so the status below has nothing left to wait for.
        exec "${SPROUTFS_FAKE_CTL:?"SPROUTFS_FAKE_CTL names the model CLI"}" replace-hosts < /dev/null
        ;;
    status)
        printf 'deployment "sproutfs-host" successfully rolled out\n'
        ;;
    *)
        echo "fake kubectl: nothing here answers a rollout ${action:-with no action}" >&2
        exit 2
        ;;
    esac
    ;;
logs)
    # The hosts' logs over the run, which this deployment has none of.
    ;;
*)
    echo "fake kubectl: nothing here answers ${1:-no subcommand}" >&2
    exit 2
    ;;
esac
