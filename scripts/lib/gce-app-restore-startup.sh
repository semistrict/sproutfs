#!/usr/bin/env bash
# Provisioning for one node of the application restore bench
# (scripts/bench-app-restore-gce.sh), run by GCE as the startup script on every
# boot: the inactivity lease, the local SSD under the kubelet, the HugeTLB pool,
# and k3s as the cluster's server or as an agent joining it. Every step is
# idempotent.
#
# The node's role, the cluster's token, the server's name and the pool's size
# are instance metadata the bench script sets: sproutfs-k3s-role (server or
# agent), sproutfs-k3s-token, sproutfs-k3s-server and sproutfs-hugepages.
# sproutfs-kubelet-disk, boot, leaves the kubelet on the boot disk of a node
# with no local SSD; without it the kubelet is on the local SSD.
set -euo pipefail

state=/var/lib/sproutfs-bench
k3s_version=v1.36.4+k3s1

log() { printf 'sproutfs-bench: %s\n' "$*"; logger -t sproutfs-bench -- "$*"; }
metadata() {
    curl -fsS -H 'Metadata-Flavor: Google' \
        "http://metadata.google.internal/computeMetadata/v1/instance/attributes/$1"
}

# --- inactivity lease ------------------------------------------------------
# The cloud-side --max-run-duration bounds the node absolutely; this bounds an
# abandoned one to a day without a command from the bench script.
install -d -m 0755 "$state"
[[ -e "$state/lease" ]] || touch "$state/lease"
cat > /usr/local/sbin/sproutfs-bench-expire <<'CHECK'
#!/usr/bin/env bash
set -euo pipefail
lease=/var/lib/sproutfs-bench/lease
age=$(($(date +%s) - $(stat -c %Y "$lease" 2> /dev/null || echo 0)))
((age < 86400)) && exit 0
logger -t sproutfs-bench '24-hour inactivity lease expired; powering off'
exec systemctl poweroff
CHECK
chmod 0755 /usr/local/sbin/sproutfs-bench-expire
cat > /etc/systemd/system/sproutfs-bench-expire.service <<'UNIT'
[Unit]
Description=Stop an abandoned application restore bench node
[Service]
Type=oneshot
ExecStart=/usr/local/sbin/sproutfs-bench-expire
UNIT
cat > /etc/systemd/system/sproutfs-bench-expire.timer <<'UNIT'
[Unit]
Description=Check the application restore bench's inactivity lease
[Timer]
OnBootSec=1min
OnUnitActiveSec=1min
[Install]
WantedBy=timers.target
UNIT
systemctl daemon-reload
systemctl enable --now sproutfs-bench-expire.timer

[[ -c /dev/kvm ]] || { log 'FATAL: /dev/kvm is missing; nested virtualization is off'; exit 1; }

# --- the local SSD ---------------------------------------------------------
# The kubelet's directory is on the SSD, so a host pod's scratch, an emptyDir,
# and its page cache's file, a hostPath beside it, are on one filesystem: the
# host's one disk limiter measures one filesystem, and the cache is what a
# host's disk is for.
ssd=/dev/disk/by-id/google-local-nvme-ssd-0
if [[ $(metadata sproutfs-kubelet-disk 2> /dev/null || true) == boot ]]; then
    log 'the kubelet directory stays on the boot disk'
elif ! mountpoint -q /var/lib/kubelet; then
    [[ -b $ssd ]] || { log "FATAL: no local SSD at $ssd"; exit 1; }
    blkid "$ssd" > /dev/null 2>&1 || mkfs.ext4 -q -F "$ssd"
    install -d -m 0755 /var/lib/kubelet
    mount -o discard,defaults "$ssd" /var/lib/kubelet
fi
log "kubelet directory: $(findmnt -no SOURCE,SIZE /var/lib/kubelet | tr -s ' ')"

# --- HugeTLB pool ----------------------------------------------------------
hugepages=$(metadata sproutfs-hugepages)
printf 'vm.nr_hugepages = %s\n' "$hugepages" > /etc/sysctl.d/99-sproutfs-bench-hugepages.conf
sysctl --system > /dev/null
allocated=$(< /proc/sys/vm/nr_hugepages)
[[ $allocated == "$hugepages" ]] || { log "FATAL: allocated $allocated of $hugepages huge pages"; exit 1; }
log "HugeTLB pool: $allocated pages of 2 MiB"

# --- k3s -------------------------------------------------------------------
role=$(metadata sproutfs-k3s-role)
token=$(metadata sproutfs-k3s-token)
# The node is named by its short host name. GCE's full name carries the
# project's internal domain, and k3s puts the node's name in a label, which
# holds at most 63 characters.
node=$(hostname -s)
case $role in
server)
    if ! systemctl is-enabled --quiet k3s 2> /dev/null; then
        log "installing the k3s $k3s_version server"
        curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION="$k3s_version" K3S_TOKEN="$token" \
            INSTALL_K3S_EXEC="server --node-name $node --tls-san $node --write-kubeconfig-mode 0644 --disable=traefik --disable=servicelb --disable=metrics-server" \
            sh -
    else
        systemctl start k3s
    fi
    export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
    # A wait for a node that has not registered yet fails at once rather
    # than waiting, so the loop paces itself.
    for _ in {1..60}; do
        if kubectl wait --for=condition=Ready "node/$node" --timeout=10s > /dev/null 2>&1; then
            touch "$state/ready"
            log 'the server is ready'
            exit 0
        fi
        sleep 5
    done
    log 'FATAL: the k3s server did not become ready'
    exit 1
    ;;
agent)
    server=$(metadata sproutfs-k3s-server)
    if ! systemctl is-enabled --quiet k3s-agent 2> /dev/null; then
        log "installing the k3s $k3s_version agent of $server"
        curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION="$k3s_version" K3S_TOKEN="$token" \
            K3S_URL="https://$server:6443" INSTALL_K3S_EXEC="agent --node-name $node" sh -
    else
        systemctl start k3s-agent
    fi
    touch "$state/ready"
    log 'the agent is installed'
    ;;
*)
    log "FATAL: sproutfs-k3s-role is '$role', want server or agent"
    exit 1
    ;;
esac
