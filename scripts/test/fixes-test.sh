#!/usr/bin/env bash
# What scripts/lib/demo-fixes.sh's rollout step does, run against the model in
# fake-sproutfsctl.sh rather than a cluster.
#
# The other three fixes need a pod to delete, a VMM to kill and a host log to
# read, and there is nothing here to model them with. The rollout is different:
# what it asserts is the deployment's own bookkeeping across a replacement of
# every host pod — that the guest's memory was migrated rather than lost, and
# that a VM can be created on the pods that came back — and the pods that come
# back carry names of their own, because the hosts are a Deployment. A flow that
# still named the StatefulSet, or that assumed a pod comes back as itself, would
# pass every unit test in the repository and fail on the node.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=scripts/test/lib.sh
. "$here/lib.sh"
shell=$(find_shell)
work=${SPROUTFS_TEST_WORK:-$(mktemp -d "${TMPDIR:-/tmp}/sproutfs-fixes-test.XXXXXX")}
# What a run left behind is kept when something did not hold, because the first
# thing anyone wants then is the output and the model's own log.
keep_or_clear() {
    if ((failures > 0)); then
        printf 'what these runs left is under %s\n' "$work" >&2
    else
        rm -rf -- "$work"
    fi
}
trap keep_or_clear EXIT

# fixes_run runs demo-fixes.sh over a model of its own and leaves everything
# under $run: out the run output, status its exit status, and model the
# deployment it ran against.
model=
status=
fixes_run() {
    local run=$1
    shift
    rm -rf -- "$run"
    mkdir -p "$run/bin" "$run/model"
    ln -s "$repo/scripts/test/fake-kubectl.sh" "$run/bin/kubectl"
    (
        PATH=$run/bin:$PATH
        export PATH
        export SPROUTFS_FAKE_STATE=$run/model
        export SPROUTFS_FAKE_CTL=$repo/scripts/test/fake-sproutfsctl.sh
        export KUBECONFIG=$run/kubeconfig
        "$shell" "$repo/scripts/lib/demo-fixes.sh" "$@" < /dev/null
    ) > "$run/out" 2>&1 && printf '0\n' > "$run/status" || printf '%s\n' "$?" > "$run/status"
    model=$run/model
    output=$run/out
    read -r status < "$run/status"
}

# --- the rollout step over a Deployment --------------------------------------

fixes_run "$work/rollout" rollout
want_status "$status" 0 'the rollout fix over a deployment of hosts'
want_file_has "$output" 'the guest still holds what it wrote after its last checkpoint' \
    'the migrated guest kept the witness it wrote after its last checkpoint'
want_file_has "$output" 'one live-cluster fix holds' 'a run that got to the end says so'

# The pods that came back are not the pods that went away. A Deployment's pod
# names carry a suffix of their own, so nothing a flow does may assume one comes
# back as itself — and nothing a template is named by may either.
for gone in sproutfs-host-0 sproutfs-host-1; do
    if grep -q "^$gone " "$model/hosts"; then
        bad "the rollout left $gone in the deployment: a Deployment replaces a pod with another name" \
            "$(cat "$model/hosts")"
    else
        ok
    fi
done
want_equal "$(wc -l < "$model/hosts" | tr -d ' ')" 2 'the deployment has two hosts again'

# And the VM is on one of them: it was migrated by the preStop drain rather than
# left on a pod that is gone.
before=$(awk '/restarting the host Deployment with/ { print $NF }' "$output")
after=$(awk '/ now runs on / { print $NF }' "$output")
want "$([[ -n $before && -n $after && $before != "$after" ]] && echo 1 || echo 0)" \
    "the VM moved off the pod that was replaced: it was on '$before' and is on '$after'"
want "$(grep -q "^$after " "$model/hosts" && echo 1 || echo 0)" \
    "the VM runs on a host the deployment still has: $after"

# A create on the pods that came back is what says they are ready to serve, and
# under this identity that costs no import at all: the template is named by its
# image's bytes, so the pods that came back found it already published.
want_file_has "$output" 'created .* from the template' \
    'a replacement host created a VM from the template already in the deployment'

# --- the rollout is of a Deployment, and the harness knows the difference -----
# The one thing this flow can get wrong that nothing else would catch is naming
# the workload it rolls. The model answers for deployment/sproutfs-host and for
# nothing else, so a flow that went back to the StatefulSet fails here.

run=$work/kind
mkdir -p "$run/model"
kubectl_says() {
    SPROUTFS_FAKE_STATE=$run/model SPROUTFS_FAKE_CTL=$repo/scripts/test/fake-sproutfsctl.sh \
        "$repo/scripts/test/fake-kubectl.sh" "$@" 2>&1
}
if kubectl_says rollout restart -n sproutfs statefulset/sproutfs-host > "$run/sts" 2>&1; then
    bad 'the model rolled a statefulset, which this deployment does not have' "$(cat "$run/sts")"
else
    ok
fi
if kubectl_says rollout restart -n sproutfs deployment/sproutfs-host > "$run/deploy" 2>&1; then
    ok
else
    bad 'the model would not roll the host Deployment' "$(cat "$run/deploy")"
fi

report fixes-test.sh
