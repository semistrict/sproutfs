#!/usr/bin/env bash
# The features merged since the soak, each exercised once on the demo node and
# each result checked.
#
# scripts/demo-gce.sh copies this one file to the node and runs it there:
#
#     bash demo-features.sh
#
# It drives the deployment through sproutfsctl, as demo-run.sh does, and reaches
# one host's own API through a port-forward from the node for what only a host
# serves: a tenant's templates and VMs, and GET /stored. What it checks:
#
#   1. Kept checkpoints. capture --keep keeps a checkpoint with the guest's
#      memory, kept lists it, and create --from VM@CHECKPOINT resumes a new VM
#      at it after the VM has moved on. A release of that checkpoint is refused
#      once a VM was created from it, and a release of one no VM came from
#      deletes it.
#   2. Creates from checkpoints. stop --suspend --keep stops a VM with its
#      memory and keeps the checkpoint; create --from VM resumes a new VM at the
#      checkpoint the stopped VM's record selects, and create --from VM@CHECKPOINT
#      at the kept one; start resumes the stopped VM itself.
#   3. capture --new captures a running VM into a new, stopped VM, and start
#      resumes that VM where the capture paused the first.
#   4. Ephemeral disks. create --ephemeral gives the guest a second PMEM device
#      no checkpoint holds: a migration carries its bytes, and a fork's child
#      finds it zeroed.
#   5. Pulls. create --pull copies the VM's whole memory onto its host's disk,
#      and the host reports the pull complete.
#   6. Templates imported at runtime. import-template takes a guest image no
#      host was configured with, names the template by the image's sha256, and
#      create --template boots a VM from it.
#   7. Tenants and billing. A host imports a template for a tenant and creates a
#      VM of that tenant from it; GET /stored?tenant= reports that VM's bytes
#      and GET /stored does not; another tenant can create neither from the
#      template nor from the VM.
#
# Overridable: SPROUTFS_DEMO_NAMESPACE.
set -euo pipefail

export KUBECONFIG=${KUBECONFIG:-/etc/rancher/k3s/k3s.yaml}
namespace=${SPROUTFS_DEMO_NAMESPACE:-sproutfs}
agent_timeout=90
exec_timeout=60s
guest_image=/opt/sproutfs-demo/guest/guest.ext4

ctl() { kubectl exec -n "$namespace" -i deploy/sproutfs-orchestrator -- sproutfsctl "$@" < /dev/null; }
step() { printf '\n=== %s ===\n' "$*"; }
fail() { printf '\nFAIL: %s\n' "$*" >&2; exit 1; }
run_in() { ctl exec "$1" --timeout "$exec_timeout" -- "${@:2}" < /dev/null; }

agent_ready() {
    local vm=$1 deadline=$(($(date +%s) + agent_timeout)) last=
    while (($(date +%s) < deadline)); do
        if last=$(run_in "$vm" true 2>&1); then return 0; fi
    done
    printf '%s\n' "$last" >&2
    return 1
}

host_of() { ctl list | awk -v vm="$1" '$1 == vm && $2 != "-" { print $2 }'; }
other_host() { ctl hosts | awk -v host="$1" 'NR > 1 && $1 != host && $2 == "true" { print $1; exit }'; }
first_host() { ctl hosts | awk 'NR > 1 && $2 == "true" { print $1; exit }'; }
# vm_of is the VM a create or a capture --new printed first.
vm_of() { awk '{ print $1; exit }' <<< "$1"; }
# checkpoint_in is the number after the word "checkpoint" in what a command
# printed.
checkpoint_in() { sed -n 's/.*checkpoint \([0-9][0-9]*\).*/\1/p' <<< "$1" | head -1; }

# --- a host's own API, through a port-forward from the node -----------------
work=$(mktemp -d /tmp/sproutfs-features.XXXXXX)
chmod 0700 "$work"
forwarding=''
cleanup() {
    if [[ -n $forwarding ]]; then kill "$forwarding" 2> /dev/null || true; fi
    rm -rf -- "$work"
}
trap cleanup EXIT
# The token goes into a header file only this user can read, rather than onto a
# command line every process on the node can list.
kubectl get secret -n "$namespace" sproutfs-api-token -o jsonpath='{.data.token}' |
    base64 -d | awk '{ printf "Authorization: Bearer %s\n", $0 }' > "$work/header"
api_port=18080

forward_to() {
    local pod=$1 deadline=$(($(date +%s) + 30))
    if [[ -n $forwarding ]]; then kill "$forwarding" 2> /dev/null || true; fi
    kubectl port-forward -n "$namespace" "pod/$pod" "$api_port:8080" > "$work/forward.log" 2>&1 &
    forwarding=$!
    until curl -fsS "http://127.0.0.1:$api_port/healthz" > /dev/null 2>&1; do
        (($(date +%s) < deadline)) || { cat "$work/forward.log" >&2; fail "no port-forward to $pod"; }
        sleep 0.5
    done
}

# host_api calls the forwarded host and prints the reply. It prints the status
# code on a line of its own after the body, so that a refusal is something a
# check can name rather than a curl failure.
host_api() {
    local method=$1 path=$2
    shift 2
    curl -sS -X "$method" -H @"$work/header" -w '\n%{http_code}\n' "$@" \
        "http://127.0.0.1:$api_port$path"
}
status_of() { tail -1 <<< "$1"; }
body_of() { sed '$d' <<< "$1"; }
json() { python3 -c "import json, sys; d = json.load(sys.stdin); print($1)"; }

printf 'sproutfs demo: the merged features against %s\n' "$namespace"
ctl hosts

# --- 1. kept checkpoints -------------------------------------------------------
step 'a kept checkpoint outlives the VM moving on, and a create resumes at it'
created=$(ctl create --template alpine)
printf '%s\n' "$created"
kept_vm=$(vm_of "$created")
[[ $kept_vm == vm-* ]] || fail "create did not name a VM: $created"
agent_ready "$kept_vm" || fail "the agent in $kept_vm never answered"
run_in "$kept_vm" 'echo at-the-kept-checkpoint > /run/marker' > /dev/null
captured=$(ctl capture "$kept_vm" --keep)
printf '%s\n' "$captured"
kept=$(checkpoint_in "$captured")
[[ -n $kept && $captured == *kept* ]] || fail "capture --keep did not say it kept a checkpoint: $captured"
run_in "$kept_vm" 'echo moved-on > /run/marker' > /dev/null
ctl capture "$kept_vm" > /dev/null
listed=$(ctl kept "$kept_vm")
printf '%s\n' "$listed"
awk -v c="$kept" '$1 "" == c && $3 == "true" && $4 == "false" { found = 1 } END { exit !found }' <<< "$listed" ||
    fail "kept did not list $kept with its state and no fork: $listed"

created=$(ctl create --from "$kept_vm@$kept")
printf '%s\n' "$created"
[[ $created == *' resumed on '* ]] || fail "a create from a kept checkpoint with state did not resume: $created"
from_kept=$(vm_of "$created")
agent_ready "$from_kept" || fail "the agent in $from_kept never answered"
seen=$(run_in "$from_kept" 'cat /run/marker' | tr -d '\r')
[[ $seen == at-the-kept-checkpoint ]] || fail "$from_kept resumed holding '$seen', not the kept checkpoint's marker"
printf '%s resumed at the kept checkpoint, not at where %s moved on to\n' "$from_kept" "$kept_vm"
listed=$(ctl kept "$kept_vm")
awk -v c="$kept" '$1 "" == c && $4 == "true" { found = 1 } END { exit !found }' <<< "$listed" ||
    fail "kept does not say a VM was created from $kept: $listed"
if refused=$(ctl release "$kept_vm@$kept" 2>&1); then
    fail "released $kept, which $from_kept was created from: $refused"
fi
printf 'a release of the checkpoint a VM came from is refused:\n%s\n' "$refused"

captured=$(ctl capture "$kept_vm" --keep)
spare=$(checkpoint_in "$captured")
ctl release "$kept_vm@$spare"
listed=$(ctl kept "$kept_vm")
awk -v c="$spare" '$1 "" == c { found = 1 } END { exit found }' <<< "$listed" ||
    fail "kept still lists $spare after its release: $listed"
printf 'a release of a checkpoint no VM came from gives it up\n'

# --- 2. creates from a stopped VM's checkpoints ---------------------------------
step 'stop --suspend --keep, then create --from the stopped VM and start it'
stopped=$(ctl stop "$kept_vm" --suspend --keep)
printf '%s\n' "$stopped"
at_stop=$(checkpoint_in "$stopped")
[[ -n $at_stop && $stopped == *kept* ]] || fail "stop --keep did not say it kept a checkpoint: $stopped"
[[ -z $(host_of "$kept_vm") ]] || fail "a host still runs $kept_vm after its stop"
for source in "$kept_vm" "$kept_vm@$at_stop"; do
    created=$(ctl create --from "$source")
    printf '%s\n' "$created"
    [[ $created == *' resumed on '* ]] || fail "a create from $source did not resume: $created"
    child=$(vm_of "$created")
    agent_ready "$child" || fail "the agent in $child never answered"
    seen=$(run_in "$child" 'cat /run/marker' | tr -d '\r')
    [[ $seen == moved-on ]] || fail "$child, created from $source, holds '$seen'"
    printf '%s resumed from %s where the stop paused it\n' "$child" "$source"
    ctl delete "$child" > /dev/null
done
started=$(ctl start "$kept_vm")
printf '%s\n' "$started"
[[ $started == *' started on '* ]] || fail "start of a suspended VM did not resume it: $started"
agent_ready "$kept_vm" || fail "the agent in $kept_vm never answered after its start"
seen=$(run_in "$kept_vm" 'cat /run/marker' | tr -d '\r')
[[ $seen == moved-on ]] || fail "$kept_vm came back holding '$seen'"
printf '%s came back where it stopped\n' "$kept_vm"

# --- 3. capture --new -------------------------------------------------------------
step 'capture --new captures a running VM into a new, stopped VM'
run_in "$kept_vm" 'echo at-the-capture > /run/marker' > /dev/null
captured=$(ctl capture "$kept_vm" --new)
printf '%s\n' "$captured"
copy=$(sed -n 's/.* captured into \([^ ]*\) at .*/\1/p' <<< "$captured")
[[ $copy == vm-* ]] || fail "capture --new named no new VM: $captured"
[[ -n $(host_of "$kept_vm") ]] || fail "$kept_vm stopped running when it was captured"
[[ -z $(host_of "$copy") ]] || fail "$copy runs, and a VM captured into is stopped"
run_in "$kept_vm" 'echo after-the-capture > /run/marker' > /dev/null
started=$(ctl start "$copy")
printf '%s\n' "$started"
agent_ready "$copy" || fail "the agent in $copy never answered"
seen=$(run_in "$copy" 'cat /run/marker' | tr -d '\r')
[[ $seen == at-the-capture ]] || fail "$copy started holding '$seen', not what the capture paused"
printf '%s resumed where the capture paused %s\n' "$copy" "$kept_vm"
ctl delete "$copy" > /dev/null
ctl delete "$from_kept" > /dev/null

# --- 4. ephemeral disks -------------------------------------------------------------
step 'an ephemeral disk is carried by a migration and zeroed in a fork'
created=$(ctl create --template alpine --ephemeral 256M)
printf '%s\n' "$created"
scratch=$(vm_of "$created")
[[ $scratch == vm-* ]] || fail "create --ephemeral did not name a VM: $created"
agent_ready "$scratch" || fail "the agent in $scratch never answered"
devices=$(run_in "$scratch" 'ls /dev/pmem*' | tr -d '\r' | tr '\n' ' ')
printf 'the guest sees: %s\n' "$devices"
[[ $devices == *pmem1* ]] || fail "$scratch has no second PMEM device: $devices"
read_ephemeral='dd if=/dev/pmem1 bs=1M count=64 2>/dev/null | md5sum'
written=$(run_in "$scratch" "dd if=/dev/urandom of=/dev/pmem1 bs=1M count=64 2>/dev/null && sync &&
    $read_ephemeral" | awk '{ print $1 }')
zeros=$(dd if=/dev/zero bs=1M count=64 2> /dev/null | md5sum | awk '{ print $1 }')
[[ -n $written && $written != "$zeros" ]] || fail "writing the ephemeral disk of $scratch failed"
from=$(host_of "$scratch")
to=$(other_host "$from")
ctl migrate "$scratch" --to "$to"
seen=$(run_in "$scratch" "$read_ephemeral" | awk '{ print $1 }')
[[ $seen == "$written" ]] || fail "the migration did not carry the ephemeral disk of $scratch"
printf 'the migration carried the ephemeral disk\n'
child=$(ctl fork "$scratch" | awk 'NR == 2 { print $1 }')
[[ $child == vm-* ]] || fail "forking $scratch named no child"
agent_ready "$child" || fail "the agent in $child never answered"
seen=$(run_in "$child" "$read_ephemeral" | awk '{ print $1 }')
[[ $seen == "$zeros" ]] || fail "the fork $child inherited its parent's ephemeral disk"
printf 'the fork found its ephemeral disk zeroed\n'
ctl delete "$child" > /dev/null
ctl delete "$scratch" > /dev/null

# --- 5. pulls ------------------------------------------------------------------------
step 'create --pull copies the whole memory onto the host disk'
created=$(ctl create --template alpine --pull)
printf '%s\n' "$created"
pulled=$(vm_of "$created")
agent_ready "$pulled" || fail "the agent in $pulled never answered"
where=$(host_of "$pulled")
forward_to "$where"
deadline=$(($(date +%s) + 300))
while :; do
    reply=$(host_api GET /status)
    [[ $(status_of "$reply") == 200 ]] || fail "GET /status on $where answered $(status_of "$reply")"
    pull=$(body_of "$reply" | json "next((v.get('pull') for v in d['vms'] if v['id'] == '$pulled'), None)")
    [[ $pull == *"'done': True"* ]] && break
    (($(date +%s) < deadline)) || fail "the pull of $pulled never finished: $pull"
    sleep 2
done
printf 'the pull of %s: %s\n' "$pulled" "$pull"
[[ $pull != *"'error'"* ]] || fail "the pull of $pulled stopped with an error: $pull"
body_of "$reply" | json "[v['pull']['pulled'] == v['pull']['bytes'] > 0 for v in d['vms'] if v['id'] == '$pulled'][0]" |
    grep -qx True || fail "the pull of $pulled did not copy every byte: $pull"
ctl delete "$pulled" > /dev/null

# --- 6. a template imported at runtime --------------------------------------------------
step 'a guest image no host was configured with is imported and booted'
image=$work/runtime.ext4
cp --sparse=always "$guest_image" "$image"
marker="sproutfs-runtime-template-$RANDOM"
printf '%s\n' "$marker" > "$work/marker"
debugfs -w -R "write $work/marker /runtime-marker" "$image" > /dev/null 2>&1 ||
    fail "writing the marker into the image failed"
digest=$(sha256sum "$image" | awk '{ print $1 }')
imported=$(kubectl exec -n "$namespace" -i deploy/sproutfs-orchestrator -- \
    sproutfsctl import-template /dev/stdin < "$image")
printf '%s\n' "$imported"
template=$(awk '{ print $1; exit }' <<< "$imported")
[[ $template == "template-$digest" ]] || fail "the template is $template, want template-$digest"
created=$(ctl create --template "$template")
printf '%s\n' "$created"
runtime_vm=$(vm_of "$created")
agent_ready "$runtime_vm" || fail "the agent in $runtime_vm never answered"
seen=$(run_in "$runtime_vm" 'cat /runtime-marker' | tr -d '\r')
[[ $seen == "$marker" ]] || fail "$runtime_vm booted an image without the marker: '$seen'"
printf '%s booted the imported image\n' "$runtime_vm"
again=$(kubectl exec -n "$namespace" -i deploy/sproutfs-orchestrator -- \
    sproutfsctl import-template /dev/stdin < "$image")
[[ $(awk '{ print $1; exit }' <<< "$again") == "$template" ]] ||
    fail "a second import of the same image named another template: $again"
printf 'a second import of the same bytes names the same template\n'
ctl delete "$runtime_vm" > /dev/null

# --- 7. tenants and billing ----------------------------------------------------------------
step "a tenant's template and VM, and what GET /stored bills"
tenant_host=$(first_host)
forward_to "$tenant_host"
# -T streams the image; --data-binary would read all of it into memory first.
reply=$(host_api POST "/templates?tenant=acme" -H 'Content-Type: application/octet-stream' \
    -T "$guest_image")
[[ $(status_of "$reply") == 200 ]] || fail "importing a template for acme answered $(body_of "$reply")"
tenant_template=$(body_of "$reply" | json "d['template']['id']")
printf "acme's template: %s\n" "$tenant_template"
[[ $tenant_template == acme/template-* ]] || fail "the tenant's template is $tenant_template"
tenant_vm="acme/vm-features-$RANDOM"
reply=$(host_api POST /vms -H 'Content-Type: application/json' \
    --data "{\"id\":\"$tenant_vm\",\"template\":\"$tenant_template\"}")
[[ $(status_of "$reply") == 200 ]] || fail "creating $tenant_vm answered $(body_of "$reply")"
escaped=${tenant_vm/\//%2F}
deadline=$(($(date +%s) + agent_timeout))
while :; do
    reply=$(host_api POST "/vms/$escaped/exec" -H 'Content-Type: application/json' \
        --data '{"cmd":"echo TENANT-OK","timeout":10}')
    [[ $(status_of "$reply") == 200 ]] && break
    (($(date +%s) < deadline)) || fail "exec in $tenant_vm answered $(body_of "$reply")"
    sleep 1
done
[[ $(body_of "$reply") == *TENANT-OK* ]] || fail "exec in $tenant_vm printed $(body_of "$reply")"
printf '%s runs on %s and answers\n' "$tenant_vm" "$tenant_host"

reply=$(host_api GET "/stored?tenant=acme")
[[ $(status_of "$reply") == 200 ]] || fail "GET /stored?tenant=acme answered $(body_of "$reply")"
printf 'stored for acme: %s\n' "$(body_of "$reply")"
billed=$(body_of "$reply" | json "d['vms'].get('$tenant_vm', 0)")
((billed > 0)) || fail "GET /stored?tenant=acme bills $tenant_vm nothing"
reply=$(host_api GET /stored)
body_of "$reply" | json "'$tenant_vm' in d['vms'] or any(k.startswith('acme/') for k in d['vms'])" |
    grep -qx False || fail "GET /stored for no tenant lists acme's VMs: $(body_of "$reply")"
body_of "$reply" | json "'$kept_vm' in d['vms']" | grep -qx True ||
    fail "GET /stored for no tenant does not list $kept_vm: $(body_of "$reply")"
printf 'acme is billed %s bytes for %s, and the VMs of no tenant are billed apart\n' "$billed" "$tenant_vm"

reply=$(host_api POST /vms -H 'Content-Type: application/json' \
    --data "{\"id\":\"beta/vm-features-$RANDOM\",\"template\":\"$tenant_template\"}")
[[ $(status_of "$reply") != 200 ]] || fail "beta created a VM from acme's template"
printf "beta is refused acme's template: %s %s\n" "$(status_of "$reply")" "$(body_of "$reply")"
reply=$(host_api POST /vms -H 'Content-Type: application/json' \
    --data "{\"id\":\"beta/vm-features-$RANDOM\",\"from\":{\"vm\":\"$tenant_vm\"}}")
[[ $(status_of "$reply") != 200 ]] || fail "beta created a VM from acme's VM"
printf "beta is refused acme's VM: %s %s\n" "$(status_of "$reply")" "$(body_of "$reply")"

reply=$(host_api DELETE "/vms/$escaped")
[[ $(status_of "$reply") == 200 ]] || fail "deleting $tenant_vm answered $(body_of "$reply")"
reply=$(host_api GET "/stored?tenant=acme")
body_of "$reply" | json "'$tenant_vm' in d['vms']" | grep -qx False ||
    fail "GET /stored?tenant=acme still bills $tenant_vm after its delete: $(body_of "$reply")"
printf 'once deleted, %s is billed nothing\n' "$tenant_vm"

ctl delete "$kept_vm" > /dev/null
ctl hosts
printf '\nEvery merged feature did what it says.\n'
