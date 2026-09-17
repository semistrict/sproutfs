#!/usr/bin/env bash
# Install the inactivity lease before provisioning or running any benchmark.
set -euo pipefail

install -d -m 0755 /var/lib/sproutfs-bench
if [[ ! -e /var/lib/sproutfs-bench/lease ]]; then
    touch /var/lib/sproutfs-bench/lease
fi
cat > /usr/local/sbin/sproutfs-bench-expire <<'CHECK'
#!/usr/bin/env bash
set -euo pipefail
lease=${SPROUTFS_BENCH_LEASE_FILE:-/var/lib/sproutfs-bench/lease}
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
logger -t sproutfs-bench '24-hour inactivity lease expired; powering off'
exec systemctl poweroff
CHECK
chmod 0755 /usr/local/sbin/sproutfs-bench-expire
cat > /etc/systemd/system/sproutfs-bench-expire.service <<'UNIT'
[Unit]
Description=Stop an abandoned memory benchmark host
[Service]
Type=oneshot
ExecStart=/usr/local/sbin/sproutfs-bench-expire
UNIT
cat > /etc/systemd/system/sproutfs-bench-expire.timer <<'UNIT'
[Unit]
Description=Check the memory benchmark inactivity lease
[Timer]
OnBootSec=1min
OnUnitActiveSec=1min
AccuracySec=1s
[Install]
WantedBy=timers.target
UNIT
systemctl daemon-reload
systemctl enable --now sproutfs-bench-expire.timer
touch /var/lib/sproutfs-bench/ready
