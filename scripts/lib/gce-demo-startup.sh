#!/usr/bin/env bash
# Guest-side provisioning for the demo VM, run by GCE as the startup script on
# every boot. It installs the inactivity lease first, then the HugeTLB pool,
# then k3s, so that kubelet sees the pool when it first reports node capacity.
# Every step is idempotent: a reboot re-runs this file unchanged.
set -euo pipefail

state=/var/lib/sproutfs-demo
work=/opt/sproutfs-demo
# 12 GiB of 2 MiB pages, shared by the two host pods.
hugepages=6144
k3s_version=v1.36.4+k3s1

log() { printf 'sproutfs-demo: %s\n' "$*"; logger -t sproutfs-demo -- "$*"; }

# --- inactivity lease ------------------------------------------------------
# The cloud-side --max-run-duration bounds the VM absolutely; this bounds an
# abandoned one to a day of inactivity. scripts/demo-gce.sh touches the lease
# on every command it runs against the VM.
install -d -m 0755 "$state" "$work"
[[ -e "$state/lease" ]] || touch "$state/lease"
cat > /usr/local/sbin/sproutfs-demo-expire <<'CHECK'
#!/usr/bin/env bash
set -euo pipefail
lease=${SPROUTFS_DEMO_LEASE_FILE:-/var/lib/sproutfs-demo/lease}
now=$(date +%s)
modified=0
if [[ -f "$lease" ]]; then modified=$(stat -c %Y "$lease"); fi
age=$((now - modified))
if ((age < 86400)); then
    if [[ ${1:-} == --check-only ]]; then echo "lease active: ${age}s old"; fi
    exit 0
fi
if [[ ${1:-} == --check-only ]]; then
    echo "lease expired: ${age}s old"
    exit 10
fi
logger -t sproutfs-demo '24-hour inactivity lease expired; powering off'
exec systemctl poweroff
CHECK
chmod 0755 /usr/local/sbin/sproutfs-demo-expire
cat > /etc/systemd/system/sproutfs-demo-expire.service <<'UNIT'
[Unit]
Description=Stop an abandoned sproutfs demo host
[Service]
Type=oneshot
ExecStart=/usr/local/sbin/sproutfs-demo-expire
UNIT
cat > /etc/systemd/system/sproutfs-demo-expire.timer <<'UNIT'
[Unit]
Description=Check the sproutfs demo inactivity lease
[Timer]
OnBootSec=1min
OnUnitActiveSec=1min
AccuracySec=1s
[Install]
WantedBy=timers.target
UNIT
systemctl daemon-reload
systemctl enable --now sproutfs-demo-expire.timer
log 'inactivity lease armed'

# --- nested virtualization -------------------------------------------------
[[ -c /dev/kvm ]] || { log 'FATAL: /dev/kvm is missing; nested virtualization is off'; exit 1; }
log "/dev/kvm present: $(stat -c '%A %U:%G' /dev/kvm)"

# --- HugeTLB pool ----------------------------------------------------------
# Persistent across reboots, and applied now so that kubelet advertises
# hugepages-2Mi capacity the first time it registers the node.
size=$(awk '/^Hugepagesize:/ {print $2; exit}' /proc/meminfo)
[[ "$size" == 2048 ]] || { log "FATAL: default huge page size is ${size} kB, want 2048"; exit 1; }
printf 'vm.nr_hugepages = %s\n' "$hugepages" > /etc/sysctl.d/99-sproutfs-demo-hugepages.conf
sysctl --system > /dev/null
allocated=$(< /proc/sys/vm/nr_hugepages)
[[ "$allocated" == "$hugepages" ]] || {
    log "FATAL: allocated $allocated of $hugepages huge pages"
    exit 1
}
log "HugeTLB pool: $allocated pages of ${size} kB"

# --- k3s -------------------------------------------------------------------
# Single node, its own containerd, no load balancer or ingress: the demo talks
# to pods through the API server and the node's own network. The kubeconfig is
# world readable so scripts/demo-gce.sh can run kubectl without sudo.
if ! systemctl is-enabled --quiet k3s 2>/dev/null; then
    log "installing k3s $k3s_version"
    curl -sfL https://get.k3s.io \
        | INSTALL_K3S_VERSION="$k3s_version" \
          INSTALL_K3S_EXEC='server --write-kubeconfig-mode 0644 --disable=traefik --disable=servicelb --disable=metrics-server' \
          sh -
else
    log 'k3s already installed'
    systemctl start k3s
fi
cat > /etc/profile.d/sproutfs-demo.sh <<'PROFILE'
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
PROFILE

export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
ready=false
for _ in {1..60}; do
    if kubectl wait --for=condition=Ready node --all --timeout=10s > /dev/null 2>&1; then
        ready=true
        break
    fi
done
"$ready" || { log 'FATAL: k3s node did not become Ready'; exit 1; }
log "node ready: $(kubectl get nodes --no-headers | tr -s ' ')"
log "hugepages capacity: $(kubectl get node -o jsonpath='{.items[0].status.capacity.hugepages-2Mi}')"

touch "$state/ready"
log 'provisioning complete'
