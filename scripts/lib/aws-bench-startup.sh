#!/usr/bin/env bash
# User data of a disposable EC2 benchmark host (scripts/bench-hot-tier-aws.sh).
# The host powers off three hours after it boots, and was launched to
# terminate when it does, which deletes its volumes too. It then installs fio
# and says it is ready.
set -euo pipefail
shutdown -h +180 'sproutfs-bench: three hours are up'
install -d -m 0755 /var/lib/sproutfs-bench /var/tmp/sproutfs
dnf install -y -q fio
touch /var/lib/sproutfs-bench/ready
